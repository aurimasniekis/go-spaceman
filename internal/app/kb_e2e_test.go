package app_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/kb"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// SM-KB-001…006, SM-CTRL-005, ADR-0026 end to end with a real agent, central and Git repository: each requesting
// instance gets its own KB session (a kb run in its own worktree); requests queue at capacity; answers carry evidence
// and reach the requester; a proposal is accepted on the node by fast-forward after the KB's check, a conflicting one
// is refused without changing anything, and read-only KBs take no proposals.
func TestKBSupportSessions(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "knowledge", Name: "Knowledge", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	repo := filepath.Join(e.agentDir, "spaces", sp.Workdir, "repos", "kb")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	// A bare-like layout: main is not checked out anywhere, so acceptance may move it.
	gitIn(t, repo, "init", "-q", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# KB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "init")
	gitIn(t, repo, "branch", "main")

	// The KB sessions' role: a template of kind kb.
	prof, err := e.c.Catalog.SaveProfile(ctx, admin, "", 0, catalog.Profile{Slug: "kb-claude", Name: "KB", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := e.c.Catalog.SaveTemplate(ctx, admin, "", 0, catalog.RoleTemplate{Slug: "kb-keeper", Name: "KB keeper", Kind: "kb", ProfileID: prof.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Catalog.SaveSpaceRole(ctx, admin, sp.ID, catalog.SpaceRole{Name: "kb-support", TemplateID: tpl.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.KB.Create(ctx, admin, sp.ID, kb.Input{Slug: "payments", Name: "Payments KB", Repo: "repos/kb", BaseBranch: "main", SpaceRole: "nope"}); err == nil {
		t.Fatal("a KB with an unknown space role was accepted")
	}
	k, err := e.c.KB.Create(ctx, admin, sp.ID, kb.Input{Slug: "payments", Name: "Payments KB", Repo: "repos/kb", BaseBranch: "main", SpaceRole: "kb-support",
		WritePolicy: kb.Review, AllowedRoles: []string{"implementor"}, MaxSessions: 1, Validator: []string{"sh", "-c", "test -f README.md"}})
	if err != nil {
		t.Fatal(err)
	}

	launch := func(role string) (*fleet.Run, string) {
		t.Helper()
		r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: role, Harness: hns.KindClaude})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, role+" running", func() bool { return e.run1(r.ID).Observed == "running" })
		return r, e.runToken(r.ID)
	}
	a, at := launch("implementor")
	b, bt := launch("implementor")
	other, ot := launch("reviewer")

	if r := e.bridgeCall(other.ID, ot, "spaceman_kb_ask", map[string]any{"kb": "payments", "kind": "context", "question": "x"}); r.OK || r.Code != "forbidden" {
		t.Fatalf("unadmitted role: %+v", r)
	}
	ask := func(run, tok, q string) string {
		t.Helper()
		r := e.bridgeCall(run, tok, "spaceman_kb_ask", map[string]any{"kb": "payments", "kind": "decision", "question": q, "task_id": "tsk_x"})
		if !r.OK {
			t.Fatalf("ask: %+v", r)
		}
		var req kb.Request
		_ = json.Unmarshal(r.Result, &req)
		return req.ID
	}
	qa := ask(a.ID, at, "Which retry budget did we decide on?")
	qb := ask(b.ID, bt, "Where are settlement decisions recorded?")
	e.c.KB.Step(ctx)
	d, err := e.c.KB.Detail(ctx, sp.ID, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Sessions) != 2 || d.KB.Stats.ActiveSessions != 1 {
		t.Fatalf("sessions: %+v stats %+v", d.Sessions, d.KB.Stats)
	}
	var sessA kb.Session
	for _, s := range d.Sessions {
		if s.State == "active" {
			sessA = s
		}
	}
	if sessA.RunID == "" || !strings.HasPrefix(sessA.Branch, "kb/payments/") {
		t.Fatalf("active session: %+v", sessA)
	}
	kbRun := e.run1(sessA.RunID)
	if kbRun.Kind != "kb" {
		t.Fatalf("session run kind %q", kbRun.Kind)
	}
	eventually(t, "KB run running", func() bool { return e.run1(sessA.RunID).Observed == "running" })
	kt := e.runToken(sessA.RunID)
	served := qa
	if r, _ := e.c.KB.GetRequest(ctx, &fleet.RunPrincipal{SpaceID: sp.ID, InstanceID: b.InstanceID, RunID: b.ID}, qb); r.SessionID == sessA.ID {
		served = qb
	}
	// The other requester's question waits: capacity is one session.
	waiting := map[string]string{qa: qb, qb: qa}[served]
	if r, _ := e.c.KB.GetRequest(ctx, &fleet.RunPrincipal{SpaceID: sp.ID, InstanceID: map[string]string{qa: a.InstanceID, qb: b.InstanceID}[waiting]}, waiting); r.State != "queued" {
		t.Fatalf("second request at capacity: %+v", r)
	}
	// KB tools are only for KB sessions; KB sessions cannot orchestrate.
	if r := e.bridgeCall(a.ID, at, "spaceman_kb_answer", map[string]any{"request_id": served, "answer": "x", "basis": "none"}); r.OK {
		t.Fatalf("a worker answered: %+v", r)
	}
	if r := e.bridgeCall(sessA.RunID, kt, "spaceman_worker_launch", map[string]any{"task_id": "tsk_x", "role": "implementor"}); r.OK || r.Code != "forbidden" {
		t.Fatalf("a KB session launched a worker: %+v", r)
	}
	head := gitIn(t, repo, "rev-parse", "main")
	r := e.bridgeCall(sessA.RunID, kt, "spaceman_kb_answer", map[string]any{"request_id": served, "answer": "A budget of 5 per batch.", "basis": "committed",
		"evidence": []map[string]string{{"path": "decisions/ADR-0002.md", "revision": head}}})
	if !r.OK {
		t.Fatalf("answer: %+v", r)
	}
	requester := map[string]*fleet.Run{qa: a, qb: b}[served]
	reqTok := map[string]string{qa: at, qb: bt}[served]
	got := e.bridgeCall(requester.ID, reqTok, "spaceman_kb_get", map[string]any{"request_id": served})
	if !got.OK || !strings.Contains(string(got.Result), "5 per batch") || !strings.Contains(string(got.Result), head) {
		t.Fatalf("requester read: %+v", got)
	}
	msgs, err := e.c.Messages.List(ctx, sp.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	notified := false
	for _, m := range msgs {
		notified = notified || (m.ToID == requester.ID && m.CorrelationID == served)
	}
	if !notified {
		t.Fatal("the requester was not notified")
	}

	// A proposal from the session's worktree is accepted on the node by fast-forward after the check.
	wt := filepath.Join(e.agentDir, "spaces", sp.Workdir, "worktrees", strings.ReplaceAll(sessA.Branch, "/", "-"))
	if err := os.WriteFile(filepath.Join(wt, "decisions.md"), []byte("- retries: 5 per batch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "Record the retry budget")
	c1 := gitIn(t, wt, "rev-parse", "HEAD")
	pr := e.bridgeCall(sessA.RunID, kt, "spaceman_kb_propose", map[string]any{"title": "Retry budget", "commit": c1, "request_id": served})
	if !pr.OK {
		t.Fatalf("propose: %+v", pr)
	}
	var prop kb.Proposal
	_ = json.Unmarshal(pr.Result, &prop)
	if _, err := e.c.KB.Accept(ctx, admin, sp.ID, prop.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "proposal accepted", func() bool {
		d, _ := e.c.KB.Detail(ctx, sp.ID, k.ID)
		for _, p := range d.Proposals {
			if p.ID == prop.ID {
				return p.State == "accepted" && p.Merged == c1
			}
		}
		return false
	})
	if gitIn(t, repo, "rev-parse", "main") != c1 {
		t.Fatal("main did not move to the accepted commit")
	}

	// A conflicting proposal is refused and nothing moves.
	gitIn(t, repo, "checkout", "-q", "-b", "maint", "main")
	if err := os.WriteFile(filepath.Join(repo, "decisions.md"), []byte("- retries: 3 per item\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "commit", "-q", "-am", "maintainer edit")
	gitIn(t, repo, "branch", "-f", "main", "maint")
	gitIn(t, repo, "checkout", "-q", "trunk")
	mainBefore := gitIn(t, repo, "rev-parse", "main")
	if err := os.WriteFile(filepath.Join(wt, "decisions.md"), []byte("- retries: 7 per batch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "commit", "-q", "-am", "Change the budget")
	c2 := gitIn(t, wt, "rev-parse", "HEAD")
	pr = e.bridgeCall(sessA.RunID, kt, "spaceman_kb_propose", map[string]any{"title": "Budget 7", "commit": c2})
	_ = json.Unmarshal(pr.Result, &prop)
	if _, err := e.c.KB.Accept(ctx, admin, sp.ID, prop.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "conflict reported", func() bool {
		d, _ := e.c.KB.Detail(ctx, sp.ID, k.ID)
		for _, p := range d.Proposals {
			if p.ID == prop.ID {
				return p.State == "conflict" && strings.Contains(p.Detail, "decisions.md")
			}
		}
		return false
	})
	if gitIn(t, repo, "rev-parse", "main") != mainBefore {
		t.Fatal("a conflicting proposal moved main")
	}
	if _, err := e.c.KB.Reject(ctx, admin, sp.ID, prop.ID, "superseded"); err != nil {
		t.Fatal(err)
	}

	// Idle sleep frees the slot: the waiting requester's session starts and gets its question.
	if err := e.c.Store.W().TxNamed(ctx, "test.backdate", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE kb_sessions SET last_active_at = 0 WHERE id = ?`, sessA.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.c.KB.Step(ctx)
	eventually(t, "idle session stopped", func() bool { o := e.run1(sessA.RunID).Observed; return o == "exited" || o == "failed" })
	e.c.KB.Step(ctx)
	eventually(t, "waiting request dispatched", func() bool {
		d, _ := e.c.KB.Detail(ctx, sp.ID, k.ID)
		for _, r := range d.Requests {
			if r.ID == waiting {
				return r.State == "dispatched"
			}
		}
		return false
	})

	// Recovery: the session's run dies before answering; the session is marked failed, restarted after its backoff,
	// and the question is offered again.
	d, _ = e.c.KB.Detail(ctx, sp.ID, k.ID)
	var sessB kb.Session
	for _, s := range d.Sessions {
		if s.State == "active" {
			sessB = s
		}
	}
	eventually(t, "second session running", func() bool { return e.run1(sessB.RunID).Observed == "running" })
	if _, err := e.c.Fleet.StopRun(ctx, admin, sessB.RunID, "kill", "crash"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "session failed", func() bool {
		d, _ := e.c.KB.Detail(ctx, sp.ID, k.ID)
		for _, s := range d.Sessions {
			if s.ID == sessB.ID {
				return s.State == "failed" && s.Failures == 1
			}
		}
		return false
	})
	if err := e.c.Store.W().TxNamed(ctx, "test.backdate", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE kb_sessions SET updated_at = 0 WHERE id = ?`, sessB.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.c.KB.Step(ctx)
	eventually(t, "request offered again", func() bool {
		d, _ := e.c.KB.Detail(ctx, sp.ID, k.ID)
		for _, r := range d.Requests {
			if r.ID == waiting {
				return r.State == "dispatched" && r.Attempts == 2
			}
		}
		return false
	})
	// The requester cancels it.
	waiter := map[string]*fleet.Run{qa: a, qb: b}[waiting]
	if r := e.bridgeCall(waiter.ID, map[string]string{qa: at, qb: bt}[waiting], "spaceman_kb_cancel", map[string]any{"request_id": waiting}); !r.OK {
		t.Fatalf("cancel: %+v", r)
	}

	// Read-only KBs take no proposals.
	cur, _ := e.c.KB.Get(ctx, sp.ID, k.ID)
	if _, err := e.c.KB.Update(ctx, admin, sp.ID, k.ID, cur.Version, kb.Input{Name: cur.Name, Repo: cur.Repo, BaseBranch: "main", SpaceRole: "kb-support",
		WritePolicy: kb.ReadOnly, AllowedRoles: cur.AllowedRoles, MaxSessions: 1}); err != nil {
		t.Fatal(err)
	}
	d, _ = e.c.KB.Detail(ctx, sp.ID, k.ID)
	for _, s := range d.Sessions {
		if s.State == "active" && s.RunID != "" {
			eventually(t, "second session running", func() bool { return e.run1(s.RunID).Observed == "running" })
			tok := e.runToken(s.RunID)
			if r := e.bridgeCall(s.RunID, tok, "spaceman_kb_propose", map[string]any{"title": "x", "commit": c1}); r.OK || r.Code != "forbidden" {
				t.Fatalf("read-only proposal: %+v", r)
			}
		}
	}
}
