package store

import (
	"context"
	"path/filepath"
	"testing"
)

// 0070 adds event_subscriptions.ingest_own (ADR-0104): a database at 0069 keeps its subscriptions, which get 0 (own
// events stay filtered until an operator opts in); the column takes only 0 and 1.
func TestSubscriptionIngestOwnMigrationFrom0069(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0069")})
	if err != nil {
		t.Fatal(err)
	}
	if hasColumn(ctx, t, s, "event_subscriptions", "ingest_own") {
		t.Fatal("0069 already has event_subscriptions.ingest_own")
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO event_connections (id, name, kind, url, creds_enc, client_id, created_at, created_by, updated_at) VALUES ('ecn_1', 'p', 'pulse', 'nats://x', x'00', 'spaceman', 0, 't', 0)`,
		`INSERT INTO event_subscriptions (id, space_id, name, connection_id, follow, custom_json, vars_json, effective_subject, durable, enabled, start_policy, created_at, created_by, updated_at)
		 VALUES ('sub_1', 'spc_a', 'dialogs', 'ecn_1', 'pinned', '{}', '{}', 'evt.spaceman.fleet.run.dialog_detected', 'sm-1', 1, 'new', 0, 't', 0)`,
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
	cur, latest, err := SchemaVersions(ctx, s.rdb, EmbeddedMigrations())
	if err != nil || cur != latest || latest < 70 {
		t.Fatalf("after the upgrade: version %d of %d, %v; want all applied, at least 0070", cur, latest, err)
	}
	var own int64
	var subject string
	if err := s.rdb.QueryRowContext(ctx, `SELECT ingest_own, effective_subject FROM event_subscriptions WHERE id = 'sub_1'`).Scan(&own, &subject); err != nil {
		t.Fatal(err)
	}
	if own != 0 || subject != "evt.spaceman.fleet.run.dialog_detected" {
		t.Errorf("existing subscription: ingest_own %d subject %q, want 0 and the subject kept", own, subject)
	}
	if _, err := s.wdb.ExecContext(ctx, `UPDATE event_subscriptions SET ingest_own = 2 WHERE id = 'sub_1'`); err == nil {
		t.Error("ingest_own accepted 2")
	}
	if _, err := s.wdb.ExecContext(ctx, `UPDATE event_subscriptions SET ingest_own = 1 WHERE id = 'sub_1'`); err != nil {
		t.Errorf("ingest_own refused 1: %v", err)
	}
}
