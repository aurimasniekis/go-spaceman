package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

type fx struct {
	t   *testing.T
	ctx context.Context
	git *Git
	svc *Service
	// node is a working repository standing in for the node's shared history.
	node string
}

func newFx(t *testing.T) *fx {
	t.Helper()
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	s := storetest.Open(t, storetest.WithClock(clock.NewFake(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))))
	kr := keyringtest.Load(t, s)
	f := &fx{t: t, ctx: context.Background(), git: g, node: t.TempDir()}
	f.svc = New(s, audit.NewWriter(kr), changefeed.New(nil), t.TempDir(), g, nil)
	must(t, s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_2', 'n2', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(f.ctx, q); err != nil {
				return err
			}
		}
		return nil
	}))
	f.run("init", "--quiet", "--initial-branch=main", ".")
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fx) run(args ...string) string {
	f.t.Helper()
	out, err := f.git.Run(f.ctx, f.node, &Identity{Name: "n", Email: "n@x"}, args...)
	must(f.t, err)
	return strings.TrimSpace(out)
}

// commit writes a file and commits it on the node, returning the new head.
func (f *fx) commit(name, content string) string {
	f.t.Helper()
	must(f.t, os.MkdirAll(filepath.Dir(filepath.Join(f.node, name)), 0o700))
	must(f.t, os.WriteFile(filepath.Join(f.node, name), []byte(content), 0o600))
	f.run("add", "-A")
	f.run("commit", "--quiet", "-m", "write "+name)
	return f.run("rev-parse", "HEAD")
}

func (f *fx) sync(parent, head string) proto.MemoryAck {
	f.t.Helper()
	b, err := f.git.Bundle(f.ctx, f.node, parent, "refs/heads/main", proto.MemoryMaxBundle)
	must(f.t, err)
	return f.svc.Apply(f.ctx, "nod_1", proto.MemorySync{SyncID: "msy_" + head[:8], SpaceID: "spc_a", Role: "implementor-backend", Parent: parent, Head: head, Bundle: b})
}

func TestApplyFastForwardsAndServes(t *testing.T) {
	f := newFx(t)
	h1 := f.commit("README.md", "# memory\n")
	if ack := f.sync("", h1); ack.Result != proto.MemoryOK || ack.Head != h1 {
		t.Fatalf("first sync: %+v", ack)
	}
	h2 := f.commit("decisions.md", "- use retries\n")
	h3 := f.commit("notes/settlement.md", "bank window 01:00-03:00\n")
	if ack := f.sync(h1, h3); ack.Result != proto.MemoryOK || ack.Head != h3 {
		t.Fatalf("incremental sync: %+v", ack)
	}
	_ = h2
	// The same head again is acknowledged without change.
	if ack := f.sync(h1, h3); ack.Result != proto.MemoryOK {
		t.Fatalf("repeat: %+v", ack)
	}
	files, err := f.svc.Files(f.ctx, "spc_a", "implementor-backend")
	must(t, err)
	if strings.Join(files, ",") != "README.md,decisions.md,notes/settlement.md" {
		t.Fatalf("files: %v", files)
	}
	c, err := f.svc.File(f.ctx, "spc_a", "implementor-backend", "notes/settlement.md")
	if err != nil || c != "bank window 01:00-03:00\n" {
		t.Fatalf("file: %q %v", c, err)
	}
	for _, p := range []string{"../x", "/etc/passwd", ".git/config", "missing.md", ""} {
		if _, err := f.svc.File(f.ctx, "spc_a", "implementor-backend", p); err == nil {
			t.Fatalf("%q was served", p)
		}
	}
	hist, err := f.svc.History(f.ctx, "spc_a", "implementor-backend", 10)
	must(t, err)
	if len(hist) != 3 || hist[0].ID != h3 || hist[0].Subject != "write notes/settlement.md" {
		t.Fatalf("history: %+v", hist)
	}
	l, err := f.svc.List(f.ctx, "spc_a")
	must(t, err)
	if len(l) != 1 || l[0].Head != h3 || l[0].HeadAt == nil {
		t.Fatalf("list: %+v", l)
	}
}

