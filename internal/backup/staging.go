package backup

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// stagingGrace is how old a staging directory must be before it counts as orphaned. Create stages every backup in
// a directory next to its destination; a crash or hard kill leaves it behind. The age is the directory's own
// filesystem mtime against the wall clock (not the service clock), and only covers the moment between the directory
// being created and Create locking it. What protects a backup another process (the offline CLI) is writing into the
// same folder is its flock on the directory (see stageLock): the sweep skips a directory it cannot lock, however old.
const stagingGrace = time.Hour

// reStaging is exactly the name os.MkdirTemp gives the staging directory of a backup (".<id>.tmp-<digits>").
var reStaging = regexp.MustCompile(`^\.bkp_[0-9a-z]{26}\.tmp-[0-9]+$`)

// reStaged is the name a backup has in the staging folder of a remote target: Create renames the staging directory to
// it before the upload. On a local target that name is the finished backup, so it is only swept for remote targets.
var reStaged = regexp.MustCompile(`^bkp_[0-9a-z]{26}$`)

func isStagingName(name string, remote bool) bool {
	return reStaging.MatchString(name) || remote && reStaged.MatchString(name)
}

// SweepStaging removes the staging directories a crashed backup left in the target's staging folder, and records
// what it removed in the log and the audit trail. It does nothing while a backup or verification runs. Only real
// directories whose name matches the staging pattern exactly (on remote targets also the renamed bkp_<id> directory),
// that are older than stagingGrace and that no running backup holds the lock of are removed; a symlink with such a
// name is never followed (it is left alone).
func (s *Service) SweepStaging(ctx context.Context) (int, error) {
	if !s.claim() {
		return 0, nil
	}
	defer s.release()
	return s.sweepStaging(ctx)
}

// sweepStaging is SweepStaging for a caller that holds the busy claim.
func (s *Service) sweepStaging(ctx context.Context) (int, error) {
	dir := s.o.Target.StagingDir()
	des, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	remote := s.o.Target.Kind() != "local"
	now := time.Now()
	var removed []string
	var firstErr error
	for _, d := range des {
		// d.Type comes from lstat: a symlink is not a directory here.
		if !d.Type().IsDir() || !isStagingName(d.Name(), remote) {
			continue
		}
		info, err := d.Info()
		if err != nil || now.Sub(info.ModTime()) < stagingGrace {
			continue
		}
		p := filepath.Join(dir, d.Name())
		lock, busy, err := lockStageDir(p, false)
		if busy {
			continue // a running backup (possibly in another process) holds it
		}
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		err = os.RemoveAll(p)
		lock.release()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed = append(removed, d.Name())
	}
	if len(removed) == 0 {
		return 0, firstErr
	}
	_ = store.FsyncDir(dir)
	s.o.Log.Warn("removed orphaned backup staging directories left by an interrupted backup", slog.Int("count", len(removed)))
	if err := s.st.W().TxNamed(ctx, "backup.staging_swept", func(tx *store.Tx) error {
		_, err := s.audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "backup.staging_swept", Target: audit.Target{Type: "backup", ID: ""},
			Details: map[string]any{"count": len(removed), "names": removed}})
		return err
	}); err != nil {
		s.o.Log.Warn("record swept backup staging directories", obs.Err(err))
	}
	return len(removed), firstErr
}
