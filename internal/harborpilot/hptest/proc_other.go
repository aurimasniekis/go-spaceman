//go:build !unix

package hptest

import (
	"os/exec"
	"syscall"
)

func setProcAttr(*exec.Cmd) {}

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if sig == syscall.SIGKILL {
		_ = cmd.Process.Kill()
		return
	}
	_ = cmd.Process.Signal(sig)
}
