package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// 0041 adds messages.deliver_at (ADR-0065) to a database at 0040: the column is added, existing messages keep NULL (so
// none of them becomes scheduled), and the partial index on pending scheduled messages exists and is used.
func TestDeliverAtMigrationFrom0040(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0040")})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO messages (id, space_id, from_kind, to_kind, kind, subject, created_at) VALUES ('msg_1', 'spc_a', 'operator', 'control', 'message', 'stored', 1)`,
		`INSERT INTO messages (id, space_id, from_kind, to_kind, kind, subject, state, available_at, created_at) VALUES ('msg_2', 'spc_a', 'operator', 'control', 'message', 'available', 'available', 2, 2)`,
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var n int
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('messages') WHERE name = 'deliver_at'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("0040 already has deliver_at: %d %v", n, err)
	}
	_ = s.Close()

	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var typ string
	if err := s.rdb.QueryRowContext(ctx, `SELECT type FROM pragma_table_info('messages') WHERE name = 'deliver_at'`).Scan(&typ); err != nil || typ != "INTEGER" {
		t.Fatalf("deliver_at column: %q %v", typ, err)
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, deliver_at FROM messages ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var id string
		var at sql.NullInt64
		if err := rows.Scan(&id, &at); err != nil {
			t.Fatal(err)
		}
		if at.Valid {
			t.Fatalf("existing message %s has deliver_at %d", id, at.Int64)
		}
		got = append(got, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "msg_1,msg_2" {
		t.Fatalf("messages after migration: %v", got)
	}
	var sqlText string
	if err := s.rdb.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'ix_messages_scheduled' AND tbl_name = 'messages'`).Scan(&sqlText); err != nil {
		t.Fatalf("partial index: %v", err)
	}
	if !strings.Contains(sqlText, "WHERE deliver_at IS NOT NULL AND state = 'stored'") {
		t.Fatalf("index is not the partial one: %s", sqlText)
	}
	// The planner uses it for the due-reminder lookup.
	plan, err := s.rdb.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT min(deliver_at) FROM messages WHERE deliver_at IS NOT NULL AND state = 'stored' AND deliver_at > 5`)
	if err != nil {
		t.Fatal(err)
	}
	var detail strings.Builder
	for plan.Next() {
		var id, parent, notused int
		var d string
		if err := plan.Scan(&id, &parent, &notused, &d); err != nil {
			t.Fatal(err)
		}
		detail.WriteString(d + "\n")
	}
	if err := plan.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail.String(), "ix_messages_scheduled") {
		t.Fatalf("plan does not use the index:\n%s", detail.String())
	}
}
