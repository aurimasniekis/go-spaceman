package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 0025 rebuilds inbox_items to allow scheduled items and adds delivery origins: existing items (with their dedup
// index), deliveries and KB sessions survive with their data, older deliveries count as live first attempts, and
// foreign keys are enforced again afterwards.
func TestLimitsSchedulesMigrationKeepsData(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0024")})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO inbox_items (id, space_id, source_kind, title, content_type, body, dedup_key, trusted_json, status, received_at, created_by, updated_at, version)
		 VALUES ('inb_1', 'spc_a', 'event', 'Issue', 'text/markdown', 'body', 'sub_1:evt_1', '{"event_id":"evt_1"}', 'in_progress', 5, 'system', 6, 3)`,
		`INSERT INTO event_connections (id, name, kind, url, creds_enc, client_id, created_at, created_by, updated_at) VALUES ('ecn_1', 'c', 'pulse', 'nats://x', x'00', 'c', 0, 't', 0)`,
		`INSERT INTO event_subscriptions (id, space_id, name, connection_id, custom_json, effective_subject, durable, created_at, created_by, updated_at)
		 VALUES ('sub_1', 'spc_a', 's', 'ecn_1', '{}', 'evt.x', 'sm-1', 0, 't', 0)`,
		`INSERT INTO event_deliveries (subscription_id, event_id, subject, outcome, inbox_item_id, at) VALUES ('sub_1', 'evt_1', 'evt.x', 'accepted', 'inb_1', 7)`,
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
	var title, status, dedup, trusted string
	var version int64
	if err := s.rdb.QueryRowContext(ctx, `SELECT title, status, dedup_key, trusted_json, version FROM inbox_items WHERE id = 'inb_1'`).Scan(&title, &status, &dedup, &trusted, &version); err != nil ||
		title != "Issue" || status != "in_progress" || dedup != "sub_1:evt_1" || !strings.Contains(trusted, "evt_1") || version != 3 {
		t.Fatalf("item after migration: %q %q %q %q %d %v", title, status, dedup, trusted, version, err)
	}
	var origin string
	var attempt int64
	if err := s.rdb.QueryRowContext(ctx, `SELECT origin, attempt FROM event_deliveries WHERE event_id = 'evt_1'`).Scan(&origin, &attempt); err != nil || origin != "live" || attempt != 1 {
		t.Fatalf("delivery after migration: %q %d %v", origin, attempt, err)
	}
	// Scheduled items are allowed now; the dedup index still holds.
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO inbox_items (id, space_id, source_kind, title, content_type, dedup_key, received_at, created_by, updated_at)
		VALUES ('inb_2', 'spc_a', 'schedule', 'Weekly', 'text/markdown', 'schedule:sch_1:1', 0, 'system', 0)`); err != nil {
		t.Fatalf("scheduled item: %v", err)
	}
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO inbox_items (id, space_id, source_kind, title, content_type, dedup_key, received_at, created_by, updated_at)
		VALUES ('inb_3', 'spc_a', 'event', 'Again', 'text/markdown', 'sub_1:evt_1', 0, 'system', 0)`); err == nil {
		t.Fatal("dedup index lost")
	}
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO inbox_items (id, space_id, source_kind, title, content_type, received_at, created_by, updated_at)
		VALUES ('inb_4', 'spc_missing', 'operator', 'x', 'text/markdown', 0, 'system', 0)`); err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("foreign key not enforced: %v", err)
	}
	var violations int
	if err := s.wdb.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign key violations after the rebuild: %d %v", violations, err)
	}
}
