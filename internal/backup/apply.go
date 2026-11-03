package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// SettingRestored holds the RestoreState of the last applied restore.
const SettingRestored = "backup.restored"

// LeaseGenerationStep is added to every space's lease generation when a restore is applied. Generations only grow
// (ADR-0006), and the source installation may have granted new generations after the backup was taken; the step puts
// the restored installation's next grant above anything the source plausibly issued, so commands and reports from
// before the restore are fenced rather than mistaken for current ones (SM-STORE-006).
const LeaseGenerationStep = 1_000_000

// RestoreReason is recorded on everything a restore stops, cancels or revokes.
const RestoreReason = "restored from a backup: execution authority was renewed"

// RestoreState describes an applied restore (SM-STORE-006). It is kept in the settings table and shown to operators
// until acknowledged.
type RestoreState struct {
	BackupID        string    `json:"backup_id"`
	BackupCreatedAt time.Time `json:"backup_created_at"`
	RestoredAt      time.Time `json:"restored_at"`
	AppliedAt       time.Time `json:"applied_at"`
	// PausedSpaces are the spaces the restore paused (active at backup time). Each needs an explicit resume.
	PausedSpaces []string `json:"paused_spaces"`
	// LeasesRenewed spaces got a revoked lease LeaseGenerationStep generations ahead.
	LeasesRenewed int64 `json:"leases_renewed"`
	// RunsUnknown were active at backup time; they are marked unknown and stopped until their node reconciles.
	RunsUnknown int64 `json:"runs_unknown"`
	// CommandsCancelled were queued for nodes and not acknowledged.
	CommandsCancelled int64 `json:"commands_cancelled"`
	// RunCredentialsRevoked are agent tool credentials of runs from before the restore.
	RunCredentialsRevoked int64 `json:"run_credentials_revoked"`
	// SubscriptionsReset replay from their oldest stored delivery (or their creation) on the next start.
	SubscriptionsReset int64 `json:"subscriptions_reset"`
	// ApprovalsRevoked were approved with uses left; their uses after the backup are unknown.
	ApprovalsRevoked int64 `json:"approvals_revoked"`
	// PendingApprovals stay pending with their human actions; approver and expiry are checked again before any use.
	PendingApprovals int64 `json:"pending_approvals"`
	// OpenHumanActions are preserved as they were.
	OpenHumanActions int64 `json:"open_human_actions"`
	// OutboxPending events were recorded but not published at backup time; they are published again (at least once).
	OutboxPending  int64      `json:"outbox_pending"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	AcknowledgedBy string     `json:"acknowledged_by,omitempty"`
}

// ApplyPendingRestore applies the post-restore rules once, on the first start after Restore (SM-STORE-006), in one
// write transaction: every active space is paused (hold); every space's lease is revoked and its generation advanced by
// LeaseGenerationStep, so the next grant fences everything issued before; runs that were active are marked unknown
// with desired state stopped; unacknowledged node commands are cancelled and run credentials revoked; every event
// subscription gets a new cursor that replays from its oldest stored delivery (never skipping an event, ingestion
// deduplicates); approvals that were approved with uses left are revoked (their use after the backup is unknown),
// while pending approvals and open human actions are kept; backup runs in progress are marked interrupted. The state is
// recorded under SettingRestored and audited. It returns nil when no restore is pending.
func ApplyPendingRestore(ctx context.Context, st *store.Store, aw *audit.Writer, ch *changefeed.Recorder) (*RestoreState, error) {
	pend, err := st.R().GetSetting(ctx, SettingRestorePending)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var p PendingRestore
	if err := json.Unmarshal([]byte(pend.Value), &p); err != nil {
		return nil, fmt.Errorf("pending restore marker: %w", err)
	}
	var rs *RestoreState
	err = st.W().TxNamed(ctx, "backup.apply_restore", func(tx *store.Tx) error {
		rs = &RestoreState{BackupID: p.BackupID, BackupCreatedAt: p.BackupCreatedAt, RestoredAt: p.RestoredAt, AppliedAt: tx.Now, PausedSpaces: []string{}}
		now := tx.NowMicros()
		exec := func(q string, args ...any) (int64, error) {
			r, err := tx.SQL.ExecContext(ctx, q, args...)
			if err != nil {
				return 0, err
			}
			return r.RowsAffected()
		}
		count := func(q string) int64 {
			var n int64
			_ = tx.SQL.QueryRowContext(ctx, q).Scan(&n)
			return n
		}
		if rs.PausedSpaces, err = activeSpaces(ctx, tx); err != nil {
			return err
		}
		var ignored int64
		// Each statement's affected rows go to its counter, in order, within the one transaction.
		for _, st := range []struct {
			n    *int64
			sql  string
			args []any
		}{
			{&ignored, `UPDATE spaces SET state = 'paused', pause_mode = 'hold', version = version + 1, updated_at = ? WHERE state = 'active'`, []any{now}},
			{&rs.LeasesRenewed, `INSERT INTO execution_leases (space_id, node_id, generation, state, granted_at, renewed_at, expires_at)
				SELECT s.id, s.node_id, coalesce(l.generation, 0) + ?, 'revoked', ?, ?, ? FROM spaces s LEFT JOIN execution_leases l ON l.space_id = s.id WHERE true
				ON CONFLICT (space_id) DO UPDATE SET generation = excluded.generation, state = 'revoked', renewed_at = excluded.renewed_at,
				expires_at = excluded.expires_at`, []any{LeaseGenerationStep, now, now, now}},
			{&rs.RunsUnknown, `UPDATE runs SET observed = 'unknown', version = version + 1, updated_at = ?
				WHERE observed IN ('pending', 'starting', 'running', 'stopping')`, []any{now}},
			{&ignored, `UPDATE runs SET desired = 'stopped', version = version + 1, updated_at = ? WHERE desired = 'running'`, []any{now}},
			{&rs.CommandsCancelled, `UPDATE node_commands SET state = 'cancelled', reason = ? WHERE state IN ('pending', 'sent')`, []any{RestoreReason}},
			{&rs.RunCredentialsRevoked, `UPDATE run_credentials SET revoked_at = ? WHERE revoked_at IS NULL`, []any{now}},
			// Replay from the oldest delivery still stored (or from the subscription's creation when none is): events the
			// source handled after the backup are seen again, and deduplication drops the ones the backup already holds.
			{&rs.SubscriptionsReset, `UPDATE event_subscriptions SET
				start_policy = CASE WHEN (SELECT min(stream_seq) FROM event_deliveries d WHERE d.subscription_id = event_subscriptions.id AND d.stream_seq > 0) IS NULL
					THEN 'time' ELSE 'seq' END,
				start_seq = (SELECT min(stream_seq) FROM event_deliveries d WHERE d.subscription_id = event_subscriptions.id AND d.stream_seq > 0),
				start_time = CASE WHEN (SELECT min(stream_seq) FROM event_deliveries d WHERE d.subscription_id = event_subscriptions.id AND d.stream_seq > 0) IS NULL
					THEN created_at ELSE NULL END,
				replay = 0, cursor_generation = cursor_generation + 1, version = version + 1, updated_at = ?`, []any{now}},
			{&rs.ApprovalsRevoked, `UPDATE approvals SET state = 'revoked', state_reason = ?, updated_at = ? WHERE state = 'approved' AND uses_left > 0`,
				[]any{"restored from a backup: uses after the backup are unknown; request the approval again", now}},
			{&ignored, `UPDATE backups SET state = CASE WHEN id = ? THEN 'succeeded' ELSE 'interrupted' END, finished_at = coalesce(finished_at, ?),
				error = CASE WHEN id = ? THEN error ELSE 'interrupted by a restore' END WHERE state = 'running'`, []any{p.BackupID, now, p.BackupID}},
		} {
			if *st.n, err = exec(st.sql, st.args...); err != nil {
				return err
			}
		}
		rs.PendingApprovals = count(`SELECT count(*) FROM approvals WHERE state = 'pending'`)
		rs.OpenHumanActions = count(`SELECT count(*) FROM human_actions WHERE status IN ('open', 'in_progress')`)
		rs.OutboxPending = count(`SELECT count(*) FROM event_outbox WHERE published_at IS NULL AND dead_at IS NULL`)
		for _, id := range rs.PausedSpaces {
			if err := ch.Record(ctx, tx, id, "space", id, changefeed.Updated, 0); err != nil {
				return err
			}
		}
		if err := ch.Record(ctx, tx, "", "restore", p.BackupID, changefeed.Updated, 0); err != nil {
			return err
		}
		b, err := json.Marshal(rs)
		if err != nil {
			return err
		}
		if err := tx.Q.UpsertSetting(ctx, db.UpsertSettingParams{Key: SettingRestored, Value: string(b), UpdatedAt: now,
			UpdatedBy: sql.NullString{String: "restore", Valid: true}}); err != nil {
			return err
		}
		if _, err := exec(`DELETE FROM settings WHERE key = ?`, SettingRestorePending); err != nil {
			return err
		}
		_, err = aw.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "backup.restored", Target: audit.Target{Type: "backup", ID: p.BackupID},
			Details: map[string]any{"backup_created_at": p.BackupCreatedAt.Format(time.RFC3339), "restored_at": p.RestoredAt.Format(time.RFC3339),
				"spaces_paused": len(rs.PausedSpaces), "leases_renewed": rs.LeasesRenewed, "generation_step": LeaseGenerationStep,
				"runs_unknown": rs.RunsUnknown, "commands_cancelled": rs.CommandsCancelled, "run_credentials_revoked": rs.RunCredentialsRevoked,
				"subscriptions_reset": rs.SubscriptionsReset, "approvals_revoked": rs.ApprovalsRevoked, "pending_approvals": rs.PendingApprovals,
				"open_human_actions": rs.OpenHumanActions, "outbox_pending": rs.OutboxPending}})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("apply the restore: %w", err)
	}
	return rs, nil
}

func activeSpaces(ctx context.Context, tx *store.Tx) ([]string, error) {
	rows, err := tx.SQL.QueryContext(ctx, `SELECT id FROM spaces WHERE state = 'active' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LoadRestoreState returns the last applied restore, or nil.
func LoadRestoreState(ctx context.Context, q *db.Queries) (*RestoreState, error) {
	s, err := q.GetSetting(ctx, SettingRestored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var rs RestoreState
	if err := json.Unmarshal([]byte(s.Value), &rs); err != nil {
		return nil, err
	}
	return &rs, nil
}

// AcknowledgeRestore records that an operator has seen the restore notice (the spaces stay as they are).
func AcknowledgeRestore(ctx context.Context, st *store.Store, aw *audit.Writer, ch *changefeed.Recorder, actor audit.Actor) (*RestoreState, error) {
	var rs *RestoreState
	err := st.W().TxNamed(ctx, "backup.ack_restore", func(tx *store.Tx) error {
		cur, err := LoadRestoreState(ctx, tx.Q)
		if err != nil || cur == nil {
			return err
		}
		if cur.AcknowledgedAt == nil {
			t := tx.Now
			cur.AcknowledgedAt, cur.AcknowledgedBy = &t, actor.ID
			b, err := json.Marshal(cur)
			if err != nil {
				return err
			}
			if err := tx.Q.UpsertSetting(ctx, db.UpsertSettingParams{Key: SettingRestored, Value: string(b), UpdatedAt: tx.NowMicros(),
				UpdatedBy: sql.NullString{String: actor.ID, Valid: true}}); err != nil {
				return err
			}
			if err := ch.Record(ctx, tx, "", "restore", cur.BackupID, changefeed.Updated, 0); err != nil {
				return err
			}
			if _, err := aw.Append(ctx, tx, audit.Event{Actor: actor, Action: "backup.restore_acknowledged", Target: audit.Target{Type: "backup", ID: cur.BackupID}}); err != nil {
				return err
			}
		}
		rs = cur
		return nil
	})
	return rs, err
}
