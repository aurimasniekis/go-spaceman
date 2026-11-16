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
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
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
	r := agenttools.New(agenttools.Deps{Extra: []func(*agenttools.Registry){Tools(f.svc, nil)}})
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

// SM-OBS-004: each fast-forward of central's copy is recorded for publication (commits only, never file content), with
// the node's sync as causation; a repeat of the same head records nothing.
func TestSyncEventsRecorded(t *testing.T) {
	f := newFx(t)
	f.svc.Outbox = outbox.New(f.svc.s, outbox.Options{Enabled: true})
	h1 := f.commit("README.md", "CONTENT-SECRET\n")
	f.sync("", h1)
	h2 := f.commit("b.md", "more\n")
	f.sync(h1, h2)
	f.sync(h1, h2)
	rows, err := f.svc.s.ReadDB().QueryContext(f.ctx, `SELECT causation_id, payload_json FROM event_outbox ORDER BY id`)
	must(t, err)
	defer rows.Close()
	var got []string
	for rows.Next() {
		var cause, payload string
		must(t, rows.Scan(&cause, &payload))
		if strings.Contains(payload, "SECRET") || !strings.Contains(payload, `"role":"implementor-backend"`) {
			t.Fatalf("payload %s", payload)
		}
		got = append(got, cause)
	}
	must(t, rows.Err())
	if strings.Join(got, ",") != "msy_"+h1[:8]+",msy_"+h2[:8] {
		t.Fatalf("events %v", got)
	}
}

// ADR-0037: a relocation's destination gets central's acknowledged history (and nothing newer) as one bundle per role;
// central's own repository keeps no temporary ref.
func TestExportBundlesForRelocation(t *testing.T) {
	f := newFx(t)
	h1 := f.commit("README.md", "# memory\n")
	if ack := f.sync("", h1); ack.Result != proto.MemoryOK {
		t.Fatalf("sync: %+v", ack)
	}
	f.commit("later.md", "not synced\n")
	_, _ = f.svc.collection(f.ctx, "spc_a", "reviewer") // a collection without history is skipped
	dir := t.TempDir()
	mems, err := f.svc.ExportBundles(f.ctx, "spc_a", dir)
	must(t, err)
	if len(mems) != 1 || mems[0].Role != "implementor-backend" || mems[0].Head != h1 || mems[0].Blob != "memory-implementor-backend.bundle" {
		t.Fatalf("export %+v", mems)
	}
	dest := t.TempDir()
	must(t, f.git.InitBare(f.ctx, dest))
	if _, err := f.git.Run(f.ctx, dest, nil, "fetch", "--quiet", filepath.Join(dir, mems[0].Blob), "+refs/relocation/main:refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if got := f.git.RevParse(f.ctx, dest, "refs/heads/main"); got != h1 {
		t.Fatalf("restored head %s, want %s", got, h1)
	}
	if ref := f.git.RevParse(f.ctx, f.svc.repo("spc_a", "implementor-backend"), "refs/relocation/main"); ref != "" {
		t.Fatal("temporary ref left in central's repository")
	}
	// Without git, a space with history cannot be exported.
	f.svc.git = nil
	if _, err := f.svc.ExportBundles(f.ctx, "spc_a", dir); err == nil {
		t.Fatal("exported without git")
	}
}

// reportConflicts has the node report n preserved conflicts and the given conflicts/ folders for the role.
func (f *fx) reportConflicts(role string, n int, folders ...string) {
	f.svc.Report(f.ctx, "nod_1", proto.MemoryStatus{Collections: []proto.MemoryCollectionStatus{{SpaceID: "spc_a", Role: role, Conflicts: n, ConflictFolders: folders}}})
}

func (f *fx) conflicts(role string) map[string]Conflict {
	f.t.Helper()
	cs, err := f.svc.Conflicts(f.ctx, "spc_a", role)
	must(f.t, err)
	out := map[string]Conflict{}
	for _, c := range cs {
		out[c.Ref+"|"+c.ID] = c
	}
	return out
}

func (f *fx) open(role string) int {
	f.t.Helper()
	c, err := f.svc.Get(f.ctx, "spc_a", role)
	must(f.t, err)
	return int(c.Conflicts)
}

