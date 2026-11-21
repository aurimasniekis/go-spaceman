//go:build unix

package agent

import (
	"io/fs"
	"syscall"
)

// fileIdentity is a file's inode and change time (ns), or zeros where the platform does not say. A same-size rewrite that
// keeps the modification time (cp -p, rsync -a) still changes the change time and, for a replace, the inode.
func fileIdentity(fi fs.FileInfo) (ino uint64, ctimeNs int64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino), statCtimeNs(st) //nolint:unconvert // the field's width differs per platform
	}
	return 0, 0
}

// linkCount is the number of hard links to a file, or 1 where the platform does not say.
func linkCount(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) //nolint:unconvert // the field's width differs per platform
	}
	return 1
}
