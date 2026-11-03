//go:build fault

package fault

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the external connection that holds the write lock

	"github.com/aurimasniekis/go-spaceman/internal/agent"
	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/app/apptest"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// Fault: corrupted or partial files on the node and a corrupted central database are refused with a clear error or
// recovered explicitly — never read as a silent wrong state.
func TestFaultCorruptNodeState(t *testing.T) {
	ctx := context.Background()

	t.Run("journal corrupted", func(t *testing.T) {
		e := apptest.New(t)
		e.Enroll()
		e.StartAgent()
		e.Connected()
		sp := e.Space("corrupt", fleet.CreateSpaceInput{})
		r := e.Launch(sp.ID, "implementor", hns.KindClaude)
		e.StopAgent()
		db := filepath.Join(e.AgentDir, "journal", store.DBFile)
		b, err := os.ReadFile(db)
		if err != nil {
			t.Fatal(err)
		}
		copy(b[:100], bytes.Repeat([]byte{0xA5}, 100)) // the SQLite header is gone
		if err := os.WriteFile(db, b, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = agent.OpenJournal(ctx, filepath.Join(e.AgentDir, "journal"), apptest.Log(), nil)
		if !errors.Is(err, store.ErrCorrupt) {
			t.Fatalf("corrupted journal opened: %v, want ErrCorrupt", err)
		}
		if !strings.Contains(err.Error(), "restore") {
			t.Fatalf("error does not say what to do: %v", err)
		}
		// The agent refuses to run without its journal; the program it was supervising is left alone, and central
		// does not claim it is still running.
		if n := len(e.Runner.LiveByLabel("spaceman.run", r.ID)); n != 1 {
			t.Fatalf("live programs %d", n)
		}
		eventually(t, "run unknown without the agent", func() bool { return e.Run(r.ID).Observed == "unknown" })
	})

	t.Run("journal truncated", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "journal")
		j, err := agent.OpenJournal(ctx, dir, apptest.Log(), nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = j.Close()
		db := filepath.Join(dir, store.DBFile)
		fi, _ := os.Stat(db)
		if err := os.Truncate(db, fi.Size()/2+1); err != nil {
			t.Fatal(err)
		}
		_, err = agent.OpenJournal(ctx, dir, apptest.Log(), nil)
		if !errors.Is(err, store.ErrCorrupt) {
			t.Fatalf("truncated journal: %v, want ErrCorrupt", err)
		}
	})

	t.Run("partial lease file", func(t *testing.T) {
		// The lease guard ends the space's runs at once when its file is unreadable (fail closed).
		lf := filepath.Join(t.TempDir(), "spc_x.json")
		if err := os.WriteFile(lf, []byte(`{"space_id":"spc_x","generation":3,"valid_un`), 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- agent.LeaseGuard(ctx, lf, time.Now) }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "unreadable") {
				t.Fatalf("lease guard on a partial file: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("lease guard kept the runs going on a partial lease file")
		}
	})

	t.Run("partial secrets file", func(t *testing.T) {
		sf := filepath.Join(t.TempDir(), agent.SecretsFile)
		if err := os.WriteFile(sf, []byte(`{"CLAUDE_CODE_OAUTH_TOKEN":"tok`), 0o600); err != nil {
			t.Fatal(err)
		}
		// The shim never starts the harness with a half-read environment.
		err := agent.LaunchShim(sf, []string{"/usr/bin/true"})
		if err == nil || !strings.Contains(err.Error(), "secrets file") {
			t.Fatalf("launch with a partial secrets file: %v", err)
		}
		if _, err := os.Stat(sf); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the partial secrets file was left on disk")
		}
	})

	t.Run("partial harness config", func(t *testing.T) {
		cfg := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(cfg, []byte(`{"permissions": {"allow": [`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := agent.MergeJSONFile(cfg, map[string]any{"x": 1}); err == nil {
			t.Fatal("a partial config was overwritten silently")
		}
		if b, _ := os.ReadFile(cfg); string(b) != `{"permissions": {"allow": [` {
			t.Fatal("a partial config was changed")
		}
	})

	t.Run("garbage recording segment", func(t *testing.T) {
		dir := t.TempDir()
		w, err := recording.Open(recording.Options{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Add(recording.Record{Kind: recording.KindOut, Data: []byte("kept")}); err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		if err := os.WriteFile(filepath.Join(dir, recording.SegmentName(2)), []byte("not zstd at all"), 0o600); err != nil {
			t.Fatal(err)
		}
		res, err := recording.Read(dir, 0)
		if err != nil || !res.Truncated || len(res.Records) != 1 || string(res.Records[0].Data) != "kept" {
			t.Fatalf("garbage segment: %+v %v", res, err)
		}
		// A writer resuming the recording continues after the readable records, in a new segment.
		w2, err := recording.Open(recording.Options{Dir: dir})
		if err != nil || w2.Seq() != 1 {
			t.Fatalf("resume over garbage: seq %d %v", w2.Seq(), err)
		}
		_ = w2.Close()
	})

	t.Run("central database corrupted", func(t *testing.T) {
		e := apptest.New(t)
		_ = e.C.Close()
		e.C = nil
		db := filepath.Join(e.Cfg.DataDir, store.DBFile)
		b, err := os.ReadFile(db)
		if err != nil {
			t.Fatal(err)
		}
		copy(b[:100], bytes.Repeat([]byte{0x5A}, 100))
		if err := os.WriteFile(db, b, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = app.OpenCentral(ctx, e.Cfg, apptest.Log(), app.Options{Environ: []string{}})
		if !errors.Is(err, store.ErrCorrupt) || !strings.Contains(err.Error(), "backup") {
			t.Fatalf("corrupted central database: %v", err)
		}
	})
}

// Fault: the database stays locked longer than the store's bounded retries (another process holds the write lock).
// The API answers 503 "unavailable" with Retry-After and writes nothing; once the lock is released, retrying with the
// same Idempotency-Key creates the space exactly once.
func TestFaultDatabaseBusyIsRetryable(t *testing.T) {
	e := apptest.New(t)
	e.CentralOptions.StoreBusyTimeout = 20 * time.Millisecond
	e.RestartCentral()
	e.Enroll()
	root := e.RootToken()
	post := func() (*http.Response, []byte) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"slug": "busy", "name": "Busy", "node_id": e.NodeID})
		req, _ := http.NewRequest(http.MethodPost, e.Srv.URL+api.Prefix+"/spaces", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+root)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "fault-busy-0001")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, b
	}

	other, err := sql.Open("sqlite", "file:"+filepath.Join(e.Cfg.DataDir, store.DBFile)+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := other.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	res, b := post()
	if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Retry-After") == "" || !strings.Contains(string(b), `"unavailable"`) {
		t.Fatalf("while locked: %d %q %s", res.StatusCode, res.Header.Get("Retry-After"), b)
	}
	_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	_ = conn.Close()

	res, b = post()
	if res.StatusCode/100 != 2 {
		t.Fatalf("retry after the lock: %d %s", res.StatusCode, b)
	}
	var first struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(b, &first)
	res, b = post()
	var again struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(b, &again)
	if res.StatusCode/100 != 2 || again.ID != first.ID || first.ID == "" {
		t.Fatalf("replay %d %s, first %s", res.StatusCode, b, first.ID)
	}
	spaces, err := e.C.Fleet.ListSpaces(context.Background(), func(string) bool { return true })
	if err != nil || len(spaces) != 1 {
		t.Fatalf("spaces after busy and retries: %d %v", len(spaces), err)
	}
}
