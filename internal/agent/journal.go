package agent

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Journal is the node's durable execution journal (SQLite in the agent's data directory, under its own lock: one
// agent per data directory).
type Journal struct{ s *store.Store }

// OpenJournal opens (and migrates) the journal.
func OpenJournal(ctx context.Context, dir string, log *slog.Logger, clk clock.Clock) (*Journal, error) {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	s, err := store.Open(ctx, store.Options{DataDir: dir, Migrations: sub, Logger: log, Clock: clk})
	if err != nil {
		return nil, err
	}
	return &Journal{s: s}, nil
}

// Close closes the journal.
func (j *Journal) Close() error { return j.s.Close() }

// runRow is one journaled run.
type runRow struct {
	RunID, SpaceID, Kind, Role, Harness, InstanceID, TerminalName, TerminalID, CoreID, Identity string
	Generation                                                                                  int64
	PID                                                                                         int
	Desired, State, Reason, Error, SessionID, Status, StatusKind                                string
	ExitCode                                                                                    *int
	Turn                                                                                        uint64
	Adopted                                                                                     bool
	UpdatedAt                                                                                   int64
	// Bin is the harness binary the run launched (ADR-0077); written once by setRunHarness, never by updateRun.
	Bin harnessRun
}

// harnessRun is the harness binary a run launched: for managed Claude Code the cached version and its absolute path, the
// requested channel or version, and the dist fallback reason when another version than the target ran.
type harnessRun struct {
	Version, Path, Requested, Fallback string
}

const runCols = `run_id, space_id, generation, kind, role, harness, instance_id, terminal_name, terminal_id, core_id, identity, pid, desired, state,
  exit_code, reason, error, session_id, status, status_kind, turn, adopted, updated_at, harness_version, harness_path, harness_requested,
  harness_fallback`

func scanRun(sc interface{ Scan(...any) error }) (runRow, error) {
	var r runRow
	var exit sql.NullInt64
	var adopted int64
	err := sc.Scan(&r.RunID, &r.SpaceID, &r.Generation, &r.Kind, &r.Role, &r.Harness, &r.InstanceID, &r.TerminalName, &r.TerminalID, &r.CoreID,
		&r.Identity, &r.PID, &r.Desired, &r.State, &exit, &r.Reason, &r.Error, &r.SessionID, &r.Status, &r.StatusKind, &r.Turn, &adopted, &r.UpdatedAt,
		&r.Bin.Version, &r.Bin.Path, &r.Bin.Requested, &r.Bin.Fallback)
	if exit.Valid {
		v := int(exit.Int64)
		r.ExitCode = &v
	}
	r.Adopted = adopted == 1
	return r, err
}

func (r runRow) report() proto.RunReport {
	return proto.RunReport{RunID: r.RunID, SpaceID: r.SpaceID, Generation: r.Generation, State: r.State, TerminalID: r.TerminalID, CoreID: r.CoreID,
		Identity: r.Identity, PID: r.PID, ExitCode: r.ExitCode, Reason: r.Reason, Error: r.Error, Status: r.Status, StatusKind: r.StatusKind,
		Turn: r.Turn, SessionID: r.SessionID, Adopted: r.Adopted, HarnessVersion: r.Bin.Version, HarnessRequested: r.Bin.Requested,
		HarnessFallback: r.Bin.Fallback, ObservedAt: time.UnixMicro(r.UpdatedAt).UTC()}
}

func ended(state string) bool {
	return state == proto.RunExited || state == proto.RunFailed || state == proto.RunLost
}

// commandSeen records a command; it returns false when the command (by ID or idempotency key) was already received.
func (j *Journal) commandSeen(ctx context.Context, c proto.Command) (bool, error) {
	fresh := false
	err := j.s.W().TxNamed(ctx, "journal.command", func(tx *store.Tx) error {
		res, err := tx.SQL.ExecContext(ctx, `INSERT INTO commands (id, kind, space_id, run_id, generation, idempotency_key, state, received_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 'received', ?, ?) ON CONFLICT DO NOTHING`, c.CommandID, c.Kind, c.SpaceID, c.RunID, c.Generation, c.IdempotencyKey,
			tx.NowMicros(), tx.NowMicros())
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		fresh = n == 1
		return nil
	})
	return fresh, err
}

