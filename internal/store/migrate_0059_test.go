package store

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"
)

// The Telegram thread migration is 0059: it runs after account switching's 0054-0058. A database at 0058 (with the
// account-switching tables and a card row from before) migrates to 0059, keeps its rows and gets the nullable
// message_thread_id, NULL for the old card. A fresh database migrates through every migration to the latest, 0059.
func TestTelegramThreadMigrationFrom0058(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0058")})
	if err != nil {
		t.Fatal(err)
	}
	cur, _, err := SchemaVersions(ctx, s.rdb, embeddedUpTo(t, "0058"))
	if err != nil || cur != 58 {
		t.Fatalf("base version %d, %v; want 58", cur, err)
	}
	var n int
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN
		('account_switches', 'usage_ledger_marks', 'pool_exhausted_notices')`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("account-switching tables at the base: %d %v", n, err)
	}
	if hasColumn(ctx, t, s, "telegram_messages", "message_thread_id") {
		t.Fatal("0058 already has telegram_messages.message_thread_id")
	}
	for _, q := range []string{
		`INSERT INTO users (id, display_name, enabled, provisioned_via, created_at, created_by, updated_at) VALUES ('usr_1', 'A', 1, 'admin', 0, 't', 0)`,
		`INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, state, created_at, updated_at)
			VALUES ('tgl_1', 'usr_1', 7, 42, 42, 'active', 0, 0)`,
		`INSERT INTO telegram_messages (chat_id, message_id, link_id, kind, created_at) VALUES (42, 1, 'tgl_1', 'card', 0)`,
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
	if err != nil || cur != latest || latest < 59 {
		t.Fatalf("after the upgrade: version %d of %d, %v; want all migrations applied, at least 0059", cur, latest, err)
	}
	var thread any
	if err := s.rdb.QueryRowContext(ctx, `SELECT message_thread_id FROM telegram_messages WHERE chat_id = 42 AND message_id = 1`).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	if thread != nil {
		t.Errorf("an old card's thread = %v, want NULL (not known)", thread)
	}
	if _, err := s.wdb.ExecContext(ctx, `UPDATE telegram_messages SET message_thread_id = 5 WHERE chat_id = 42`); err != nil {
		t.Fatal(err)
	}

	// A fresh database applies every embedded migration in order and ends at the same version.
	fresh, err := Open(ctx, Options{DataDir: filepath.Join(t.TempDir(), "fresh")})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	cur, latest, err = SchemaVersions(ctx, fresh.rdb, EmbeddedMigrations())
	if err != nil || cur != latest || cur < 59 {
		t.Fatalf("fresh database: version %d of %d, %v; want the latest, at least 59", cur, latest, err)
	}
	if !hasColumn(ctx, t, fresh, "telegram_messages", "message_thread_id") {
		t.Fatal("a fresh database lacks telegram_messages.message_thread_id")
	}
	entries, err := fs.ReadDir(EmbeddedMigrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	applied := 0
	if err := fresh.rdb.QueryRowContext(ctx, `SELECT count(DISTINCT version_id) FROM goose_db_version WHERE version_id > 0 AND is_applied`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != len(entries) {
		t.Errorf("a fresh database applied %d migrations, want all %d embedded ones", applied, len(entries))
	}
}

func hasColumn(ctx context.Context, t *testing.T, s *Store, table, column string) bool {
	t.Helper()
	var n int
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}
