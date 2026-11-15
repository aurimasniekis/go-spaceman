package store

import (
	"context"
	"path/filepath"
	"testing"
)

// SM-TG-001: migration 0052 adds tg_confirmed_at (NULL) and created_session_id (”) to the link codes 0051 stored, so
// no older code is in a state the web confirmation accepts (it needs both a Telegram confirmation and its creating
// session).
func TestTelegramLinkConfirmMigrationFrom0051(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0051")})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, display_name, enabled, provisioned_via, created_at, created_by, updated_at) VALUES ('usr_1', 'A', 1, 'admin', 0, 't', 0)`,
		`INSERT INTO telegram_link_codes (code_hash, user_id, created_session_method, created_at, expires_at, state, redeemed_tg_user_id, redeemed_at)
			VALUES (zeroblob(32), 'usr_1', 'password', 0, 9000000000000000, 'redeemed', 42, 1)`,
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
	var confirmed any
	var session string
	if err := s.rdb.QueryRowContext(ctx, `SELECT tg_confirmed_at, created_session_id FROM telegram_link_codes`).Scan(&confirmed, &session); err != nil {
		t.Fatal(err)
	}
	if confirmed != nil || session != "" {
		t.Fatalf("an older code after 0052: tg_confirmed_at %v, created_session_id %q", confirmed, session)
	}
}
