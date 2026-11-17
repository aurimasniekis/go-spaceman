//go:build unix

package agent

import (
	"io/fs"
	"syscall"
)

// linkCount is the number of hard links to a file, or 1 where the platform does not say.
func linkCount(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) //nolint:unconvert // the field's width differs per platform
	}
	return 1
}
