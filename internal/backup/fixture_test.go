package backup_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// fixture is a central data directory with database rows and payload files of every kind.
type fixture struct {
	t       *testing.T
	ctx     context.Context
	st      *store.Store
	kek     *keyring.LocalKEK
	kr      *keyring.Keyring
	aw      *audit.Writer
	ch      *changefeed.Recorder
	git     *memory.Git
	spaceID string
	nodeID  string
	runID   string
	// files maps a payload path (as in the backup) to its content.
	files map[string][]byte
	// secret is a marker that must never appear in a backup in plaintext.
	secret string
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newKEK(t testing.TB) *keyring.LocalKEK {
	t.Helper()
	k, err := keyring.GenerateKEK()
	must(t, err)
	w, err := keyring.NewLocalKEK(k)
	must(t, err)
	return w
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), files: map[string][]byte{}, secret: "plaintext-marker-" + rand.Text()}
	f.st = storetest.Open(t)
	f.kek = newKEK(t)
	var err error
	f.kr, err = keyring.Load(f.ctx, f.st, f.kek)
	must(t, err)
	f.aw = audit.NewWriter(f.kr)
	f.ch = changefeed.New(nil)
	if g, err := memory.NewGit(); err == nil {
		f.git = g
	}
	fl := fleet.New(f.st, f.kr, f.aw, eventbus.New(), f.ch, fleet.Options{})
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, err := fl.CreateNode(f.ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	must(t, err)
	f.nodeID = en.Node.ID
	sp, err := fl.CreateSpace(f.ctx, actor, fleet.CreateSpaceInput{Slug: "billing", Name: "Billing", NodeID: f.nodeID})
	must(t, err)
	f.spaceID = sp.ID
	f.runID = "run_" + "0000000000000000000000000r"
	now := f.st.Now().UnixMicro()
	must(t, f.st.W().TxNamed(f.ctx, "fixture", func(tx *store.Tx) error {
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO execution_leases (space_id, node_id, generation, state, granted_at, renewed_at, expires_at) VALUES (?, ?, 3, 'active', ?, ?, ?)`,
				[]any{f.spaceID, f.nodeID, now, now, now + 60e6}},
			{`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
				VALUES (?, ?, ?, 3, 'worker', 'implementor', 'claude-code', 'ins_1', 'running', 'running', '{}', ?, 'usr_test', ?)`,
				[]any{f.runID, f.spaceID, f.nodeID, now, now}},
			{`INSERT INTO node_commands (id, node_id, space_id, run_id, generation, kind, idempotency_key, state, created_at) VALUES ('cmd_1', ?, ?, ?, 3, 'stop', 'k1', 'sent', ?)`,
				[]any{f.nodeID, f.spaceID, f.runID, now}},
			{`INSERT INTO run_credentials (mac, run_id, created_at) VALUES (x'01', ?, ?)`, []any{f.runID, now}},
			{`INSERT INTO approvals (id, space_id, policy_id, operation, resource, args_digest, args_preview, requester_run, requester_role, state,
				uses_allowed, uses_left, valid_seconds, valid_until, request_expires_at, created_at, updated_at)
				VALUES ('apv_used', ?, 'pol', 'mcp.x', '', 'd1', '{}', ?, 'implementor', 'approved', 2, 1, 3600, ?, ?, ?, ?)`,
				[]any{f.spaceID, f.runID, now + 3600e6, now + 3600e6, now, now}},
			{`INSERT INTO approvals (id, space_id, policy_id, operation, resource, args_digest, args_preview, requester_run, requester_role, state,
				uses_allowed, uses_left, valid_seconds, request_expires_at, created_at, updated_at)
				VALUES ('apv_wait', ?, 'pol', 'mcp.y', '', 'd2', '{}', ?, 'implementor', 'pending', 1, 1, 3600, ?, ?, ?)`,
				[]any{f.spaceID, f.runID, now + 3600e6, now, now}},
		} {
			if _, err := tx.SQL.ExecContext(f.ctx, q.sql, q.args...); err != nil {
				return err
			}
		}
		_, err := f.aw.Append(f.ctx, tx, audit.Event{Actor: actor, Action: "fixture.ready"})
		return err
	}))

	content := func(label string) []byte { return []byte(label + " " + f.secret + " " + rand.Text()) }
	f.write("recordings/"+f.runID+"/seg-000001.smrec.zst", content("recording"))
	blob := content("attachment")
	sum := sha256.Sum256(blob)
	h := hex.EncodeToString(sum[:])
	f.write("artifacts/sha256/"+h[:2]+"/"+h, blob)
	f.write("transcripts/"+f.runID+".jsonl", content("transcript"))
	f.write("transcripts/"+f.runID+"x.jsonl.part", content("partial transcript"))
	// An unfinished upload is not part of a backup.
	must(t, os.MkdirAll(f.st.Path("artifacts", "tmp"), 0o700))
	must(t, os.WriteFile(f.st.Path("artifacts", "tmp", "upload-1"), []byte("partial upload"), 0o600))
	if f.git != nil {
		f.memoryRepo()
	}
	return f
}

func (f *fixture) write(rel string, b []byte) {
	f.t.Helper()
	p := filepath.Join(f.st.Dir(), filepath.FromSlash(rel))
	must(f.t, os.MkdirAll(filepath.Dir(p), 0o700))
	must(f.t, os.WriteFile(p, b, 0o600))
	f.files[rel] = b
}

// memoryRepo creates the central bare repository of one role with one commit and a preserved conflict ref.
func (f *fixture) memoryRepo() {
	f.t.Helper()
	ctx := f.ctx
	bare := f.st.Path("memory", f.spaceID, "implementor.git")
	must(f.t, f.git.InitBare(ctx, bare))
	work := filepath.Join(f.t.TempDir(), "work")
	must(f.t, os.MkdirAll(work, 0o700))
	id := &memory.Identity{Name: "test", Email: "test@example.invalid"}
	_, err := f.git.Run(ctx, work, id, "init", "--quiet")
	must(f.t, err)
	must(f.t, os.WriteFile(filepath.Join(work, "decisions.md"), []byte("- "+f.secret+"\n"), 0o600))
	for _, args := range [][]string{{"add", "decisions.md"}, {"commit", "--quiet", "-m", "remember"}, {"push", "--quiet", bare, "HEAD:refs/heads/main"},
		{"push", "--quiet", bare, "HEAD:refs/conflicts/node-1"}} {
		if _, err := f.git.Run(ctx, work, id, args...); err != nil {
			f.t.Fatal(err)
		}
	}
	// An empty repository is skipped.
	must(f.t, f.git.InitBare(ctx, f.st.Path("memory", f.spaceID, "reviewer.git")))
}

func (f *fixture) sources() backup.Sources {
	return backup.DefaultSources(f.st.Dir(), "", f.git)
}

func (f *fixture) source() *backup.ReadOnlySource {
	f.t.Helper()
	src, err := backup.OpenReadOnly(f.ctx, f.st.Dir())
	must(f.t, err)
	f.t.Cleanup(func() { _ = src.Close() })
	return src
}

func (f *fixture) create(dest string) *backup.Result {
	f.t.Helper()
	res, err := backup.Create(f.ctx, f.source(), f.kek, dest, backup.Options{Sources: f.sources(), Config: []byte("mode: development\n")})
	must(f.t, err)
	return res
}

func gitHead(t *testing.T, repo, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", repo, "rev-parse", "--verify", "--quiet", ref).Output()
	if err != nil {
		return ""
	}
	return string(out)
}
