//go:build linux

package hptest

import (
	"os/exec"
	"syscall"
)

// setProcAttr puts the gateway in its own process group and has the kernel send it SIGTERM when the test binary dies,
// which covers a `go test -timeout` panic or a SIGKILL, where t.Cleanup never runs. Pdeathsig follows the thread that
// started the child; the Go runtime does not end threads of goroutines that aren't locked to one, so in practice that is
// the life of the test binary.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}
