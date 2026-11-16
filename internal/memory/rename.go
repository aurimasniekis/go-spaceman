package memory

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Central's half of a role rename's memory move (ADR-0093 §3.2). The rename transaction renames the collection row; the
// repository on disk, <space>/<from>.git, moves to <space>/<to>.git right after the commit (RenameCommitted). A move the
// commit was not followed by (a crash, a backup holding the moves) is finished before anything reads or writes the
// space's memory (ensureMoved) and when central starts (FinishAllRenames). The move is idempotent: the old repository
// present means move it; the old one absent means it already moved, or there was none. A repository already under the
// new name when the old one moves (left on disk without a collection) is set aside under <space>/.retained, never merged
// and never deleted.

// RetainedDir is the folder of a space's memory directory that holds repositories set aside by renames.
const RetainedDir = ".retained"

// ErrRoleRenamed is the message central answers a sync under a role name that was renamed away.
const ErrRoleRenamed = "role renamed"

// HoldMoves keeps renames from moving repositories until release is called. A backup holds it from its database
// snapshot until it has bundled the memory repositories, so the bundles sit under the names the snapshot gives them. A
// rename that commits meanwhile moves on release (or on the next access to its space's memory, which waits for the
// release).
func (svc *Service) HoldMoves() (release func()) {
	svc.moves.RLock()
	released := false
	return func() {
		if released {
			return
		}
		released = true
		svc.moves.RUnlock()
		if svc.deferred.Swap(false) {
			if err := svc.FinishAllRenames(context.Background()); err != nil {
				svc.log.Warn("finish role renames' memory moves after a backup", slog.Any("error", err))
			}
		}
	}
}

// RenameCommitted moves the repository of a rename that just committed in the space (catalog.Service.AfterRename). It
// never waits for a backup: while one holds the moves, the move is left to the backup's release and to the next access.
func (svc *Service) RenameCommitted(ctx context.Context, spaceID string) {
	if err := svc.finish(ctx, spaceID, false); err != nil {
		svc.log.Warn("move a renamed role's memory repository; it is retried on the next access", slog.String("space.id", spaceID), slog.Any("error", err))
	}
}

// FinishAllRenames finishes every rename whose central repository has not moved yet, in every space (at start, before
// nodes connect, and after a backup released the moves).
func (svc *Service) FinishAllRenames(ctx context.Context) error {
	rows, err := svc.s.R().ListUnmovedRoleRenames(ctx)
	if err != nil {
		return err
	}
	done := map[string]bool{}
	var errs []error
	for _, r := range rows {
		if done[r.SpaceID] {
			continue
		}
		done[r.SpaceID] = true
		if err := svc.finish(ctx, r.SpaceID, true); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ensureMoved finishes the space's unmoved renames before its memory is read or written. It waits while a backup holds
// the moves. An error means the space's repositories are not where the collections say: the caller must not touch them.
func (svc *Service) ensureMoved(ctx context.Context, spaceID string) error {
	return svc.finish(ctx, spaceID, true)
}

// finish moves the repositories of the space's unmoved renames, oldest first (a chain a→b→c moves a to b, then b to
// c). With wait false it gives up while a backup holds the moves, and leaves the move to the release.
func (svc *Service) finish(ctx context.Context, spaceID string, wait bool) error {
	rows, err := svc.s.R().ListUnmovedSpaceRoleRenames(ctx, spaceID)
	if err != nil || len(rows) == 0 {
		return err
	}
	if wait {
		svc.moves.Lock()
	} else {
		// Flag first, so a release that comes before TryLock fails still finishes the move.
		svc.deferred.Store(true)
		if !svc.moves.TryLock() {
			return nil
		}
	}
	defer svc.moves.Unlock()
	// Read again under the lock: another caller may have finished them meanwhile.
	if rows, err = svc.s.R().ListUnmovedSpaceRoleRenames(ctx, spaceID); err != nil {
		return err
	}
	for _, r := range rows {
		if err := svc.move(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// move moves one rename's repository under both roles' locks (taken in sorted order, as everywhere two are held) and
// records it. The caller holds svc.moves.
func (svc *Service) move(ctx context.Context, r db.SpaceRoleRename) error {
	if !ReRole.MatchString(r.FromName) || !ReRole.MatchString(r.ToName) {
		return errors.New("a role rename has an invalid name")
	}
	keys := []string{r.SpaceID + "/" + r.FromName, r.SpaceID + "/" + r.ToName}
	sort.Strings(keys)
	defer svc.lock(keys[0])()
	defer svc.lock(keys[1])()
	src, dst := svc.repo(r.SpaceID, r.FromName), svc.repo(r.SpaceID, r.ToName)
	srcOK, err := exists(src)
	if err != nil {
		return err
	}
	dstOK, err := exists(dst)
	if err != nil {
		return err
	}
	retained := ""
	if srcOK {
		if dstOK {
			retained = filepath.Join(svc.dir, r.SpaceID, RetainedDir, r.ToName+"-"+r.ID+".git")
			if err := os.MkdirAll(filepath.Dir(retained), 0o700); err != nil {
				return err
			}
			if err := os.Rename(dst, retained); err != nil {
				return err
			}
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	return svc.s.W().TxNamed(ctx, "memory.rename_moved", func(tx *store.Tx) error {
		n, err := tx.Q.SetRoleRenameCentralMoved(ctx, db.SetRoleRenameCentralMovedParams{CentralMovedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: r.ID})
		if err != nil || n == 0 || !srcOK {
			return err
		}
		details := map[string]any{"space_id": r.SpaceID, "rename_id": r.ID, "from": r.FromName, "to": r.ToName}
		if retained != "" {
			details["retained"] = filepath.ToSlash(filepath.Join(r.SpaceID, RetainedDir, filepath.Base(retained)))
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorSystem, ID: "memory"}, Action: "memory.repository_renamed",
			Target: audit.Target{Type: "space", ID: r.SpaceID}, Details: details})
		return err
	})
}

// renamedAway reports whether memory under the role name is refused because the name was renamed away: the node has
// not moved its folder of that name yet, or no role has the name again (a stale node, or one restored from an old
// backup, would otherwise start a second collection under the old name).
func (svc *Service) renamedAway(ctx context.Context, spaceID, role string) (bool, error) {
	n, err := svc.s.R().RoleNameRenamedAway(ctx, db.RoleNameRenamedAwayParams{SpaceID: spaceID, Name: role})
	return n > 0, err
}

func exists(p string) (bool, error) {
	_, err := os.Stat(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}
