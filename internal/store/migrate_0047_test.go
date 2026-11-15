package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// 0047 adds a run's terminal size (ADR-0075): existing runs keep the node's default (both columns NULL), and the
// CHECKs keep a size within runner.terminal_size's bounds (40-400 columns, 10-200 rows) with both columns set or
// both NULL.
func TestRunTerminalSizeMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0045")})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
			VALUES ('run_old', 'spc_a', 'nod_1', 1, 'worker', 'w', 'codex', 'ins_1', 'running', 'running', '{}', 0, 't', 0)`,
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
	var cols, rows sql.NullInt64
	if err := s.rdb.QueryRowContext(ctx, `SELECT terminal_cols, terminal_rows FROM runs WHERE id = 'run_old'`).Scan(&cols, &rows); err != nil || cols.Valid || rows.Valid {
		t.Fatalf("existing run after the migration: %v %v %v", cols, rows, err)
	}

	insert := func(i int, c, r any) error {
		_, err := s.wdb.ExecContext(ctx, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed,
			launch_json, created_at, created_by, updated_at, terminal_cols, terminal_rows)
			VALUES (?, 'spc_a', 'nod_1', 1, 'worker', 'w', 'codex', ?, 'running', 'running', '{}', 0, 't', 0, ?, ?)`,
			fmt.Sprintf("run_%d", i), fmt.Sprintf("ins_%d", i), c, r)
		return err
	}
	for i, tc := range []struct {
		cols, rows any
		ok         bool
	}{
		{nil, nil, true},
		{40, 10, true},
		{400, 200, true},
		{120, 40, true},
		{39, 40, false},
		{401, 40, false},
		{120, 9, false},
		{120, 201, false},
		{120, nil, false}, // both or neither
		{nil, 40, false},
	} {
		err := insert(i, tc.cols, tc.rows)
		if (err == nil) != tc.ok {
			t.Errorf("insert %vx%v: err %v, want ok=%v", tc.cols, tc.rows, err, tc.ok)
		}
	}
	// Updates are checked too: clearing one column alone is refused.
	if _, err := s.wdb.ExecContext(ctx, `UPDATE runs SET terminal_cols = NULL WHERE id = 'run_3'`); err == nil {
		t.Error("cleared terminal_cols alone")
	}
	if _, err := s.wdb.ExecContext(ctx, `UPDATE runs SET terminal_cols = NULL, terminal_rows = NULL WHERE id = 'run_3'`); err != nil {
		t.Errorf("clearing both: %v", err)
	}
}