func (j *Journal) commandDone(ctx context.Context, id, state, reason string) {
	_ = j.s.W().TxNamed(ctx, "journal.command_done", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE commands SET state = ?, reason = ?, updated_at = ? WHERE id = ?`, state, reason, tx.NowMicros(), id)
		return err
	})
}

// insertRun records a launch intent before the process is started (SM-NODE-009).
func (j *Journal) insertRun(ctx context.Context, r runRow) error {
	return j.s.W().TxNamed(ctx, "journal.insert_run", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO runs (run_id, space_id, generation, kind, role, harness, instance_id, terminal_name, desired, state,
			session_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'running', ?, ?, ?, ?) ON CONFLICT (run_id) DO NOTHING`,
			r.RunID, r.SpaceID, r.Generation, r.Kind, r.Role, r.Harness, r.InstanceID, r.TerminalName, proto.RunPending, r.SessionID, tx.NowMicros(), tx.NowMicros())
		return err
	})
}

func (j *Journal) getRun(ctx context.Context, id string) (runRow, error) {
	return scanRun(j.s.ReadDB().QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE run_id = ?`, id))
}

func (j *Journal) runByTerminal(ctx context.Context, terminalID string) (runRow, error) {
	return scanRun(j.s.ReadDB().QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE terminal_id = ? AND terminal_id != ''`, terminalID))
}

// runs lists journaled runs; active only returns runs that are not ended.
func (j *Journal) runs(ctx context.Context, active bool) ([]runRow, error) {
	q := `SELECT ` + runCols + ` FROM runs`
	if active {
		q += ` WHERE state NOT IN ('exited', 'failed', 'lost')`
	}
	rows, err := j.s.ReadDB().QueryContext(ctx, q+` ORDER BY run_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []runRow
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// updateRun writes the observed state of a run.
func (j *Journal) updateRun(ctx context.Context, r runRow) error {
	return j.s.W().TxNamed(ctx, "journal.update_run", func(tx *store.Tx) error {
		var exit sql.NullInt64
		if r.ExitCode != nil {
			exit = sql.NullInt64{Int64: int64(*r.ExitCode), Valid: true}
		}
		_, err := tx.SQL.ExecContext(ctx, `UPDATE runs SET terminal_id = ?, core_id = ?, identity = ?, pid = ?, desired = ?, state = ?, exit_code = ?,
			reason = ?, error = ?, session_id = ?, status = ?, status_kind = ?, turn = ?, adopted = ?, updated_at = ? WHERE run_id = ?`,
			r.TerminalID, r.CoreID, r.Identity, r.PID, r.Desired, r.State, exit, r.Reason, r.Error, r.SessionID, r.Status, r.StatusKind, r.Turn,
			b2i(r.Adopted), tx.NowMicros(), r.RunID)
		return err
	})
}

// setRunHarness records the harness binary a run launches, before it is started.
func (j *Journal) setRunHarness(ctx context.Context, runID string, h harnessRun) error {
	return j.s.W().TxNamed(ctx, "journal.run_harness", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE runs SET harness_version = ?, harness_path = ?, harness_requested = ?, harness_fallback = ?,
			updated_at = ? WHERE run_id = ?`, h.Version, h.Path, h.Requested, h.Fallback, tx.NowMicros(), runID)
		return err
	})
}

