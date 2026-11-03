package app_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/memory"
)

func backupItem(t *testing.T, e *fleetEnv, id string) backup.Item {
	t.Helper()
	st, err := e.c.Backups.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range st.Items {
		if x.ID == id {
			return x
		}
	}
	t.Fatalf("backup %s not listed", id)
	return backup.Item{}
}

// SM-MEM-005 (simulated: in-memory runner, real Git, real agent): before a backup, central asks the connected node to
// checkpoint and sync role memory (protocol minor 5), so a note written in a running instance and never checkpointed is
// in the backup's memory bundle; the manifest lists the space as flushed. With the node disconnected, the next backup
// still succeeds and its manifest names the space's memory as possibly stale.
func TestBackupFlushesRoleMemoryFirst(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "flushme", Name: "Flush me", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { x, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return x.Lease != nil && x.Lease.Valid })
	role := "implementor-backend"
	run, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: role, Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "run running", func() bool { return e.run1(run.ID).Observed == "running" })
	dir := filepath.Join(e.agentDir, "memory", sp.ID, role, "instances", e.run1(run.ID).InstanceID)
	eventually(t, "memory checkout", func() bool { _, err := os.Stat(filepath.Join(dir, ".git")); return err == nil })
	// The run keeps running: nothing checkpoints the note before the backup asks for it.
	const note = "- reconcile before every payout\n"
	if err := os.WriteFile(filepath.Join(dir, "payouts.md"), []byte(note), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Memory.File(ctx, sp.ID, role, "payouts.md"); err == nil {
		t.Fatal("the note reached central before the backup")
	}

	b, err := e.c.Backups.Start(ctx, admin, backup.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	e.c.Backups.Wait()
	it := backupItem(t, e, b.ID)
	if it.State != backup.StateSucceeded || it.MemoryFlush == nil || !it.MemoryFlush.Attempted || !slices.Contains(it.MemoryFlush.Flushed, sp.ID) ||
		len(it.MemoryFlush.Stale) != 0 {
		t.Fatalf("backup after a flush: %+v / %+v", it, it.MemoryFlush)
	}
	if c, err := e.c.Memory.File(ctx, sp.ID, role, "payouts.md"); err != nil || c != note {
		t.Fatalf("central memory after the flush: %q %v", c, err)
	}
	// The backup itself holds the note: restore it elsewhere and read the restored collection.
	kek, err := keyring.LoadKEK(keyring.Source{Kind: "file", File: e.cfg.KEK.File, Production: true})
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if _, err := backup.Restore(ctx, filepath.Join(e.cfg.DataDir, "backups", b.ID), backup.RestoreOptions{DataDir: restored, KEK: kek,
		Git: &memory.Git{Bin: git}}); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(ctx, git, "--git-dir", filepath.Join(restored, "memory", sp.ID, role+".git"), "show", "refs/heads/main:payouts.md").Output()
	if err != nil || string(out) != note {
		t.Fatalf("restored memory: %q %v", out, err)
	}
	var details string
	if err := e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT details_json FROM audit_events WHERE action = 'backup.finished' AND target_id = ?`, b.ID).
		Scan(&details); err != nil || !strings.Contains(details, `"memory_flushed_spaces":1`) || !strings.Contains(details, `"memory_stale_spaces":0`) {
		t.Fatalf("backup.finished details: %s %v", details, err)
	}

	// The node goes away: the backup does not wait for it and records the space's memory as possibly stale.
	e.stopAgent()
	eventually(t, "node disconnected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return !n.Connected })
	b2, err := e.c.Backups.Start(ctx, admin, backup.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	e.c.Backups.Wait()
	it = backupItem(t, e, b2.ID)
	mf := it.MemoryFlush
	if it.State != backup.StateSucceeded || mf == nil || !mf.Attempted || len(mf.Flushed) != 0 || len(mf.Stale) != 1 ||
		mf.Stale[0].SpaceID != sp.ID || mf.Stale[0].SpaceSlug != "flushme" || mf.Stale[0].Reason != fleet.FlushNodeDisconnected || mf.Stale[0].NodeID != nodes[0].ID {
		t.Fatalf("backup without the node: %+v / %+v", it, mf)
	}
	m, err := backup.ReadManifest(filepath.Join(e.cfg.DataDir, "backups", b2.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(m.Warnings, func(w string) bool { return strings.Contains(w, "role memory possibly stale for spaces flushme") }) {
		t.Fatalf("manifest warnings: %v", m.Warnings)
	}
	if rep, err := backup.Verify(ctx, filepath.Join(e.cfg.DataDir, "backups", b2.ID), backup.VerifyOptions{KEK: kek}); err != nil || !rep.OK {
		t.Fatalf("verify: %+v %v", rep, err)
	}
}
