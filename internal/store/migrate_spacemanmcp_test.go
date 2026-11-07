package store

import (
	"context"
	"path/filepath"
	"testing"
)

// 0042 adds the SpaceMan MCP columns to a database at 0041 (ADR-0063): existing gateway servers become kind 'http'
// with no internal token and '{}' settings, existing API tokens get the empty (bearer) purpose, and the new
// constraints hold: two kinds, two purposes, and an internal token reference that must exist.
func TestSpacemanMCPMigrationFrom0041(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0041")})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, display_name, enabled, provisioned_via, created_at, created_by, updated_at) VALUES ('usr_1', 'A', 1, 'admin', 0, 't', 0)`,
		`INSERT INTO api_tokens (id, name, user_id, mac, hint, created_at, expires_at) VALUES ('atk_1', 'ci', 'usr_1', x'01', 'abcd', 0, 1)`,
		`INSERT INTO mcp_servers (id, slug, name, url, created_at, created_by, updated_at) VALUES ('mcp_1', 'tickets', 'Tickets', 'https://mcp.example.com', 0, 't', 0)`,
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var n int
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('api_tokens') WHERE name = 'purpose'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("0041 already has api_tokens.purpose: %d %v", n, err)
	}
	_ = s.Close()

	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var kind, settings, purpose string
	var token *string
	if err := s.rdb.QueryRowContext(ctx, `SELECT kind, api_token_id, spaceman_json FROM mcp_servers WHERE id = 'mcp_1'`).Scan(&kind, &token, &settings); err != nil ||
		kind != "http" || token != nil || settings != "{}" {
		t.Fatalf("existing server %q %v %q %v", kind, token, settings, err)
	}
	if err := s.rdb.QueryRowContext(ctx, `SELECT purpose FROM api_tokens WHERE id = 'atk_1'`).Scan(&purpose); err != nil || purpose != "" {
		t.Fatalf("existing token purpose %q %v", purpose, err)
	}
	good := []string{
		`INSERT INTO api_tokens (id, name, user_id, mac, hint, created_at, expires_at, purpose) VALUES ('atk_2', 'SpaceMan', 'usr_1', x'02', '', 0, 1, 'spaceman_mcp')`,
		`INSERT INTO mcp_servers (id, slug, name, url, kind, api_token_id, spaceman_json, created_at, created_by, updated_at)
			VALUES ('mcp_2', 'spaceman', 'SpaceMan', '', 'spaceman', 'atk_2', '{"preset":"admin"}', 0, 't', 0)`,
	}
	for _, q := range good {
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, q := range []string{
		`INSERT INTO api_tokens (id, name, user_id, mac, hint, created_at, expires_at, purpose) VALUES ('atk_3', 'x', 'usr_1', x'03', '', 0, 1, 'bearer')`,
		`INSERT INTO mcp_servers (id, slug, name, url, kind, created_at, created_by, updated_at) VALUES ('mcp_3', 'stdio', 'S', '', 'stdio', 0, 't', 0)`,
		`INSERT INTO mcp_servers (id, slug, name, url, kind, api_token_id, created_at, created_by, updated_at) VALUES ('mcp_4', 'ghost', 'G', '', 'spaceman', 'atk_missing', 0, 't', 0)`,
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err == nil {
			t.Fatalf("accepted: %s", q)
		}
	}
}