// Removing a conflicts/<name>/ folder in an accepted checkpoint resolves the conflict kept for it, with a system actor
// and the preserved ref intact; a folder that never was in the collection, and a conflict without a folder, stay open.
func TestRemovingConflictFolderResolves(t *testing.T) {
	f := newFx(t)
	const role = "implementor-backend"
	const a, b = "ins_a-20261101T000000Z", "ins_b-20261102T000000Z"
	h1 := f.commit("README.md", "# memory\n")
	f.sync("", h1)
	f.reportConflicts(role, 1, a)    // folder a
	f.reportConflicts(role, 2, a, b) // folder b (a is already tied)
	f.reportConflicts(role, 3, a, b, "ghost")
	f.reportConflicts(role, 4, a, b, "ghost") // no new folder: recorded without one
	if n := f.open(role); n != 4 {
		t.Fatalf("open before: %d", n)
	}
	h2 := f.commit("conflicts/"+a+"/x.md", "mine\n")
	h3 := f.commit("conflicts/"+b+"/y.md", "mine\n")
	if ack := f.sync(h1, h3); ack.Result != proto.MemoryOK {
		t.Fatalf("sync: %+v", ack)
	}
	_ = h2
	if n := f.open(role); n != 4 {
		t.Fatalf("adding folders must not resolve: %d", n)
	}
	// The role reconciles folder a and deletes it.
	f.run("rm", "-r", "--quiet", "conflicts/"+a)
	f.run("commit", "--quiet", "-m", "reconcile a")
	h4 := f.run("rev-parse", "HEAD")
	if ack := f.sync(h3, h4); ack.Result != proto.MemoryOK {
		t.Fatalf("sync 2: %+v", ack)
	}
	if n := f.open(role); n != 3 {
		t.Fatalf("open after removing a: %d", n)
	}
	resolved := 0
	for _, c := range f.conflicts(role) {
		if c.ResolvedAt != nil {
			resolved++
			if c.ResolvedBy != AutoResolver.ID || c.Ref != "refs/conflicts/"+a {
				t.Fatalf("resolved conflict: %+v", c)
			}
		}
	}
	if resolved != 1 {
		t.Fatalf("resolved: %d", resolved)
	}
	// The same checkpoint again changes nothing (idempotent); so does a later one that touches no folder.
	f.sync(h3, h4)
	h5 := f.commit("notes.md", "n\n")
	f.sync(h4, h5)
	if n := f.open(role); n != 3 {
		t.Fatalf("open after repeats: %d", n)
	}
	// Removing b resolves b only; "ghost" never existed in the collection and the folderless record stays open.
	f.run("rm", "-r", "--quiet", "conflicts")
	f.run("commit", "--quiet", "-m", "reconcile b")
	h6 := f.run("rev-parse", "HEAD")
	f.sync(h5, h6)
	if n := f.open(role); n != 2 {
		t.Fatalf("open after removing b: %d", n)
	}
	// An auto-resolved conflict cannot be resolved again by hand.
	for _, c := range f.conflicts(role) {
		if c.ResolvedBy == AutoResolver.ID {
			if err := f.svc.ResolveConflict(f.ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_a"}, "spc_a", role, c.ID); apperr.CodeOf(err) != apperr.CodePrecondition {
				t.Fatalf("resolve resolved: %v", err)
			}
		}
	}
}

