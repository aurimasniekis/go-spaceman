package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

const (
	oldRole = "implementor-backend"
	newRole = "implementor-hard"
)

// renameEnv is the memory fixture with the role as a space role, the space's node at protocol minor 14 and a catalog
// service to rename it with.
type renameEnv struct {
	*fx
	cat *catalog.Service
}

func newRenameEnv(t *testing.T) *renameEnv {
	t.Helper()
	f := newFx(t)
	e := &renameEnv{fx: f, cat: catalog.New(f.svc.s, f.svc.audit, changefeed.New(nil), catalog.Options{})}
	e.exec(`UPDATE nodes SET protocol_minor = ? WHERE id = 'nod_1'`, proto.MinorRoleRename)
	e.exec(`INSERT INTO role_templates (id, slug, name, kind, created_at, created_by, updated_at) VALUES ('rtp_1', 'w', 'W', 'worker', 0, 't', 0)`)
	e.addRole(oldRole)
	return e
}

func (e *renameEnv) exec(q string, args ...any) {
	e.t.Helper()
	must(e.t, e.svc.s.W().TxNamed(e.ctx, "test.exec", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(e.ctx, q, args...)
		return err
	}))
}

func (e *renameEnv) addRole(name string) {
	e.exec(`INSERT INTO space_roles (space_id, name, template_id, created_at, created_by, updated_at) VALUES ('spc_a', ?, 'rtp_1', 0, 't', 0)`, name)
}