// harnessVersionsInUse returns the harness versions of the journal's live and adoptable runs (not ended) of kind.
func (j *Journal) harnessVersionsInUse(ctx context.Context, kind string) ([]string, error) {
	rows, err := j.s.ReadDB().QueryContext(ctx, `SELECT DISTINCT harness_version FROM runs
		WHERE harness = ? AND harness_version != '' AND state NOT IN ('exited', 'failed', 'lost') ORDER BY harness_version`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// harnessRequestsSince returns what the journal's started runs of kind updated since t requested (a channel or an exact
// version, the run's profile pin or the node's channel), each with when a run requesting it was last updated. A launch
// that failed before its program started (no terminal) doesn't count.
func (j *Journal) harnessRequestsSince(ctx context.Context, kind string, t time.Time) (map[string]time.Time, error) {
	rows, err := j.s.ReadDB().QueryContext(ctx, `SELECT harness_requested, MAX(updated_at) FROM runs
		WHERE harness = ? AND harness_requested != '' AND terminal_id != '' AND updated_at >= ? GROUP BY harness_requested`, kind, t.UnixMicro())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var v string
		var at int64
		if err := rows.Scan(&v, &at); err != nil {
			return nil, err
		}
		out[v] = time.UnixMicro(at).UTC()
	}
	return out, rows.Err()
}

type leaseRow struct {
	SpaceID          string
	Generation       int64
	ExpiresAt        time.Time
	Workdir          string
	DisconnectPolicy string
	Grace            time.Duration
}

func (j *Journal) putLease(ctx context.Context, l leaseRow) error {
	return j.s.W().TxNamed(ctx, "journal.lease", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO leases (space_id, generation, expires_at, workdir, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (space_id) DO UPDATE SET generation = excluded.generation, expires_at = excluded.expires_at, workdir = excluded.workdir,
			updated_at = excluded.updated_at`, l.SpaceID, l.Generation, l.ExpiresAt.UnixMicro(), l.Workdir, tx.NowMicros())
		return err
	})
}

func (j *Journal) deleteLease(ctx context.Context, spaceID string) error {
	return j.s.W().TxNamed(ctx, "journal.lease_delete", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `DELETE FROM leases WHERE space_id = ?`, spaceID)
		return err
	})
}

func (j *Journal) leases(ctx context.Context) ([]leaseRow, error) {
	rows, err := j.s.ReadDB().QueryContext(ctx, `SELECT space_id, generation, expires_at, workdir FROM leases ORDER BY space_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []leaseRow
	for rows.Next() {
		var l leaseRow
		var exp int64
		if err := rows.Scan(&l.SpaceID, &l.Generation, &exp, &l.Workdir); err != nil {
			return nil, err
		}
		l.ExpiresAt = time.UnixMicro(exp).UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// memRow is one collection's sync state.
type memRow struct {
	SpaceID, Role, Acked, LastError string
	LastAckAt, LastCheckpoint       *time.Time
	Retries, Conflicts              int
}

func (j *Journal) memory(ctx context.Context, spaceID, role string) (memRow, error) {
	r := memRow{SpaceID: spaceID, Role: role}
	var ack, cp sql.NullInt64
	err := j.s.ReadDB().QueryRowContext(ctx, `SELECT acked, last_ack_at, last_checkpoint_at, retries, conflicts, last_error FROM memory_sync
		WHERE space_id = ? AND role = ?`, spaceID, role).Scan(&r.Acked, &ack, &cp, &r.Retries, &r.Conflicts, &r.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if ack.Valid {
		t := time.UnixMicro(ack.Int64).UTC()
		r.LastAckAt = &t
	}
	if cp.Valid {
		t := time.UnixMicro(cp.Int64).UTC()
		r.LastCheckpoint = &t
	}
	return r, err
}

func nullTime(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMicro(), Valid: true}
}

func (j *Journal) putMemory(ctx context.Context, r memRow) error {
	return j.s.W().TxNamed(ctx, "journal.memory", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO memory_sync (space_id, role, acked, last_ack_at, last_checkpoint_at, retries, conflicts, last_error, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (space_id, role) DO UPDATE SET acked = excluded.acked, last_ack_at = excluded.last_ack_at,
			last_checkpoint_at = excluded.last_checkpoint_at, retries = excluded.retries, conflicts = excluded.conflicts,
			last_error = excluded.last_error, updated_at = excluded.updated_at`,
			r.SpaceID, r.Role, r.Acked, nullTime(r.LastAckAt), nullTime(r.LastCheckpoint), r.Retries, r.Conflicts, r.LastError, tx.NowMicros())
		return err
	})
}