func TestConflictTools(t *testing.T) {
	f := newFx(t)
	covers := map[string][]string{"run_dream": {"reviewer"}, "run_all": {"*"}}
	dr := func(_ context.Context, id string) ([]string, error) {
		if r, ok := covers[id]; ok {
			return r, nil
		}
		return nil, apperr.New(apperr.CodeForbidden, "this run is not a dream")
	}
	r := agenttools.New(agenttools.Deps{Extra: []func(*agenttools.Registry){Tools(f.svc, dr)}})
	f.reportConflicts("reviewer", 1)
	f.reportConflicts("implementor-backend", 1)
	ids := func(role string) string {
		for _, c := range f.conflicts(role) {
			return c.ID
		}
		return ""
	}
	rev, impl := ids("reviewer"), ids("implementor-backend")
	control := &fleet.RunPrincipal{RunID: "run_ctl", SpaceID: "spc_a", Kind: "control", Role: "control"}
	dreamer := &fleet.RunPrincipal{RunID: "run_dream", SpaceID: "spc_a", Kind: "dreamer", Role: "dreamer"}
	allDreamer := &fleet.RunPrincipal{RunID: "run_all", SpaceID: "spc_a", Kind: "dreamer", Role: "dreamer"}
	call := func(p *fleet.RunPrincipal, tool, args string) (string, error) {
		out, err := r.Dispatch(f.ctx, p, tool, []byte(args))
		return string(out), err
	}

	// Only control and the dreamer see the tools; no other kind may call them.
	for _, kind := range []string{"worker", "kb", "diagnostic"} {
		p := &fleet.RunPrincipal{RunID: "run_x", SpaceID: "spc_a", Kind: kind, Role: "implementor-backend"}
		for _, tl := range r.Catalog(kind) {
			if strings.HasPrefix(tl.Name, "spaceman_memory_conflict_") {
				t.Fatalf("%s sees %s", kind, tl.Name)
			}
		}
		if _, err := call(p, "spaceman_memory_conflict_resolve", `{"role":"implementor-backend","conflict_id":"`+impl+`"}`); apperr.CodeOf(err) != apperr.CodeForbidden {
			t.Fatalf("%s resolve: %v", kind, err)
		}
		if _, err := call(p, "spaceman_memory_conflict_list", ``); apperr.CodeOf(err) != apperr.CodeForbidden {
			t.Fatalf("%s list: %v", kind, err)
		}
	}
	for _, kind := range []string{"control", "dreamer"} {
		seen := 0
		for _, tl := range r.Catalog(kind) {
			if strings.HasPrefix(tl.Name, "spaceman_memory_conflict_") {
				seen++
			}
		}
		if seen != 2 {
			t.Fatalf("%s sees %d conflict tools", kind, seen)
		}
	}

	// The dreamer: only the roles its dream covers.
	out, err := call(dreamer, "spaceman_memory_conflict_list", ``)
	if err != nil || !strings.Contains(out, rev) || strings.Contains(out, impl) {
		t.Fatalf("dreamer list: %s %v", out, err)
	}
	if _, err := call(dreamer, "spaceman_memory_conflict_resolve", `{"role":"implementor-backend","conflict_id":"`+impl+`"}`); apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Fatalf("dreamer outside its roles: %v", err)
	}
	if _, err := call(&fleet.RunPrincipal{RunID: "run_other", SpaceID: "spc_a", Kind: "dreamer"}, "spaceman_memory_conflict_list", ``); apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Fatalf("dreamer without a dream: %v", err)
	}
	// A conflict id of another role does not resolve through a covered role.
	if _, err := call(dreamer, "spaceman_memory_conflict_resolve", `{"role":"reviewer","conflict_id":"`+impl+`"}`); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("cross-role id: %v", err)
	}
	if _, err := call(dreamer, "spaceman_memory_conflict_resolve", `{"role":"reviewer","conflict_id":"`+rev+`"}`); err != nil {
		t.Fatalf("dreamer resolve: %v", err)
	}
	if c := f.conflicts("reviewer"); func() string {
		for _, x := range c {
			return x.ResolvedBy
		}
		return ""
	}() != "run_dream" {
		t.Fatalf("actor not recorded: %+v", c)
	}
	if _, err := call(dreamer, "spaceman_memory_conflict_resolve", `{"role":"reviewer","conflict_id":"`+rev+`"}`); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("resolve twice: %v", err)
	}

	// A dream covering "*" and control reach every role; control's own space only.
	if out, err := call(allDreamer, "spaceman_memory_conflict_list", ``); err != nil || !strings.Contains(out, impl) {
		t.Fatalf("all-roles dreamer: %s %v", out, err)
	}
	if out, err := call(control, "spaceman_memory_conflict_list", `{"role":"implementor-backend"}`); err != nil || !strings.Contains(out, impl) {
		t.Fatalf("control list: %s %v", out, err)
	}
	elsewhere := &fleet.RunPrincipal{RunID: "run_ctl2", SpaceID: "spc_other", Kind: "control", Role: "control"}
	if _, err := call(elsewhere, "spaceman_memory_conflict_resolve", `{"role":"implementor-backend","conflict_id":"`+impl+`"}`); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("other space: %v", err)
	}
	if _, err := call(control, "spaceman_memory_conflict_resolve", `{"role":"implementor-backend","conflict_id":"`+impl+`"}`); err != nil {
		t.Fatalf("control resolve: %v", err)
	}
	if f.open("implementor-backend") != 0 {
		t.Fatal("still open")
	}
}
