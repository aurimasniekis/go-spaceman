//go:build unix

package backup

import (
	"errors"
	"os"
	"syscall"
)

// stageLock is an exclusive advisory flock on a staging directory. Create holds it from the moment the directory
// exists until the caller has finished with the backup (Result.Close), so a sweep in another process can tell a live
// staging directory from one a crashed backup left behind: the kernel drops the lock when the process dies. The lock
// is on the directory's inode, so it survives the rename of the directory to its destination.
type stageLock struct{ f *os.File }

// lockStageDir opens dir (never following a symlink) and takes the lock, waiting for it only when block is set.
// busy is true when another holder has it.
func lockStageDir(dir string, block bool) (l *stageLock, busy bool, err error) {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0) //nolint:gosec // G304: a checked staging name
	if err != nil {
		return nil, false, err
	}
	how := syscall.LOCK_EX
	if !block {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil { //nolint:gosec // fd fits int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return &stageLock{f: f}, false, nil
}

func (l *stageLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN) //nolint:gosec // fd fits int
	_ = l.f.Close()
	l.f = nil
}
