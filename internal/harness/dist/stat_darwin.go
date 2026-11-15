package dist

import (
	"os"
	"syscall"
)

// statFingerprint is fi's identity and change time (see stat_linux.go).
func statFingerprint(fi os.FileInfo) (fingerprint, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fingerprint{}, false
	}
	return fingerprint{dev: uint64(st.Dev), ino: st.Ino, size: fi.Size(), mode: fi.Mode(), ctimeSec: st.Ctimespec.Sec, ctimeNsec: st.Ctimespec.Nsec}, true //nolint:gosec // dev is non-negative
}
