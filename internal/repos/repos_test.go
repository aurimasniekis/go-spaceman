package repos

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

type fx struct {
	t   *testing.T
	ctx context.Context
	s   *store.Store
	clk *clock.Fake
	svc *Service
}

// newFx seeds a node (protocol minor minor) with spc_a holding a valid lease and spc_b without one.
func newFx(t *testing.T, minor int, local bool) *fx {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	aw, ch := audit.NewWriter(kr), changefeed.New(nil)
	fl := fleet.New(s, kr, aw, eventbus.New(), ch, fleet.Options{Clock: clk})
	f := &fx{t: t, ctx: context.Background(), s: s, clk: clk, svc: New(s, aw, ch, fl, Options{AllowLocalURLs: local, Clock: clk})}
	exp := clk.Now().Add(time.Hour).UnixMicro()
	f.exec(`INSERT INTO nodes (id, name, state, protocol_minor, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', ?, 0, 't', 0)`, minor)
	f.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`)
	f.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`)
	f.exec(`INSERT INTO execution_leases (space_id, node_id, generation, state, granted_at, renewed_at, expires_at) VALUES ('spc_a', 'nod_1', 1, 'active', 0, 0, ?)`, exp)
	return f
}

func (f *fx) exec(q string, args ...any) {
	f.t.Helper()
	if err := f.s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, q, args...)
		return err
	}); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

