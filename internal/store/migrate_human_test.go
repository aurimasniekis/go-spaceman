package store

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// embeddedUpTo returns the embedded migrations numbered up to max.
func embeddedUpTo(t *testing.T, max string) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(EmbeddedMigrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name()[:4] > max {
			continue
		}
		b, err := fs.ReadFile(EmbeddedMigrations(), e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = &fstest.MapFile{Data: b}
	}
	return out
}

// 0013 rebuilds human_actions (SQLite cannot alter a CHECK constraint): existing actions, their comments and
// notifications survive with their foreign keys intact, and foreign keys are enforced again afterwards.
func TestHumanTypesMigrationKeepsData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0012")})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO human_actions (id, space_id, type, title, requester_kind, requester_run, rank, status, outcome, created_at, created_by, updated_at)
		 VALUES ('hum_1', 'spc_a', 'question', 'Which?', 'agent', 'run_1', 1, 'resolved', 'answered', 0, 't', 0)`,
		`INSERT INTO human_comments (id, action_id, body, author, created_at) VALUES ('cmt_1', 'hum_1', 'hello', 'usr', 0)`,
		`INSERT INTO human_notifications (id, action_id, space_id, target_run, kind, created_at) VALUES ('hnt_1', 'hum_1', 'spc_a', 'run_1', 'resolved', 0)`,
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = s.Close()

	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var title, outcome string
	if err := s.rdb.QueryRowContext(ctx, `SELECT title, outcome FROM human_actions WHERE id = 'hum_1'`).Scan(&title, &outcome); err != nil || title != "Which?" || outcome != "answered" {
		t.Fatalf("action after migration: %q %q %v", title, outcome, err)
	}
	var n int
	if err := s.rdb.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM human_comments) + (SELECT count(*) FROM human_notifications)`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("children after migration: %d %v", n, err)
	}
	var violations int
	if err := s.wdb.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign key violations after the rebuild: %d %v", violations, err)
	}
	var fk int
	if err := s.wdb.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign keys not re-enabled: %d %v", fk, err)
	}
	// Enforced again: a comment for a missing action is refused; deleting the action cascades.
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO human_comments (id, action_id, body, author, created_at) VALUES ('cmt_2', 'hum_missing', 'x', 'u', 0)`); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("foreign key not enforced: %v", err)
	}
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO human_actions (id, space_id, type, title, requester_kind, rank, created_at, created_by, updated_at)
		VALUES ('hum_2', 'spc_a', 'custom', 'Deploy window', 'operator', 2, 0, 't', 0)`); err != nil {
		t.Fatalf("custom type refused: %v", err)
	}
}

// 0024 adds reminders, escalation, routing and policies: existing actions and notifications keep their data, a
// notification keeps its requester run as both target and original run, and pending actions have no schedule until
// central recomputes it (human.Service.Recover).
func TestHumanFollowupMigrationKeepsData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0023")})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO human_actions (id, space_id, type, title, requester_kind, requester_run, rank, status, outcome, priority, created_at, created_by, updated_at)
		 VALUES ('hum_1', 'spc_a', 'question', 'Which?', 'agent', 'run_1', 1, 'resolved', 'answered', 'high', 5, 't', 6)`,
		`INSERT INTO human_actions (id, space_id, type, title, requester_kind, rank, created_at, created_by, updated_at)
		 VALUES ('hum_2', 'spc_a', 'todo', 'Open one', 'operator', 2, 7, 't', 7)`,
		`INSERT INTO human_notifications (id, action_id, space_id, target_run, target_role, kind, state, created_at) VALUES ('hnt_1', 'hum_1', 'spc_a', 'run_1', 'dev', 'resolved', 'delivered', 8)`,
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = s.Close()
	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var title, priority string
	var reminders int
	var next any
	if err := s.rdb.QueryRowContext(ctx, `SELECT title, priority, reminder_count, next_reminder_at FROM human_actions WHERE id = 'hum_1'`).Scan(&title, &priority, &reminders, &next); err != nil ||
		title != "Which?" || priority != "high" || reminders != 0 || next != nil {
		t.Fatalf("action after migration: %q %q %d %v %v", title, priority, reminders, next, err)
	}
	var kind, run, orig, role, state string
	if err := s.rdb.QueryRowContext(ctx, `SELECT target_kind, target_run, original_run, target_role, state FROM human_notifications WHERE id = 'hnt_1'`).Scan(&kind, &run, &orig, &role, &state); err != nil ||
		kind != "run" || run != "run_1" || orig != "run_1" || role != "dev" || state != "delivered" {
		t.Fatalf("notification after migration: %s %s %s %s %s %v", kind, run, orig, role, state, err)
	}
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO human_policies (id, space_id, action_type, created_at, created_by, updated_at, updated_by) VALUES ('hpl_1', NULL, 'all', 0, 't', 0, 't')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO human_policies (id, space_id, action_type, created_at, created_by, updated_at, updated_by) VALUES ('hpl_2', NULL, 'all', 0, 't', 0, 't')`); err == nil {
		t.Fatal("two installation defaults for one type")
	}
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO human_notifications (id, action_id, space_id, target_kind, kind, created_at) VALUES ('hnt_2', 'hum_1', 'spc_a', 'everyone', 'resolved', 0)`); err == nil {
		t.Fatal("unknown target kind accepted")
	}
}
