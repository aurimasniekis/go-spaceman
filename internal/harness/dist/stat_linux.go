package dist

import (
	"os"
	"syscall"
)

// statFingerprint is fi's identity and change time. The kernel sets ctime on every change to the file's content or
// metadata (a write, chmod, a rename over it gives another inode), and no unprivileged call can set it back.
func statFingerprint(fi os.FileInfo) (fingerprint, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fingerprint{}, false
	}
	return fingerprint{dev: st.Dev, ino: st.Ino, size: fi.Size(), mode: fi.Mode(), ctimeSec: int64(st.Ctim.Sec), ctimeNsec: int64(st.Ctim.Nsec)}, true //nolint:unconvert // int32 on 32-bit platforms
}
