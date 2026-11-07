//go:build unix && !linux

package hptest

import (
	"os/exec"
	"syscall"
)

// setProcAttr puts the gateway in its own process group. Only Linux has a parent-death signal.
func setProcAttr(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
