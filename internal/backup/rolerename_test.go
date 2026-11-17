package backup_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// ADR-0093 T-H2 acceptance 7: a backup taken while a role rename commits succeeds. The backup holds the memory moves
// from before its database snapshot until it has bundled the repositories, so the rename that commits meanwhile moves
// the repository only afterwards, and the backup carries it under the name its snapshot gives it. Restored, the
// rename is still unmoved in the database, and the start-up reconciler finishes it on the restored disk.
func TestBackupDuringRoleRename(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if f.git == nil {
		t.Skip("git not installed")
	}
	mem := memory.New(f.st, f.aw, f.ch, f.st.Path("memory"), f.git, nil)
	oldRepo, newRepo := f.st.Path("memory", f.spaceID, "implementor.git"), f.st.Path("memory", f.spaceID, "implementor-hard.git")
	head := gitHead(t, oldRepo, "refs/heads/main")
	src := f.sources()
	held := 0
	src.HoldMemory = func() func() {
		held++
		release := mem.HoldMoves()
		// A rename commits right after the backup took the hold (before its snapshot) and tries to move the repository.
		must(t, f.st.W().TxNamed(f.ctx, "test.rename", func(tx *store.Tx) error {
			_, err := tx.SQL.ExecContext(f.ctx, `INSERT INTO space_role_renames (id, space_id, from_name, to_name, renamed_at, renamed_by, node_id, node_state)
				VALUES ('rrn_00000000000000000000000001', ?, 'implementor', 'implementor-hard', 1, 'usr_test', ?, 'done')`, f.spaceID, f.nodeID)
			return err
		}))
		mem.RenameCommitted(f.ctx, f.spaceID)
		if _, err := os.Stat(newRepo); err == nil {
			t.Error("the repository moved while the backup held the moves")
		}
		return release
	}
	out := filepath.Join(t.TempDir(), "target", "b1")
	if _, err := backup.Create(f.ctx, f.source(), f.kek, out, backup.Options{Sources: src}); err != nil {
		t.Fatalf("a backup during a rename: %v", err)
	}
	if held != 1 {
		t.Fatalf("the hold was taken %d times", held)
	}
	// Released: the source's repository moved.
	if _, err := os.Stat(oldRepo); err == nil || gitHead(t, newRepo, "refs/heads/main") != head {
		t.Fatal("the rename did not move the repository after the backup")
	}
	must(t, f.st.Close())

	target := filepath.Join(t.TempDir(), "restored")
	if _, err := backup.Restore(f.ctx, out, backup.RestoreOptions{DataDir: target, KEK: f.kek, Git: f.git}); err != nil {
		t.Fatal(err)
	}
	restoredOld := filepath.Join(target, "memory", f.spaceID, "implementor.git")
	restoredNew := filepath.Join(target, "memory", f.spaceID, "implementor-hard.git")
	if gitHead(t, restoredOld, "refs/heads/main") != head {
		t.Fatal("the backup does not hold the repository under the snapshot's name")
	}
	st, err := store.Open(f.ctx, store.Options{DataDir: target})
	must(t, err)
	defer st.Close()
	kr, err := keyring.Load(f.ctx, st, f.kek)
	must(t, err)
	rmem := memory.New(st, audit.NewWriter(kr), changefeed.New(nil), st.Path("memory"), f.git, nil)
	must(t, rmem.FinishAllRenames(f.ctx))
	if _, err := os.Stat(restoredOld); err == nil || gitHead(t, restoredNew, "refs/heads/main") != head {
		t.Fatal("the restored rename was not finished at start")
	}
}
