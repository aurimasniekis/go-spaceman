package link

import (
	"context"
	"errors"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// prefsEnv: users usr_a (links tgl_a1, tgl_a2, and the revoked tgl_a3) and usr_b (link tgl_b1).
func prefsEnv(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	s := storetest.Open(t)
	kr := keyringtest.Load(t, s)
	ctx := context.Background()
	if err := s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO users (id, display_name, enabled, provisioned_via, created_at, created_by, updated_at) VALUES ('usr_a', 'A', 1, 'admin', 0, 't', 0)`,
			`INSERT INTO users (id, display_name, enabled, provisioned_via, created_at, created_by, updated_at) VALUES ('usr_b', 'B', 1, 'admin', 0, 't', 0)`,
			`INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, created_at, updated_at) VALUES ('tgl_a1', 'usr_a', 1, 11, 11, 0, 0)`,
			`INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, created_at, updated_at, prefs_json) VALUES ('tgl_a2', 'usr_a', 1, 12, 12, 0, 0, '{"digest":false}')`,
			`INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, created_at, updated_at, state, revoked_at) VALUES ('tgl_a3', 'usr_a', 1, 13, 13, 0, 0, 'revoked', 1)`,
			`INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, created_at, updated_at, prefs_json) VALUES ('tgl_b1', 'usr_b', 1, 21, 21, 0, 0, '{"digest":false}')`,
		} {
			if _, err := tx.SQL.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return New(Options{Store: s, Audit: audit.NewWriter(kr)}), s
}

func tgPrincipal(user, link string) *auth.Principal {
	return &auth.Principal{User: &auth.User{ID: user}, Method: auth.MethodTelegram, TelegramLinkID: link}
}

func storedPrefs(t *testing.T, s *store.Store, link string) (string, int64) {
	t.Helper()
	l, err := s.R().GetTelegramLink(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	return l.PrefsJson, l.Version
}

func wantCode(t *testing.T, what string, err error, code apperr.Code) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != code {
		t.Fatalf("%s: error %v, want %s", what, err, code)
	}
}

func ptr[T any](v T) *T { return &v }

// SM-TG-009 (amendment TG-10-4): a principal reads and changes only the link it acts through. A principal whose link
// ID names another user's link, a revoked link or no link, and any non-Telegram principal, gets nothing and changes
// nothing; the other links' preferences stay as they were.
func TestLinkPrefsOwnership(t *testing.T) {
	t.Parallel()
	svc, s := prefsEnv(t)
	ctx := context.Background()
	in := PrefsInput{}
	in.NewActions = ptr(false)

	p, err := svc.GetPrefs(ctx, tgPrincipal("usr_a", "tgl_a1"))
	if err != nil || p.LinkID != "tgl_a1" || p.Version != 1 || !p.Effective.NewActions || p.Topics.Available {
		t.Fatalf("own link: %+v, %v", p, err)
	}
	refused := []struct {
		name string
		p    *auth.Principal
		code apperr.Code
	}{
		{"another user's link", tgPrincipal("usr_a", "tgl_b1"), apperr.CodeNotFound},
		{"a revoked link", tgPrincipal("usr_a", "tgl_a3"), apperr.CodeNotFound},
		{"an unknown link", tgPrincipal("usr_a", "tgl_none"), apperr.CodeNotFound},
		{"a web session", &auth.Principal{User: &auth.User{ID: "usr_a"}, Method: auth.MethodPassword}, apperr.CodeForbidden},
		{"an API token", &auth.Principal{User: &auth.User{ID: "usr_a"}, TokenID: "tok_1", TelegramLinkID: "tgl_a1"}, apperr.CodeForbidden},
		{"a root token", &auth.Principal{RootTokenID: "rt_1", TelegramLinkID: "tgl_a1"}, apperr.CodeForbidden},
		{"no user", &auth.Principal{TelegramLinkID: "tgl_a1"}, apperr.CodeForbidden},
		{"nil", nil, apperr.CodeForbidden},
	}
	for _, c := range refused {
		_, err := svc.GetPrefs(ctx, c.p)
		wantCode(t, "get as "+c.name, err, c.code)
		_, err = svc.SetPrefs(ctx, c.p, audit.System, 1, in)
		wantCode(t, "set as "+c.name, err, c.code)
	}
	for _, l := range []string{"tgl_a2", "tgl_b1"} {
		if raw, v := storedPrefs(t, s, l); raw != `{"digest":false}` || v != 1 {
			t.Fatalf("%s changed: %s v%d", l, raw, v)
		}
	}
	if raw, _ := storedPrefs(t, s, "tgl_a3"); raw != "{}" {
		t.Fatalf("revoked link changed: %s", raw)
	}

	// The owner's write lands on its own link only, and the version guards it.
	out, err := svc.SetPrefs(ctx, tgPrincipal("usr_a", "tgl_a1"), audit.System, 1, in)
	if err != nil || out.LinkID != "tgl_a1" || out.Version != 2 || out.Effective.NewActions || out.Overrides.NewActions == nil {
		t.Fatalf("own write: %+v, %v", out, err)
	}
	if raw, _ := storedPrefs(t, s, "tgl_a1"); raw != `{"new_actions":false}` {
		t.Fatalf("stored %s", raw)
	}
	_, err = svc.SetPrefs(ctx, tgPrincipal("usr_a", "tgl_a1"), audit.System, 1, in)
	wantCode(t, "stale version", err, apperr.CodePrecondition)
	for _, l := range []string{"tgl_a2", "tgl_b1"} {
		if raw, v := storedPrefs(t, s, l); raw != `{"digest":false}` || v != 1 {
			t.Fatalf("%s changed: %s v%d", l, raw, v)
		}
	}
}

// The time zone is the user's: it is written into notification_prefs without touching the other fields there, and it
// is validated; muted spaces are de-duplicated and must be space IDs; invalid overrides are refused.
func TestLinkPrefsTimezoneAndValidation(t *testing.T) {
	t.Parallel()
	svc, s := prefsEnv(t)
	ctx := context.Background()
	if err := s.W().Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO notification_prefs (user_id, prefs_json, updated_at) VALUES ('usr_a', '{"desktop":true,"enabled":true}', 0)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	me := tgPrincipal("usr_a", "tgl_a1")
	in := PrefsInput{Timezone: ptr(" Europe/Vilnius ")}
	in.MutedSpaces = &[]string{"spc_01aaaaaaaaaaaaaaaaaaaaaaaa", " spc_01aaaaaaaaaaaaaaaaaaaaaaaa", "spc_01bbbbbbbbbbbbbbbbbbbbbbbb"}
	out, err := svc.SetPrefs(ctx, me, audit.System, 1, in)
	if err != nil || out.Timezone != "Europe/Vilnius" || len(out.Effective.MutedSpaces) != 2 {
		t.Fatalf("set: %+v, %v", out, err)
	}
	row, err := s.R().GetNotificationPrefs(ctx, "usr_a")
	if err != nil || row.PrefsJson != `{"desktop":true,"enabled":true,"timezone":"Europe/Vilnius"}` {
		t.Fatalf("user prefs %q, %v", row.PrefsJson, err)
	}
	// Left out keeps the zone; empty clears it.
	if out, err = svc.SetPrefs(ctx, me, audit.System, 2, PrefsInput{}); err != nil || out.Timezone != "Europe/Vilnius" || len(out.Effective.MutedSpaces) != 0 {
		t.Fatalf("keep: %+v, %v", out, err)
	}
	if out, err = svc.SetPrefs(ctx, me, audit.System, 3, PrefsInput{Timezone: ptr("")}); err != nil || out.Timezone != "" {
		t.Fatalf("clear: %+v, %v", out, err)
	}
	bad := []PrefsInput{{Timezone: ptr("Mars/Olympus")}, {Timezone: ptr("Local")}}
	b := PrefsInput{}
	b.MutedSpaces = &[]string{"*"}
	bad = append(bad, b)
	b = PrefsInput{}
	b.QuietHoursStart = ptr("25:00")
	bad = append(bad, b)
	b = PrefsInput{}
	b.Reminders = ptr("loud")
	bad = append(bad, b)
	for _, in := range bad {
		_, err := svc.SetPrefs(ctx, me, audit.System, 4, in)
		wantCode(t, "bad input", err, apperr.CodeInvalid)
	}
	if _, v := storedPrefs(t, s, "tgl_a1"); v != 4 {
		t.Fatalf("an invalid input was written: v%d", v)
	}
}

// The UPDATE repeats the ownership check (defense in depth behind ownLink): another user's link, a revoked link or a
// stale version writes nothing.
func TestSetTelegramLinkPrefsQueryGuards(t *testing.T) {
	t.Parallel()
	_, s := prefsEnv(t)
	ctx := context.Background()
	cases := []struct {
		link, user string
		version    int64
		want       int64
	}{
		{"tgl_b1", "usr_a", 1, 0},
		{"tgl_a3", "usr_a", 1, 0},
		{"tgl_a1", "usr_a", 7, 0},
		{"tgl_a1", "usr_a", 1, 1},
	}
	for _, c := range cases {
		var n int64
		if err := s.W().Tx(ctx, func(tx *store.Tx) error {
			var err error
			n, err = tx.Q.SetTelegramLinkPrefs(ctx, db.SetTelegramLinkPrefsParams{PrefsJson: `{"digest":true}`, Now: 1, ID: c.link, UserID: c.user, Version: c.version})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if n != c.want {
			t.Fatalf("%s as %s v%d: %d rows, want %d", c.link, c.user, c.version, n, c.want)
		}
	}
	for _, l := range []string{"tgl_b1", "tgl_a3"} {
		if raw, _ := storedPrefs(t, s, l); raw == `{"digest":true}` {
			t.Fatalf("%s written", l)
		}
	}
}
