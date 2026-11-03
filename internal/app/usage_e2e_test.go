package app_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// SM-REC-005, ADR-0028 end to end: a Claude Code run's usage is read from its native transcript (where Claude Code
// writes it) and its transcript is uploaded after the run ends; a Codex run's usage stays unknown, not zero.
// SM-REC-001: the run's recording, transcript and usage are kept apart.
func TestUsageAndTranscript(t *testing.T) {
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "usage", Name: "Usage", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	cl, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	cx, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "reviewer", Harness: hns.KindCodex})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "runs running", func() bool {
		return e.run1(cl.ID).Observed == "running" && e.run1(cx.ID).Observed == "running" && e.run1(cl.ID).SessionID != ""
	})
	r := e.run1(cl.ID)
	e.run.Output(e.run.LiveByLabel("spaceman.run", cl.ID)[0].ID, "terminal output only\r\n")
	transcript := []byte(`{"type":"assistant","message":{"id":"msg_1","model":"claude-haiku-5-5","usage":{"input_tokens":3,"output_tokens":7,"cache_creation_input_tokens":11,"cache_read_input_tokens":13}}}` + "\n" +
		`{"type":"cost-state","totalCostUSD":0.002,"hasUnknownModelCost":false}` + "\n")
	p := filepath.Join(e.agentDir, "spaces", sp.Workdir, "instances", r.InstanceID, "claude", "projects", "-work", r.SessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{cl.ID, cx.ID} {
		if _, err := e.c.Fleet.StopRun(ctx, admin, id, "kill", "done"); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "usage and transcript stored", func() bool {
		u, err := e.c.Usage.ForRun(ctx, sp.ID, cl.ID)
		return err == nil && u.Known && u.Final && u.Transcript != nil && u.Transcript.State == "stored"
	})
	u, _ := e.c.Usage.ForRun(ctx, sp.ID, cl.ID)
	if len(u.Models) != 1 || *u.Models[0].Output != 7 || u.Source != proto.UsageClaudeTranscript || u.Estimate == nil || u.Estimate.USD != 0.002 {
		t.Fatalf("claude usage: %+v", u)
	}
	f, _, err := e.c.Usage.OpenTranscript(ctx, sp.ID, cl.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	_ = f.Close()
	if string(got) != string(transcript) {
		t.Fatalf("stored transcript: %q", got)
	}
	eventually(t, "codex usage reported as unknown", func() bool {
		u, err := e.c.Usage.ForRun(ctx, sp.ID, cx.ID)
		return err == nil && u.Final && !u.Known && u.Note != ""
	})
	s, err := e.c.Usage.Space(ctx, sp.ID)
	if err != nil || s.KnownRuns != 1 || s.UnknownRuns != 1 || s.Output != 7 {
		t.Fatalf("summary: %+v %v", s, err)
	}

	// SM-REC-001: the terminal recording, the native transcript and the normalized usage are separate records. The
	// recording holds terminal output and not the transcript; deleting either artifact leaves the others as they were.
	recorded := func() string {
		res, err := e.c.Fleet.Recordings().Read(cl.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, rec := range res.Records {
			b.Write(rec.Data)
		}
		return b.String()
	}
	eventually(t, "terminal output recorded", func() bool { return strings.Contains(recorded(), "terminal output only") })
	if strings.Contains(recorded(), "cache_creation_input_tokens") {
		t.Fatal("the transcript was written into the terminal recording")
	}
	if err := e.c.Usage.DeleteTranscript(ctx, admin, sp.ID, cl.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorded(), "terminal output only") {
		t.Fatal("deleting the transcript removed the recording")
	}
	if u, err := e.c.Usage.ForRun(ctx, sp.ID, cl.ID); err != nil || !u.Known || *u.Models[0].Output != 7 || u.Transcript == nil || u.Transcript.State != "deleted" {
		t.Fatalf("usage after the transcript was deleted: %+v %v", u, err)
	}
	if err := e.c.Fleet.Recordings().Delete(cl.ID); err != nil {
		t.Fatal(err)
	}
	if recorded() != "" {
		t.Fatal("recording not deleted")
	}
	if u, err := e.c.Usage.ForRun(ctx, sp.ID, cl.ID); err != nil || !u.Known || *u.Models[0].Output != 7 {
		t.Fatalf("usage after the recording was deleted: %+v %v", u, err)
	}
}
