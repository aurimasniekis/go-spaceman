package store

import (
	"context"
	"path/filepath"
	"testing"
)

// 0072 adds event_subscriptions.ingest_own_scope and event_fleet_scope_grants (ADR-0106): a database at 0071 keeps its
// subscriptions, which get scope space (nothing widens until an administrator grants a space and a subscription opts
// in); the column takes only space and fleet; a space's grant goes with the space.
func TestFleetScopeMigrationFrom0071(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0071")})
	if err != nil {
		t.Fatal(err)
	}
	if hasColumn(ctx, t, s, "event_subscriptions", "ingest_own_scope") {
		t.Fatal("0071 already has event_subscriptions.ingest_own_scope")
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO event_connections (id, name, kind, url, creds_enc, client_id, created_at, created_by, updated_at) VALUES ('ecn_1', 'p', 'pulse', 'nats://x', x'00', 'spaceman', 0, 't', 0)`,
		`INSERT INTO event_subscriptions (id, space_id, name, connection_id, follow, custom_json, vars_json, effective_subject, durable, enabled, start_policy, created_at, created_by, updated_at, ingest_own)
		 VALUES ('sub_1', 'spc_a', 'dialogs', 'ecn_1', 'pinned', '{}', '{}', 'evt.spaceman.fleet.run.dialog_detected', 'sm-1', 1, 'new', 0, 't', 0, 1)`,
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
	if err != nil || cur != latest || latest < 72 {
		t.Fatalf("after the upgrade: version %d of %d, %v; want all applied, at least 0072", cur, latest, err)
	}
	var scope string
	var own int64
	if err := s.rdb.QueryRowContext(ctx, `SELECT ingest_own_scope, ingest_own FROM event_subscriptions WHERE id = 'sub_1'`).Scan(&scope, &own); err != nil {
		t.Fatal(err)
	}
	if scope != "space" || own != 1 {
		t.Errorf("existing subscription: scope %q ingest_own %d, want space and 1", scope, own)
	}
	if _, err := s.wdb.ExecContext(ctx, `UPDATE event_subscriptions SET ingest_own_scope = 'all' WHERE id = 'sub_1'`); err == nil {
		t.Error("ingest_own_scope accepted all")
	}
	if _, err := s.wdb.ExecContext(ctx, `UPDATE event_subscriptions SET ingest_own_scope = 'fleet' WHERE id = 'sub_1'`); err != nil {
		t.Errorf("ingest_own_scope refused fleet: %v", err)
	}
	var grants int
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM event_fleet_scope_grants`).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("grants after the upgrade: %d %v, want none", grants, err)
	}
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO event_fleet_scope_grants (space_id, granted_at, granted_by) VALUES ('spc_missing', 0, 't')`); err == nil {
		t.Error("a grant for a missing space was accepted")
	}
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO event_fleet_scope_grants (space_id, granted_at, granted_by) VALUES ('spc_a', 0, 't')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.wdb.ExecContext(ctx, `DELETE FROM event_subscriptions; DELETE FROM spaces WHERE id = 'spc_a'`); err != nil {
		t.Fatal(err)
	}
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM event_fleet_scope_grants`).Scan(&grants); err != nil || grants != 0 {
		t.Errorf("grants after the space was purged: %d %v, want none", grants, err)
	}
}
