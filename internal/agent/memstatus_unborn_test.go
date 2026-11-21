package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// unbornHead needs positive evidence: an initialised checkout without commits is unborn; one whose branch exists (as
// a loose or a packed ref), a detached HEAD, or any checkout under an ended context is not.
func TestUnbornHeadNeedsPositiveEvidence(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	col := statusFleet(t, m, 1, 1)[0]
	sp, role := col[0], col[1]
	stopSyncs(m)
	born := m.instance(sp, role, "ins_0")
	if m.unbornHead(ctx, born) {
		t.Fatal("a checkout with commits read as unborn")
	}
	gitIn := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...) //nolint:gosec // test
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	dir := unbornInstance(t, m, sp, role, "ins_1")
	if !m.unbornHead(ctx, dir) {
		t.Fatal("an initialised checkout without commits did not read as unborn")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if m.unbornHead(cctx, dir) {
		t.Fatal("an unborn checkout read as unborn under an ended context")
	}
	// A packed entry for HEAD's branch (pointing at a commit the checkout lacks, so git cannot resolve it) is not unborn.
	head := m.git.RevParse(ctx, m.shared(sp, role), "refs/heads/main")
	if err := os.WriteFile(filepath.Join(dir, ".git", "packed-refs"), []byte("# pack-refs with: peeled fully-peeled sorted\n"+head+" refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m.unbornHead(ctx, dir) {
		t.Fatal("a branch with a packed ref read as unborn")
	}
	if !branchAbsent(filepath.Join(dir, ".git"), "refs/heads/other") || branchAbsent(filepath.Join(dir, ".git"), "refs/heads/main") {
		t.Fatal("packed-refs lookup is wrong")
	}
	// A detached HEAD is not unborn either.
	gitIn(born, "checkout", "-q", "--detach")
	if m.unbornHead(ctx, born) {
		t.Fatal("a detached HEAD read as unborn")
	}
}
