//go:build unix

package hptest

import (
	"os/exec"
	"syscall"
)

// signalGroup signals the gateway's whole process group, so anything it started stops with it.
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		_ = cmd.Process.Signal(sig)
	}
}
