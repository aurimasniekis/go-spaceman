//go:build linux

package hptest

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// The gateway runs in its own process group with a parent-death signal, and stopping it signals the whole group.
func TestProcessGroupAndParentDeathSignal(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec,noctx // the test binary
	cmd.Env = append(os.Environ(), fakeServeEnv+"=sleep")
	setProcAttr(cmd)
	if a := cmd.SysProcAttr; a == nil || !a.Setpgid || a.Pdeathsig != syscall.SIGTERM {
		t.Fatalf("SysProcAttr %+v", a)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err != nil || pgid != cmd.Process.Pid {
		t.Fatalf("pgid %d %v, pid %d", pgid, err, cmd.Process.Pid)
	}
	signalGroup(cmd, syscall.SIGKILL)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the child exited cleanly instead of by the signal")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the group signal did not stop the child")
	}
}
