package app_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
)

// SM-TG-014, ADR-0083 Q8: the stored link_sso_grants setting reaches the Telegram principal. The "#sso" subject appears
// only while the policy is on and the link's sso_until is in the future; with the policy off (the default) or the
// window over, it never does.
func TestTelegramSSOGrantsPolicyReachesThePrincipal(t *testing.T) {
	t.Parallel()
	s := newTelegramSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.h.c.TelegramSettings.Run(ctx)
	sso := auth.SSOSubject(s.userID)
	hasSSO := func() bool {
		t.Helper()
		if s.h.c.TelegramSSOGrants == nil {
			t.Fatal("Central.TelegramSSOGrants is not wired")
		}
		p, err := s.h.c.Auth.TelegramPrincipal(ctx, s.linkID, auth.TelegramOptions{SSOGrants: s.h.c.TelegramSSOGrants()})
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(p.Subjects(), sso)
	}
	setLink := func(until time.Time) {
		s.exec(t, `UPDATE telegram_links SET via_sso = 1, sso_until = ? WHERE id = ?`, until.UnixMicro(), s.linkID)
	}
	version := 0 // the stored version: the first write creates the row, later ones need If-Match
	setPolicy := func(on bool) {
		var hdr []string
		if version > 0 {
			hdr = []string{"If-Match", fmt.Sprintf(`"%d"`, version)}
		}
		expect(t, s.root.do("PUT", "/telegram/settings", map[string]any{"link_sso_grants": on}, hdr...), 200)
		version++
		eventually(t, "the policy is applied", func() bool { return s.h.c.TelegramSettings.Current().LinkSSOGrants == on })
	}

	setLink(s.h.c.Clock.Now().Add(time.Hour))
	if hasSSO() {
		t.Fatal("the #sso subject appears with the policy off")
	}
	setPolicy(true)
	if !hasSSO() {
		t.Fatal("the #sso subject is missing with the policy on and the window open")
	}
	setLink(s.h.c.Clock.Now().Add(-time.Minute))
	if hasSSO() {
		t.Fatal("the #sso subject appears after sso_until")
	}
	setLink(s.h.c.Clock.Now().Add(time.Hour))
	setPolicy(false)
	if hasSSO() {
		t.Fatal("the #sso subject stays after the policy is turned off")
	}
}
