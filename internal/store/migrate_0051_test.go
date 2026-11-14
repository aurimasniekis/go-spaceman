package store

import (
	"context"
	"path/filepath"
	"testing"
)

// SM-TG-004 (groundwork for SM-TG-001/010/014): migration 0051 adds every Telegram table to a database at 0049 without
// touching existing rows, and its constraints hold: one bot row and one settings row, a 22-character hook id, one live
// link per Telegram user (a revoked one frees it), a redeemed code names its redeemer, and a purged space takes its
// Telegram settings and message rows with it while links only lose their current space.
func TestTelegramMigrationFrom0049(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0049")})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO users (id, display_name, enabled, provisioned_via, created_at, created_by, updated_at) VALUES ('usr_1', 'A', 1, 'admin', 0, 't', 0)`,
		`INSERT INTO role_grants (subject, role, domain, source, source_ref, created_at, created_by) VALUES ('user:usr_1', 'admin', '*', 'manual', '', 7, 't'),
			('user:usr_1#sso', 'viewer', 'spc_a', 'sso', 'eng', 8, 't')`,
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var n int
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name LIKE '%telegram%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("0049 already has Telegram tables: %d %v", n, err)
	}
	_ = s.Close()

	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.wdb.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	refused := func(what, q string, args ...any) {
		t.Helper()
		if _, err := s.wdb.ExecContext(ctx, q, args...); err == nil {
			t.Fatalf("%s was accepted", what)
		}
	}
	count := func(q string) int {
		t.Helper()
		var c int
		if err := s.rdb.QueryRowContext(ctx, q).Scan(&c); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return c
	}
	if count(`SELECT count(*) FROM spaces WHERE id = 'spc_a'`) != 1 {
		t.Fatal("space lost by the migration")
	}
	// role_grants is rebuilt to allow the 'telegram' source: rows kept as they were, the index back, other sources refused.
	if count(`SELECT count(*) FROM role_grants WHERE (subject, role, domain, source, source_ref, created_at) IN
		(VALUES ('user:usr_1', 'admin', '*', 'manual', '', 7), ('user:usr_1#sso', 'viewer', 'spc_a', 'sso', 'eng', 8))`) != 2 ||
		count(`SELECT count(*) FROM role_grants`) != 2 {
		t.Fatal("role grants changed by the migration")
	}
	if count(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'ix_role_grants_domain' AND tbl_name = 'role_grants'`) != 1 {
		t.Fatal("role_grants domain index lost")
	}
	grant := `INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES ('telegram:tgl_1', 'viewer', 'spc_a', ?, 0, 't')`
	exec(grant, "telegram")
	refused("an unknown grant source", grant, "chat")
	refused("a duplicate grant", grant, "telegram")

	bot := `INSERT INTO telegram_bot (singleton, token_enc, webhook_secret_enc, hook_id, mode, created_at, created_by, updated_at, updated_by)
		VALUES (?, x'00', x'00', ?, ?, 0, 't', 0, 't')`
	refused("a short hook id", bot, 1, "short", "webhook")
	refused("an unknown mode", bot, 1, "AAAAAAAAAAAAAAAAAAAAAA", "push")
	exec(bot, 1, "AAAAAAAAAAAAAAAAAAAAAA", "polling")
	refused("a second bot", bot, 2, "BBBBBBBBBBBBBBBBBBBBBB", "webhook")
	if count(`SELECT count(*) FROM telegram_bot WHERE enabled = 0 AND webhook_ip_allowlist_json = '[]' AND miniapp_host = ''`) != 1 {
		t.Fatal("bot defaults")
	}
	exec(`INSERT INTO telegram_settings (singleton, updated_at, updated_by) VALUES (1, 0, 't')`)
	refused("a second settings row", `INSERT INTO telegram_settings (singleton, updated_at, updated_by) VALUES (2, 0, 't')`)
	refused("invalid settings JSON", `UPDATE telegram_settings SET settings_json = '{'`)

	code := `INSERT INTO telegram_link_codes (code_hash, user_id, created_session_method, created_at, expires_at, state, redeemed_tg_user_id)
		VALUES (?, 'usr_1', 'password', 0, 600, ?, ?)`
	refused("a 16-byte code hash", code, make([]byte, 16), "new", nil)
	refused("a redeemed code without its redeemer", code, make([]byte, 32), "redeemed", nil)
	exec(code, make([]byte, 32), "redeemed", 42)

	link := `INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, state, current_space_id, revoked_at, created_at, updated_at)
		VALUES (?, 'usr_1', 7, ?, ?, ?, ?, ?, 0, 0)`
	exec(link, "tgl_1", 42, 42, "active", "spc_a", nil)
	refused("a second live link for one Telegram user", link, "tgl_2", 42, 42, "suspended", nil, nil)
	refused("a revoked link without revoked_at", link, "tgl_2", 42, 42, "revoked", nil, nil)
	exec(link, "tgl_2", 42, 42, "revoked", nil, 5)
	refused("an unknown cap", `UPDATE telegram_links SET max_role = 'root' WHERE id = 'tgl_1'`)

	exec(`INSERT INTO space_telegram_settings (space_id, enabled, created_at, created_by, updated_at, updated_by) VALUES ('spc_a', 1, 0, 't', 0, 't')`)
	refused("an unknown content level", `UPDATE space_telegram_settings SET content_level = 'everything'`)
	if count(`SELECT count(*) FROM space_telegram_settings WHERE content_level = 'full' AND enforced_approvals = 'web_only' AND terminal_watch = 0 AND topics = 0`) != 1 {
		t.Fatal("space settings defaults")
	}
	msg := `INSERT INTO telegram_messages (chat_id, message_id, link_id, space_id, kind, created_at) VALUES (42, ?, 'tgl_1', ?, 'card', 0)`
	exec(msg, 1, "spc_a")
	exec(msg, 2, nil)
	refused("a duplicate message", msg, 1, nil)

	// Purging the space removes its Telegram rows; the link stays and only forgets its current space.
	exec(`DELETE FROM spaces WHERE id = 'spc_a'`)
	if c := count(`SELECT count(*) FROM space_telegram_settings`); c != 0 {
		t.Fatalf("space settings after purge: %d", c)
	}
	if c := count(`SELECT count(*) FROM telegram_messages`); c != 1 {
		t.Fatalf("message rows after purge: %d, want the cross-space one", c)
	}
	if c := count(`SELECT count(*) FROM telegram_links WHERE id = 'tgl_1' AND current_space_id IS NULL`); c != 1 {
		t.Fatal("the link's current space was not cleared")
	}
	// Removing the link removes its message rows.
	exec(`DELETE FROM telegram_links WHERE id = 'tgl_1'`)
	if c := count(`SELECT count(*) FROM telegram_messages`); c != 0 {
		t.Fatalf("message rows after deleting the link: %d", c)
	}
}