// rename renames the role through the catalog; with moveAfter the central move runs after the commit, as central wires
// it (catalog.Service.AfterRename); without it the move is left undone, as after a crash between commit and move.
func (e *renameEnv) rename(from, to string, moveAfter bool) *catalog.RoleRename {
	e.t.Helper()
	e.cat.AfterRename = nil
	if moveAfter {
		e.cat.AfterRename = func(ctx context.Context, r catalog.RoleRename) { e.svc.RenameCommitted(ctx, r.SpaceID) }
	}
	_, rn, err := e.cat.RenameSpaceRole(e.ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_op"}, "spc_a", from, to, 0)
	must(e.t, err)
	return rn
}

func (e *renameEnv) moved(id string) bool {
	e.t.Helper()
	r, err := e.svc.s.R().GetSpaceRoleRename(e.ctx, id)
	must(e.t, err)
	return r.CentralMovedAt.Valid
}

func (e *renameEnv) repoExists(role string) bool {
	e.t.Helper()
	ok, err := exists(e.svc.repo("spc_a", role))
	must(e.t, err)
	return ok
}

func (e *renameEnv) collections() map[string]string {
	e.t.Helper()
	rows, err := e.svc.s.R().ListMemoryCollections(e.ctx, "spc_a")
	must(e.t, err)
	out := map[string]string{}
	for _, r := range rows {
		out[r.Role] = r.Head
	}
	return out
}

func (e *renameEnv) syncAs(role, parent, head string) proto.MemoryAck {
	e.t.Helper()
	b, err := e.git.Bundle(e.ctx, e.node, parent, "refs/heads/main", proto.MemoryMaxBundle)
	must(e.t, err)
	return e.svc.Apply(e.ctx, "nod_1", proto.MemorySync{SyncID: "msy_" + role[:4] + head[:8], SpaceID: "spc_a", Role: role, Parent: parent, Head: head, Bundle: b})
}

// ADR-0093 T-H2 acceptance 1 and 5: the central repository is renamed right after the commit, the collection serves
// its history under the new name, and a sync under the new name fast-forwards the head it already had.
func TestRenameMovesRepositoryAfterCommit(t *testing.T) {
	t.Parallel()
	e := newRenameEnv(t)
	h1 := e.commit("decisions.md", "one")
	if ack := e.syncAs(oldRole, "", h1); ack.Result != proto.MemoryOK {
		t.Fatalf("first sync: %+v", ack)
	}
	rn := e.rename(oldRole, newRole, true)
	if rn.NodeState != catalog.RenameNodePending {
		t.Fatalf("node state %q", rn.NodeState)
	}
	if e.repoExists(oldRole) || !e.repoExists(newRole) || !e.moved(rn.ID) {
		t.Fatalf("not moved after the commit: old %v new %v recorded %v", e.repoExists(oldRole), e.repoExists(newRole), e.moved(rn.ID))
	}
	if got := e.collections(); len(got) != 1 || got[newRole] != h1 {
		t.Fatalf("collections %v", got)
	}
	files, err := e.svc.Files(e.ctx, "spc_a", newRole)
	if err != nil || len(files) != 1 || files[0] != "decisions.md" {
		t.Fatalf("files under the new name: %v %v", files, err)
	}
	if _, err := e.svc.Files(e.ctx, "spc_a", oldRole); err == nil {
		t.Fatal("the old name still serves files")
	}
	// Until the node has moved its folders, memory under the new name is refused too: a folder of that name on the node
	// would be a leftover it sets aside, not the role's memory.
	h2 := e.commit("decisions.md", "two")
	if ack := e.syncAs(newRole, h1, h2); ack.Result != proto.MemoryError || ack.Message != ErrRoleRenamed {
		t.Fatalf("sync under the new name before the node move: %+v", ack)
	}
	// The node moved its folder, reported it, and offers what it wrote since under the new name, from the head central
	// acknowledged.
	e.exec(`UPDATE space_role_renames SET node_state = 'done' WHERE id = ?`, rn.ID)
	if ack := e.syncAs(newRole, h1, h2); ack.Result != proto.MemoryOK || ack.Head != h2 {
		t.Fatalf("sync under the new name: %+v", ack)
	}
	if got := e.collections(); len(got) != 1 || got[newRole] != h2 {
		t.Fatalf("collections after the sync %v", got)
	}
	if n := e.auditCount("memory.repository_renamed"); n != 1 {
		t.Fatalf("%d move audit events", n)
	}
}

// Acceptance 2: a move the commit was not followed by (a crash) is finished by the next access (Files, Apply) or by the
// start-up reconciler, and a finished move is not repeated.
func TestRenameMoveFinishedAfterCrash(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		finish func(e *renameEnv, h1 string)
	}{
		{"files", func(e *renameEnv, h1 string) {
			files, err := e.svc.Files(e.ctx, "spc_a", newRole)
			if err != nil || len(files) != 1 {
				e.t.Fatalf("files: %v %v", files, err)
			}
		}},
		{"history", func(e *renameEnv, h1 string) {
			if hist, err := e.svc.History(e.ctx, "spc_a", newRole, 5); err != nil || len(hist) != 1 || hist[0].ID != h1 {
				e.t.Fatalf("history: %v %v", hist, err)
			}
		}},
		{"apply", func(e *renameEnv, h1 string) {
			e.exec(`UPDATE space_role_renames SET node_state = 'done'`) // the node moved and reported it
			h2 := e.commit("b.md", "b")
			if ack := e.syncAs(newRole, h1, h2); ack.Result != proto.MemoryOK {
				e.t.Fatalf("sync: %+v", ack)
			}
		}},
		{"report", func(e *renameEnv, h1 string) {
			e.svc.Report(e.ctx, "nod_1", proto.MemoryStatus{Collections: []proto.MemoryCollectionStatus{{SpaceID: "spc_a", Role: newRole}}})
		}},
		{"start-up", func(e *renameEnv, h1 string) { must(e.t, e.svc.FinishAllRenames(e.ctx)) }},
		{"export", func(e *renameEnv, h1 string) {
			got, err := e.svc.ExportBundles(e.ctx, "spc_a", e.t.TempDir())
			if err != nil || len(got) != 1 || got[0].Role != newRole {
				e.t.Fatalf("export: %+v %v", got, err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newRenameEnv(t)
			h1 := e.commit("a.md", "a")
			if ack := e.syncAs(oldRole, "", h1); ack.Result != proto.MemoryOK {
				t.Fatalf("sync: %+v", ack)
			}
			rn := e.rename(oldRole, newRole, false)
			if !e.repoExists(oldRole) || e.repoExists(newRole) || e.moved(rn.ID) {
				t.Fatal("the simulated crash moved the repository")
			}
			c.finish(e, h1)
			if e.repoExists(oldRole) || !e.repoExists(newRole) || !e.moved(rn.ID) {
				t.Fatalf("not finished: old %v new %v recorded %v", e.repoExists(oldRole), e.repoExists(newRole), e.moved(rn.ID))
			}
			must(t, e.svc.FinishAllRenames(e.ctx))
			if !e.repoExists(newRole) || e.auditCount("memory.repository_renamed") != 1 {
				t.Fatal("a finished move ran again")
			}
		})
	}
}

// A chain a→b→c that crashed before both moves moves in order, and a rename of a role without central memory records
// its move without touching the disk.
func TestRenameChainAndNothingToMove(t *testing.T) {
	t.Parallel()
	e := newRenameEnv(t)
	h1 := e.commit("a.md", "a")
	if ack := e.syncAs(oldRole, "", h1); ack.Result != proto.MemoryOK {
		t.Fatalf("sync: %+v", ack)
	}
	first := e.rename(oldRole, "mid", false)
	e.exec(`UPDATE space_role_renames SET node_state = 'done' WHERE id = ?`, first.ID) // the node moved: the next rename may go
	second := e.rename("mid", newRole, false)
	must(t, e.svc.FinishAllRenames(e.ctx))
	if e.repoExists(oldRole) || e.repoExists("mid") || !e.repoExists(newRole) || !e.moved(first.ID) || !e.moved(second.ID) {
		t.Fatal("the chain did not move in order")
	}
	if got := e.collections(); len(got) != 1 || got[newRole] != h1 {
		t.Fatalf("collections %v", got)
	}

	e.addRole("empty")
	rn := e.rename("empty", "still-empty", true)
	if !e.moved(rn.ID) || e.repoExists("still-empty") || e.repoExists("empty") {
		t.Fatal("a rename without central memory")
	}
}

// Acceptance 3: a repository already under the new name (left on disk without a collection) is set aside under
// .retained, never merged and never deleted.
func TestRenameRetainsLeftoverRepository(t *testing.T) {
	t.Parallel()
	e := newRenameEnv(t)
	h1 := e.commit("a.md", "a")
	if ack := e.syncAs(oldRole, "", h1); ack.Result != proto.MemoryOK {
		t.Fatalf("sync: %+v", ack)
	}
	leftover := e.svc.repo("spc_a", newRole)
	must(t, e.git.InitBare(e.ctx, leftover))
	must(t, os.WriteFile(filepath.Join(leftover, "marker"), []byte("left over"), 0o600))
	rn := e.rename(oldRole, newRole, true)
	if got := e.git.RevParse(e.ctx, leftover, "refs/heads/main"); got != h1 {
		t.Fatalf("the new name's repository has %q, want the role's history %s", got, h1)
	}
	if _, err := os.Stat(filepath.Join(leftover, "marker")); err == nil {
		t.Fatal("the leftover was merged into the role's repository")
	}
	kept := filepath.Join(e.svc.dir, "spc_a", RetainedDir, newRole+"-"+rn.ID+".git")
	if b, err := os.ReadFile(filepath.Join(kept, "marker")); err != nil || string(b) != "left over" {
		t.Fatalf("the leftover was not retained at %s: %v", kept, err)
	}
	if files, err := e.svc.Files(e.ctx, "spc_a", newRole); err != nil || len(files) != 1 {
		t.Fatalf("files: %v %v", files, err)
	}
}

// Acceptance 4: a sync or a status report under the old name is refused ("role renamed") and creates no collection,
// while the node move is open and after it, unless a role has that name again once the node has moved.
func TestRenamedAwayNameIsRefused(t *testing.T) {
	t.Parallel()
	e := newRenameEnv(t)
	h1 := e.commit("a.md", "a")
	if ack := e.syncAs(oldRole, "", h1); ack.Result != proto.MemoryOK {
		t.Fatalf("sync: %+v", ack)
	}
	rn := e.rename(oldRole, newRole, true)
	h2 := e.commit("b.md", "b")
	refused := func(when string) {
		t.Helper()
		if ack := e.syncAs(oldRole, h1, h2); ack.Result != proto.MemoryError || ack.Message != ErrRoleRenamed {
			t.Fatalf("%s: sync under the old name: %+v", when, ack)
		}
		e.svc.Report(e.ctx, "nod_1", proto.MemoryStatus{Collections: []proto.MemoryCollectionStatus{{SpaceID: "spc_a", Role: oldRole, Head: h2}}})
		if got := e.collections(); len(got) != 1 || got[newRole] != h1 {
			t.Fatalf("%s: collections %v", when, got)
		}
		if e.repoExists(oldRole) {
			t.Fatalf("%s: a repository under the old name", when)
		}
	}
	refused("pending")
	// A new role of the old name while the node has not moved yet: its folder of that name is still the renamed role's.
	e.addRole(oldRole)
	refused("pending, name reused")
	e.exec(`UPDATE space_role_renames SET node_state = 'failed' WHERE id = ?`, rn.ID)
	refused("failed, name reused")
	e.exec(`DELETE FROM space_roles WHERE space_id = 'spc_a' AND name = ?`, oldRole)
	e.exec(`UPDATE space_role_renames SET node_state = 'done' WHERE id = ?`, rn.ID)
	refused("done") // a stale node, or one restored from an old backup
	// The node moved and a role has the name again: its memory is that new role's.
	e.addRole(oldRole)
	h3 := e.commit("c.md", "c")
	if ack := e.syncAs(oldRole, "", h3); ack.Result != proto.MemoryOK {
		t.Fatalf("a reused name: %+v", ack)
	}
	if got := e.collections(); len(got) != 2 || got[oldRole] != h3 || got[newRole] != h1 {
		t.Fatalf("collections %v", got)
	}
}

// Acceptance 7 (central side): a backup holding the moves keeps a rename that commits meanwhile from moving the
// repository under it. The move waits for the release, and an access in the meantime waits too, then finds it moved.
func TestHoldMovesDefersRename(t *testing.T) {
	t.Parallel()
	e := newRenameEnv(t)
	h1 := e.commit("a.md", "a")
	if ack := e.syncAs(oldRole, "", h1); ack.Result != proto.MemoryOK {
		t.Fatalf("sync: %+v", ack)
	}
	release := e.svc.HoldMoves()
	rn := e.rename(oldRole, newRole, true)
	if !e.repoExists(oldRole) || e.moved(rn.ID) {
		t.Fatal("moved while a backup held the moves")
	}
	got := make(chan error, 1)
	go func() {
		_, err := e.svc.Files(e.ctx, "spc_a", newRole)
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("an access did not wait for the release: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	release()
	release() // a second release is harmless
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if e.repoExists(oldRole) || !e.repoExists(newRole) || !e.moved(rn.ID) {
		t.Fatal("not moved after the release")
	}
}

// The guard answers per space and name: while the node move is open both names are refused.
func TestRenamedAwayIsPerSpace(t *testing.T) {
	t.Parallel()
	e := newRenameEnv(t)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`)
	e.rename(oldRole, newRole, true)
	for _, c := range []struct {
		space, role string
		want        bool
	}{{"spc_a", oldRole, true}, {"spc_a", newRole, true}, {"spc_b", oldRole, false}, {"spc_b", newRole, false}} {
		got, err := e.svc.renamedAway(e.ctx, c.space, c.role)
		if err != nil || got != c.want {
			t.Errorf("%s/%s: %v %v, want %v", c.space, c.role, got, err, c.want)
		}
	}
	// Once the node has moved, the new name is the role's, and the old one stays refused while no role has it.
	e.exec(`UPDATE space_role_renames SET node_state = 'done'`)
	for _, c := range []struct {
		role string
		want bool
	}{{oldRole, true}, {newRole, false}} {
		if got, err := e.svc.renamedAway(e.ctx, "spc_a", c.role); err != nil || got != c.want {
			t.Errorf("done, %s: %v %v, want %v", c.role, got, err, c.want)
		}
	}
}
