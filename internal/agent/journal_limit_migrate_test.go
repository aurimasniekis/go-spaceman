package agent

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// Journal migration 0008 (ADR-0079) adds a run's usage limit and transcript scan offset: an existing run has none and
// offset 0, and the migration goes down and up again without touching the run.
func TestJournalUsageLimitMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, d, sub, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO runs (run_id, space_id, generation, kind, role, harness, instance_id, terminal_name, desired, state, created_at, updated_at)
		VALUES ('run_1', 'spc_1', 1, 'worker', 'w', 'claude-code', 'ins_1', 'sm-run_1', 'running', 'running', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		var kind string
		var resets, since sql.NullInt64
		var off int64
		if err := d.QueryRowContext(ctx, `SELECT limit_kind, limit_resets_at, limit_since, limit_offset FROM runs WHERE run_id = 'run_1'`).Scan(&kind, &resets, &since, &off); err != nil ||
			kind != "" || resets.Valid || since.Valid || off != 0 {
			t.Fatalf("existing run: %q %v %v %d %v", kind, resets, since, off, err)
		}
	}
	if _, err := p.UpTo(ctx, 8); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err := d.ExecContext(ctx, `UPDATE runs SET limit_kind = 'five_hour', limit_resets_at = 2, limit_since = 1, limit_offset = 4096 WHERE run_id = 'run_1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `SELECT limit_offset FROM runs`); err == nil {
		t.Fatal("down kept limit_offset")
	}
	var n int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE run_id = 'run_1' AND state = 'running'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("run after down: %d %v", n, err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	check()
	if v, err := p.GetDBVersion(ctx); err != nil || v < 8 {
		t.Fatalf("version %d %v", v, err)
	}
}