// commands returns the queued space.repos.sync payloads of a space, oldest first.
func (f *fx) commands(space string) []proto.ReposSyncPayload {
	f.t.Helper()
	rows, err := f.s.ReadDB().QueryContext(f.ctx, `SELECT payload_json FROM node_commands WHERE space_id = ? AND kind = ? ORDER BY created_at, id`, space, proto.CmdReposSync)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []proto.ReposSyncPayload
	for rows.Next() {
		var s string
		var p proto.ReposSyncPayload
		if err := rows.Scan(&s); err != nil || json.Unmarshal([]byte(s), &p) != nil {
			f.t.Fatalf("payload %q %v", s, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func code(err error) apperr.Code { return apperr.CodeOf(err) }

// SM-REPO-001, SM-SPACE-001: repositories are configured per space with validation, If-Match versions and audit; a
// URL with credentials or a remote helper is refused; name and path are fixed.
func TestRepositoryCRUDAndValidation(t *testing.T) {
	f := newFx(t, proto.MinorRepositories, false)
	for name, in := range map[string]Input{
		"credentials":  {Name: "a", URL: "https://user:tok@git.example.com/a.git"},
		"https user":   {Name: "a", URL: "https://ghp_x@git.example.com/a.git"},
		"ext helper":   {Name: "a", URL: "ext::sh -c touch% /tmp/pwned"},
		"file in prod": {Name: "a", URL: "file:///srv/a.git"},
		"local path":   {Name: "a", URL: "/srv/a.git"},
		"http in prod": {Name: "a", URL: "http://git.example.com/a.git"},
		"option":       {Name: "a", URL: "--upload-pack=touch"},
		"no url auto":  {Name: "a", ClonePolicy: proto.CloneAuto},
		"reserved":     {Name: "a", Path: "worktrees/a", URL: "https://git.example.com/a.git"},
		"escape":       {Name: "a", Path: "../a", URL: "https://git.example.com/a.git"},
		"git dir":      {Name: "a", Path: "x/.git", URL: "https://git.example.com/a.git"},
		"bad name":     {Name: "A b", URL: "https://git.example.com/a.git"},
		"bad branch":   {Name: "a", URL: "https://git.example.com/a.git", DefaultBranch: "-x"},
		"bad role":     {Name: "a", URL: "https://git.example.com/a.git", Role: "docs"},
	} {
		if _, err := f.svc.Create(f.ctx, admin, "spc_a", in); code(err) != apperr.CodeInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	r, err := f.svc.Create(f.ctx, admin, "spc_a", Input{Name: "api", Path: "repos/api", URL: "git@git.example.com:acme/api.git", Description: "The API"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ClonePolicy != proto.CloneAuto || r.DefaultBranch != "main" || r.Role != RoleCode || r.Version != 1 || r.Status != nil {
		t.Fatalf("created %+v", r)
	}
	manual, err := f.svc.Create(f.ctx, admin, "spc_a", Input{Name: "notes"})
	if err != nil || manual.ClonePolicy != proto.CloneManual || manual.Path != "notes" || manual.URL != "" {
		t.Fatalf("manual %+v %v", manual, err)
	}
	if _, err := f.svc.Create(f.ctx, admin, "spc_a", Input{Name: "api", URL: "https://git.example.com/x.git"}); code(err) != apperr.CodeAlreadyExists {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := f.svc.Create(f.ctx, admin, "spc_a", Input{Name: "other", Path: "repos/api", URL: "https://git.example.com/x.git"}); code(err) != apperr.CodeAlreadyExists {
		t.Fatalf("duplicate path: %v", err)
	}
	// The same name in another space is fine.
	if _, err := f.svc.Create(f.ctx, admin, "spc_b", Input{Name: "api", URL: "https://git.example.com/x.git"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Update(f.ctx, admin, "spc_a", r.ID, 1, Input{Path: "elsewhere", URL: r.URL}); code(err) != apperr.CodeInvalid {
		t.Fatalf("path change: %v", err)
	}
	up, err := f.svc.Update(f.ctx, admin, "spc_a", r.ID, 1, Input{URL: "https://git.example.com/acme/api.git", DefaultBranch: "trunk", ClonePolicy: proto.CloneOnDemand})
	if err != nil || up.Version != 2 || up.DefaultBranch != "trunk" || up.ClonePolicy != proto.CloneOnDemand || up.Path != "repos/api" || up.Description != "" {
		t.Fatalf("update %+v %v", up, err)
	}
	if _, err := f.svc.Update(f.ctx, admin, "spc_a", r.ID, 1, Input{URL: up.URL}); code(err) != apperr.CodePrecondition {
		t.Fatalf("stale version: %v", err)
	}
	if got, err := f.svc.Get(f.ctx, "spc_a", "api"); err != nil || got.ID != r.ID {
		t.Fatalf("get by name: %+v %v", got, err)
	}
	if err := f.svc.Delete(f.ctx, admin, "spc_a", r.ID, 1); code(err) != apperr.CodePrecondition {
		t.Fatalf("delete stale: %v", err)
	}
	if err := f.svc.Delete(f.ctx, admin, "spc_a", r.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(f.ctx, "spc_a", r.ID); code(err) != apperr.CodeNotFound {
		t.Fatalf("after delete: %v", err)
	}
	var n int
	_ = f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action LIKE 'space_repository.%'`).Scan(&n)
	if n < 5 {
		t.Fatalf("audit events %d", n)
	}
	// Development mode accepts local remotes.
	dev := newFx(t, proto.MinorRepositories, true)
	if _, err := dev.svc.Create(dev.ctx, admin, "spc_a", Input{Name: "local", URL: "/srv/a.git"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dev.svc.Create(dev.ctx, admin, "spc_a", Input{Name: "ext", URL: "ext::git-remote-x"}); code(err) != apperr.CodeInvalid {
		t.Fatalf("ext in development: %v", err)
	}
}

// ADR-0044: creating an auto repository queues a clone for the connected node; a sync clones what is missing and fetches
// what was asked; a node without the repository protocol or a space without a lease gets no command.
func TestSyncCommands(t *testing.T) {
	f := newFx(t, proto.MinorRepositories, false)
	api, err := f.svc.Create(f.ctx, admin, "spc_a", Input{Name: "api", URL: "https://git.example.com/api.git"})
	if err != nil {
		t.Fatal(err)
	}
	cmds := f.commands("spc_a")
	if len(cmds) != 1 || len(cmds[0].Repositories) != 1 || len(cmds[0].Clone) != 1 || cmds[0].Clone[0] != api.ID || len(cmds[0].Fetch) != 0 {
		t.Fatalf("auto clone %+v", cmds)
	}
	lazy, _ := f.svc.Create(f.ctx, admin, "spc_a", Input{Name: "lazy", URL: "https://git.example.com/lazy.git", ClonePolicy: proto.CloneOnDemand})
	if _, err := f.svc.Create(f.ctx, admin, "spc_a", Input{Name: "kb", Role: RoleKB}); err != nil {
		t.Fatal(err)
	}
	cmds = f.commands("spc_a")
	if last := cmds[len(cmds)-1]; len(last.Repositories) != 3 || len(last.Clone) != 1 {
		t.Fatalf("on_demand and manual are not cloned automatically: %+v", last)
	}
	res, err := f.svc.Sync(f.ctx, admin, "spc_a", SyncInput{Repositories: []string{"lazy"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Clone, ",") != "api,lazy" || strings.Join(res.Fetch, ",") != "lazy" || res.CommandID == "" {
		t.Fatalf("sync %+v", res)
	}
	if _, err := f.svc.Sync(f.ctx, admin, "spc_a", SyncInput{Repositories: []string{"nope"}}); code(err) != apperr.CodeNotFound {
		t.Fatalf("unknown repository: %v", err)
	}
	ov, _ := f.svc.List(f.ctx, "spc_a")
	if !ov.SyncPending || ov.RequestedAt == nil {
		t.Fatalf("pending %+v", ov)
	}
	_ = lazy
	// No lease: nothing is queued automatically, and an explicit sync says why.
	if _, err := f.svc.Create(f.ctx, admin, "spc_b", Input{Name: "api", URL: "https://git.example.com/api.git"}); err != nil {
		t.Fatal(err)
	}
	if len(f.commands("spc_b")) != 0 {
		t.Fatal("command queued without a lease")
	}
	if _, err := f.svc.Sync(f.ctx, admin, "spc_b", SyncInput{}); code(err) != apperr.CodeUnavailable {
		t.Fatalf("sync without lease: %v", err)
	}
	// An old agent: the configuration is stored, the explicit sync is refused with the reason.
	old := newFx(t, proto.MinorRelocation, false)
	if _, err := old.svc.Create(old.ctx, admin, "spc_a", Input{Name: "api", URL: "https://git.example.com/api.git"}); err != nil {
		t.Fatal(err)
	}
	if len(old.commands("spc_a")) != 0 {
		t.Fatal("command queued for an old node")
	}
	if _, err := old.svc.Sync(old.ctx, admin, "spc_a", SyncInput{}); code(err) != apperr.CodePrecondition || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("old node: %v", err)
	}
}

// SM-REPO-003: the node's report is stored per space (only from the space's node, only configured repositories, never an
// older one over a newer one) and summarized per repository: remote, heads, dirty, untracked, unpushed and worktrees
// with their owner.
func TestStatusReports(t *testing.T) {
	f := newFx(t, proto.MinorRepositories, false)
	r, _ := f.svc.Create(f.ctx, admin, "spc_a", Input{Name: "api", URL: "https://git.example.com/api.git"})
	at := f.clk.Now().Add(-time.Minute)
	var cmd string
	_ = f.s.ReadDB().QueryRowContext(f.ctx, `SELECT command_id FROM space_repository_reports WHERE space_id = 'spc_a'`).Scan(&cmd)
	if ov, _ := f.svc.List(f.ctx, "spc_a"); cmd == "" || !ov.SyncPending {
		t.Fatalf("the clone on create is pending: %q %+v", cmd, ov)
	}
	rep := proto.RepoStatusReport{CommandID: cmd, SpaceID: "spc_a", CheckedAt: at, Unlisted: []string{"scratch"}, Repositories: []proto.RepoState{
		{ID: r.ID, Path: "api", Present: true, Cloned: true, Remote: &proto.RepoRemoteState{URL: r.URL, Reachable: true, Head: "aaaa"}, DefaultHead: "aaaa",
			Tracking: "aaaa", LastFetchAt: &at, Check: &proto.RepoCheck{Path: "api", Branch: "main", Head: "aaaa", Dirty: 1, Untracked: 2,
				Branches:  []proto.BranchCheck{{Name: "main", Commit: "aaaa"}, {Name: "spaceman/tsk_1", Commit: "bbbb", Unpushed: 3, NoUpstream: true}},
				Worktrees: []proto.WorktreeCheck{{Path: "worktrees/spaceman-tsk_1", Branch: "spaceman/tsk_1", Head: "bbbb", Dirty: 4, Untracked: 5}}}},
		{ID: "rep_unknown", Path: "x", Present: true},
	}}
	f.svc.OnStatus(f.ctx, "nod_other", rep)
	if got, _ := f.svc.Get(f.ctx, "spc_a", r.ID); got.Status != nil {
		t.Fatal("report from another node stored")
	}
	f.svc.OnStatus(f.ctx, "nod_1", rep)
	got, _ := f.svc.Get(f.ctx, "spc_a", r.ID)
	st := got.Status
	if st == nil || st.State != "present" || st.Stale || st.Dirty != 1 || st.Untracked != 2 || st.Unpushed != 3 || !st.Remote.Reachable ||
		st.DefaultHead != "aaaa" || st.LastFetchAt == nil || len(st.Worktrees) != 1 || st.Worktrees[0].Unpushed != 3 || st.Worktrees[0].Dirty != 4 {
		t.Fatalf("status %+v", st)
	}
	ov, _ := f.svc.List(f.ctx, "spc_a")
	if len(ov.Unlisted) != 1 || ov.SyncPending || ov.ReportedAt == nil {
		t.Fatalf("overview %+v", ov)
	}
	var stored string
	_ = f.s.ReadDB().QueryRowContext(f.ctx, `SELECT report_json FROM space_repository_reports WHERE space_id = 'spc_a'`).Scan(&stored)
	if strings.Contains(stored, "rep_unknown") {
		t.Fatal("unconfigured repository kept")
	}
	older := rep
	older.CheckedAt = at.Add(-time.Hour)
	older.Repositories = []proto.RepoState{{ID: r.ID, Path: "api", Present: false}}
	f.svc.OnStatus(f.ctx, "nod_1", older)
	if got, _ := f.svc.Get(f.ctx, "spc_a", r.ID); got.Status.State != "present" {
		t.Fatal("an older report replaced a newer one")
	}
	// After a relocation the report is the earlier node's: shown, marked stale.
	f.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_2', 'n2', 'active', 0, 't', 0)`)
	f.exec(`UPDATE spaces SET node_id = 'nod_2' WHERE id = 'spc_a'`)
	if got, _ := f.svc.Get(f.ctx, "spc_a", r.ID); got.Status == nil || !got.Status.Stale {
		t.Fatalf("stale %+v", got.Status)
	}
}

// Knowledge bases keep naming a folder; every KB folder is a listed repository (role kb, manual), and a repository a KB
// uses cannot be removed.
func TestKnowledgeBaseRepositories(t *testing.T) {
	f := newFx(t, proto.MinorRepositories, false)
	var path string
	err := f.s.W().TxNamed(f.ctx, "kb", func(tx *store.Tx) error {
		var err error
		path, err = f.svc.ForKB(f.ctx, tx, admin, "spc_a", "knowledge/main", "trunk")
		return err
	})
	if err != nil || path != "knowledge/main" {
		t.Fatalf("for kb %q %v", path, err)
	}
	r, err := f.svc.Get(f.ctx, "spc_a", "knowledge-main")
	if err != nil || r.Role != RoleKB || r.ClonePolicy != proto.CloneManual || r.DefaultBranch != "trunk" {
		t.Fatalf("registered %+v %v", r, err)
	}
	// A configured folder is reused as it is.
	err = f.s.W().TxNamed(f.ctx, "kb", func(tx *store.Tx) error {
		path, err = f.svc.ForKB(f.ctx, tx, admin, "spc_a", "knowledge/main", "main")
		return err
	})
	if err != nil || path != "knowledge/main" {
		t.Fatalf("reuse %q %v", path, err)
	}
	if ov, _ := f.svc.List(f.ctx, "spc_a"); len(ov.Items) != 1 {
		t.Fatalf("registered twice: %d", len(ov.Items))
	}
	err = f.s.W().TxNamed(f.ctx, "kb", func(tx *store.Tx) error {
		_, err := f.svc.ForKB(f.ctx, tx, admin, "spc_a", "instances/x", "main")
		return err
	})
	if code(err) != apperr.CodeInvalid {
		t.Fatalf("reserved folder: %v", err)
	}
	f.exec(`INSERT INTO kb_repos (id, space_id, slug, name, repo, space_role, created_at, created_by, updated_at) VALUES ('kbr_1', 'spc_a', 'docs', 'Docs', 'knowledge/main', 'kb', 0, 't', 0)`)
	if got, _ := f.svc.Get(f.ctx, "spc_a", r.ID); len(got.KBs) != 1 || got.KBs[0] != "docs" {
		t.Fatalf("kbs %+v", got.KBs)
	}
	if err := f.svc.Delete(f.ctx, admin, "spc_a", r.ID, 0); code(err) != apperr.CodeConflict {
		t.Fatalf("delete a KB's repository: %v", err)
	}
}

func TestValidPath(t *testing.T) {
	for p, ok := range map[string]bool{"api": true, "repos/api": true, "a.b_c-d": true, "": false, "/abs": false, "../x": false, "a/../b": false,
		"a//b": false, "instances": false, "worktrees/x": false, "work": false, "dreams/x": false, "x/.git": false, ".spaceman-x": false, "-x": false,
		"a/b/c/d/e/f/g/h/i": false, "a\\b": false, "a\nb": false} {
		if ValidPath(p) != ok {
			t.Errorf("ValidPath(%q) != %v", p, ok)
		}
	}
}
