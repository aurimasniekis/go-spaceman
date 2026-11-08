package backup_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// SM-STORE-006: the first start after a restore comes up paused with new execution authority, keeps unresolved human
// actions and pending approvals, revokes approvals whose later use is unknown, and does all of it exactly once.
func TestApplyPendingRestore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	out := filepath.Join(t.TempDir(), "b")
	m := f.create(out).Manifest
	must(t, f.st.Close())
	target := filepath.Join(t.TempDir(), "restored")
	_, err := backup.Restore(f.ctx, out, backup.RestoreOptions{DataDir: target, KEK: f.kek, Git: f.git})
	must(t, err)

	st := storetest.Open(t, storetest.WithDir(target))
	kr, err := keyring.Load(f.ctx, st, f.kek)
	must(t, err)
	aw := audit.NewWriter(kr)
	ch := changefeed.New(nil)
	rs, err := backup.ApplyPendingRestore(f.ctx, st, aw, ch)
	must(t, err)
	if rs == nil || rs.BackupID != m.ID || len(rs.PausedSpaces) != 1 || rs.PausedSpaces[0] != f.spaceID || rs.LeasesRenewed != 1 || rs.RunsUnknown != 1 ||
		rs.CommandsCancelled != 1 || rs.RunCredentialsRevoked != 1 || rs.ApprovalsRevoked != 1 || rs.PendingApprovals != 1 {
		t.Fatalf("restore state: %+v", rs)
	}
	var state, mode string
	must(t, st.ReadDB().QueryRowContext(f.ctx, `SELECT state, pause_mode FROM spaces WHERE id = ?`, f.spaceID).Scan(&state, &mode))
	if state != "paused" || mode != "hold" {
		t.Errorf("space %s/%s, want paused/hold", state, mode)
	}
	var gen int64
	var lstate string
	must(t, st.ReadDB().QueryRowContext(f.ctx, `SELECT generation, state FROM execution_leases WHERE space_id = ?`, f.spaceID).Scan(&gen, &lstate))
	if gen != 3+backup.LeaseGenerationStep || lstate != "revoked" {
		t.Errorf("lease generation %d %s", gen, lstate)
	}
	var observed, desired string
	must(t, st.ReadDB().QueryRowContext(f.ctx, `SELECT observed, desired FROM runs WHERE id = ?`, f.runID).Scan(&observed, &desired))
	if observed != "unknown" || desired != "stopped" {
		t.Errorf("run %s/%s", observed, desired)
	}
	var cmdState string
	must(t, st.ReadDB().QueryRowContext(f.ctx, `SELECT state FROM node_commands WHERE id = 'cmd_1'`).Scan(&cmdState))
	var revoked sql.NullInt64
	must(t, st.ReadDB().QueryRowContext(f.ctx, `SELECT revoked_at FROM run_credentials WHERE run_id = ?`, f.runID).Scan(&revoked))
	if cmdState != "cancelled" || !revoked.Valid {
		t.Errorf("command %s, credential revoked %v", cmdState, revoked.Valid)
	}
	for id, want := range map[string]string{"apv_used": "revoked", "apv_wait": "pending"} {
		var s string
		must(t, st.ReadDB().QueryRowContext(f.ctx, `SELECT state FROM approvals WHERE id = ?`, id).Scan(&s))
		if s != want {
			t.Errorf("approval %s is %s, want %s", id, s, want)
		}
	}
	var n int64
	must(t, st.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action = 'backup.restored'`).Scan(&n))
	if n != 1 {
		t.Errorf("restore audited %d times", n)
	}
	if res, err := audit.Verify(f.ctx, st.R()); err != nil || !res.OK {
		t.Errorf("audit chain after restore: %+v %v", res, err)
	}

	// Exactly once.
	again, err := backup.ApplyPendingRestore(f.ctx, st, aw, ch)
	if err != nil || again != nil {
		t.Fatalf("second apply: %+v %v", again, err)
	}
	loaded, err := backup.LoadRestoreState(f.ctx, st.R())
	if err != nil || loaded == nil || loaded.BackupID != m.ID || loaded.AcknowledgedAt != nil {
		t.Fatalf("stored state: %+v %v", loaded, err)
	}
	ack, err := backup.AcknowledgeRestore(f.ctx, st, aw, ch, audit.Actor{Type: audit.ActorUser, ID: "usr_admin"})
	if err != nil || ack == nil || ack.AcknowledgedAt == nil || ack.AcknowledgedBy != "usr_admin" {
		t.Fatalf("acknowledge: %+v %v", ack, err)
	}
	// Nothing pending on a fresh installation.
	fresh := storetest.Open(t)
	if rs, err := backup.ApplyPendingRestore(f.ctx, fresh, aw, ch); err != nil || rs != nil {
		t.Fatalf("fresh store: %+v %v", rs, err)
	}
	if rs, err := backup.LoadRestoreState(f.ctx, fresh.R()); err != nil || rs != nil {
		t.Fatalf("fresh store state: %+v %v", rs, err)
	}
	if rs, err := backup.AcknowledgeRestore(f.ctx, fresh, aw, ch, audit.System); err != nil || rs != nil {
		t.Fatalf("fresh acknowledge: %+v %v", rs, err)
	}
}
