package notify_test

import (
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
)

// TestMayRead is the check a topic rename makes before it writes a space's name into a chat: every way a link can lose
// the right to read a space, and one case that is allowed. Each case changes one thing from the allowed link.
func TestMayRead(t *testing.T) {
	grant := func(e *env, subject, space string) {
		e.az.mu.Lock()
		defer e.az.mu.Unlock()
		if e.az.space[subject] == nil {
			e.az.space[subject] = map[string]bool{}
		}
		e.az.space[subject][space] = true
	}
	cases := []struct {
		name  string
		setup func(e *env, l *notify.Link)
		chat  int64 // 0: the link's chat
		link  string
		want  bool
	}{
		{name: "allowed", want: true},
		{name: "not in the snapshot", link: "tgl_gone"},
		{name: "moved to another chat", chat: 9999},
		{name: "re-confirmation overdue", setup: func(e *env, l *notify.Link) { l.ReconfirmAt = t0.Add(-time.Hour) }},
		{name: "re-confirmation still ahead", setup: func(e *env, l *notify.Link) { l.ReconfirmAt = t0.Add(time.Hour) }, want: true},
		{name: "no read grant", setup: func(e *env, l *notify.Link) {
			e.az.mu.Lock()
			delete(e.az.space["user:"+l.UserID], "spc_1")
			delete(e.az.space["telegram:"+l.ID], "spc_1")
			e.az.mu.Unlock()
		}},
		{name: "the user's grant is hidden by the link's cap", setup: func(e *env, l *notify.Link) {
			e.az.mu.Lock()
			delete(e.az.space["telegram:"+l.ID], "spc_1") // the user holds it, the link's capped grants do not
			e.az.mu.Unlock()
		}},
		{name: "space not visible to Telegram", setup: func(e *env, l *notify.Link) { e.pol.hide("spc_1") }},
		{name: "SSO grant with the policy off", setup: func(e *env, l *notify.Link) {
			l.ViaSSO, l.SSOUntil = true, t0.Add(time.Hour)
			e.az.mu.Lock()
			delete(e.az.space["user:"+l.UserID], "spc_1") // only the SSO subject holds it
			e.az.mu.Unlock()
			grant(e, auth.SSOSubject(l.UserID), "spc_1")
		}},
		{name: "SSO grant with the policy on", setup: func(e *env, l *notify.Link) {
			e.pol.set.LinkSSOGrants = true
			l.ViaSSO, l.SSOUntil = true, t0.Add(time.Hour)
			e.az.mu.Lock()
			delete(e.az.space["user:"+l.UserID], "spc_1")
			e.az.mu.Unlock()
			grant(e, auth.SSOSubject(l.UserID), "spc_1")
		}, want: true},
		{name: "SSO session ended", setup: func(e *env, l *notify.Link) {
			e.pol.set.LinkSSOGrants = true
			l.ViaSSO, l.SSOUntil = true, t0.Add(-time.Hour)
			e.az.mu.Lock()
			delete(e.az.space["user:"+l.UserID], "spc_1")
			e.az.mu.Unlock()
			grant(e, auth.SSOSubject(l.UserID), "spc_1")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, 1)
			l := linkFor(1)
			if tc.setup != nil {
				tc.setup(e, &l)
			}
			e.links.Set([]notify.Link{l})
			n := notify.New(notify.Options{Feed: e.feed, Reader: e.rd, Authz: e.az, Policy: e.pol, Links: e.links,
				Sender: func() *sender.Sender { return e.snd }, Bus: e.bus, Now: e.clk.Now})
			id, c := l.ID, tc.chat
			if tc.link != "" {
				id = tc.link
			}
			if c == 0 {
				c = l.ChatID
			}
			if got := n.MayRead(id, c, "spc_1"); got != tc.want {
				t.Fatalf("MayRead = %v, want %v", got, tc.want)
			}
		})
	}
}