func TestApplyRefusesAndPreserves(t *testing.T) {
	f := newFx(t)
	h1 := f.commit("a.md", "1\n")
	b, err := f.git.Bundle(f.ctx, f.node, "", "refs/heads/main", proto.MemoryMaxBundle)
	must(t, err)
	base := proto.MemorySync{SyncID: "msy_1", SpaceID: "spc_a", Role: "control", Head: h1, Bundle: b}

	// Not the owning node, bad names, a head the bundle does not contain.
	bad := []struct {
		node string
		m    proto.MemorySync
	}{
		{"nod_2", base},
		{"nod_1", func() proto.MemorySync { m := base; m.Role = "Bad Role"; return m }()},
		{"nod_1", func() proto.MemorySync { m := base; m.Head = "nothex"; return m }()},
		{"nod_1", func() proto.MemorySync { m := base; m.Bundle = nil; return m }()},
		{"nod_1", func() proto.MemorySync { m := base; m.Head = strings.Repeat("a", 40); return m }()},
		{"nod_1", func() proto.MemorySync { m := base; m.Bundle = []byte("not a bundle"); return m }()},
	}
	for i, c := range bad {
		if ack := f.svc.Apply(f.ctx, c.node, c.m); ack.Result != proto.MemoryError {
			t.Errorf("case %d: %+v", i, ack)
		}
	}
	if ack := f.svc.Apply(f.ctx, "nod_1", base); ack.Result != proto.MemoryOK {
		t.Fatalf("base: %+v", ack)
	}

	// A bundle built on a commit central does not have.
	h2 := f.commit("a.md", "2\n")
	h3 := f.commit("a.md", "3\n")
	b2, err := f.git.Bundle(f.ctx, f.node, h2, "refs/heads/main", proto.MemoryMaxBundle)
	must(t, err)
	if ack := f.svc.Apply(f.ctx, "nod_1", proto.MemorySync{SyncID: "msy_2", SpaceID: "spc_a", Role: "control", Parent: h2, Head: h3, Bundle: b2}); ack.Result != proto.MemoryMissingBase {
		t.Fatalf("missing base: %+v", ack)
	}

	// Rewritten history (not a descendant of central's head) is preserved, never applied.
	f.run("reset", "--quiet", "--hard", h1)
	f.run("commit", "--quiet", "--amend", "-m", "rewritten")
	other := f.run("rev-parse", "HEAD")
	b3, err := f.git.Bundle(f.ctx, f.node, "", "refs/heads/main", proto.MemoryMaxBundle)
	must(t, err)
	ack := f.svc.Apply(f.ctx, "nod_1", proto.MemorySync{SyncID: "msy_3", SpaceID: "spc_a", Role: "control", Head: other, Bundle: b3})
	if ack.Result != proto.MemoryConflict || ack.Head != h1 || !strings.Contains(ack.Message, "refs/conflicts/") {
		t.Fatalf("conflict: %+v", ack)
	}
	c, err := f.svc.Get(f.ctx, "spc_a", "control")
	must(t, err)
	if c.Head != h1 || c.Conflicts != 1 {
		t.Fatalf("after conflict: %+v", c)
	}
	conf, err := f.svc.Conflicts(f.ctx, "spc_a", "control")
	must(t, err)
	if len(conf) != 1 || conf[0].Commit != other || f.git.RevParse(f.ctx, f.svc.repo("spc_a", "control"), conf[0].Ref) != other {
		t.Fatalf("preserved: %+v", conf)
	}
	must(t, f.svc.ResolveConflict(f.ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_a"}, "spc_a", "control", conf[0].ID))
	if err := f.svc.ResolveConflict(f.ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_a"}, "spc_a", "control", conf[0].ID); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("resolve twice: %v", err)
	}
	if err := f.svc.ResolveConflict(f.ctx, audit.Actor{}, "spc_a", "control", "mcf_missing"); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("resolve missing: %v", err)
	}
}

func TestReportAndWithoutGit(t *testing.T) {
	f := newFx(t)
	f.svc.Report(f.ctx, "nod_1", proto.MemoryStatus{Collections: []proto.MemoryCollectionStatus{
		{SpaceID: "spc_a", Role: "reviewer", Uncheckpointed: 2, LocalCommits: 1, Pending: true, Conflicts: 1},
		{SpaceID: "spc_a", Role: "Bad"},
	}})
	f.svc.Report(f.ctx, "nod_2", proto.MemoryStatus{Collections: []proto.MemoryCollectionStatus{{SpaceID: "spc_a", Role: "intruder"}}})
	l, err := f.svc.List(f.ctx, "spc_a")
	must(t, err)
	if len(l) != 1 || l[0].Node == nil || l[0].Node.Uncheckpointed != 2 || !l[0].Node.Pending || l[0].Conflicts != 1 {
		t.Fatalf("status: %+v", l)
	}
	if _, err := f.svc.Files(f.ctx, "spc_a", "reviewer"); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("files before any sync: %v", err)
	}
	if _, err := f.svc.Get(f.ctx, "spc_a", "nobody"); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("missing collection: %v", err)
	}
	f.svc.git = nil
	if f.svc.Available() {
		t.Fatal("available without git")
	}
	h := f.commit("x.md", "x")
	b, _ := (&Git{Bin: f.git.Bin}).Bundle(f.ctx, f.node, "", "refs/heads/main", proto.MemoryMaxBundle)
	if ack := f.svc.Apply(f.ctx, "nod_1", proto.MemorySync{SyncID: "msy_x", SpaceID: "spc_a", Role: "reviewer", Head: h, Bundle: b}); ack.Result != proto.MemoryError || !strings.Contains(ack.Message, "git") {
		t.Fatalf("without git: %+v", ack)
	}
}

func TestBundleLimit(t *testing.T) {
	f := newFx(t)
	f.commit("big.md", strings.Repeat("x", 4096))
	if _, err := f.git.Bundle(f.ctx, f.node, "", "refs/heads/main", 100); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("limit: %v", err)
	}
	if ValidCommit("abc") || !ValidCommit(strings.Repeat("a", 40)) {
		t.Fatal("ValidCommit")
	}
}

func TestStatusTool(t *testing.T) {
	f := newFx(t)
	r := agenttools.New(agenttools.Deps{Extra: []func(*agenttools.Registry){Tools(f.svc)}})
	p := &fleet.RunPrincipal{RunID: "run_1", SpaceID: "spc_a", Kind: "worker", Role: "implementor-backend"}
	out, err := r.Dispatch(f.ctx, p, "spaceman_memory_status", nil)
	if err != nil || !strings.Contains(string(out), "nothing has been synced") {
		t.Fatalf("before sync: %s %v", out, err)
	}
	h := f.commit("a.md", "a")
	f.sync("", h)
	out, err = r.Dispatch(f.ctx, p, "spaceman_memory_status", nil)
	if err != nil || !strings.Contains(string(out), h) || !strings.Contains(string(out), `"open_conflicts":[]`) {
		t.Fatalf("after sync: %s %v", out, err)
	}
}
