package miniapp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
)

// fx is a service over a fake bot, fake links and a fake principal builder.
type fx struct {
	t      *testing.T
	clk    *clock.Fake
	tok    string
	svc    *Service
	bot    atomic.Pointer[Bot]
	mu     sync.Mutex
	links  map[int64]string // tg user → active link
	dead   map[string]bool  // links whose principal is refused (revoked, user disabled)
	lim    map[string]bool  // links whose principal is Limited (MFA required, no factor)
	built  atomic.Int64     // principals built
	seq    atomic.Int64
	limits Limits
	refusd []int64
}

func newFx(t *testing.T) *fx {
	t.Helper()
	f := &fx{t: t, clk: clock.NewFake(now0), tok: testToken(t), links: map[int64]string{42: "tgl_a", 43: "tgl_b"}, dead: map[string]bool{}, lim: map[string]bool{},
		limits: DefaultLimits}
	f.bot.Store(&Bot{ID: 123456789, Secret: SecretKey(f.tok)})
	f.svc = New(Options{
		Bot: func() (Bot, bool) {
			b := f.bot.Load()
			if b == nil {
				return Bot{}, false
			}
			return *b, true
		},
		Limits: func() Limits { f.mu.Lock(); defer f.mu.Unlock(); return f.limits },
		ActiveLink: func(_ context.Context, botID, tgUser int64) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if id, ok := f.links[tgUser]; ok && botID == 123456789 {
				return id, nil
			}
			return "", ErrNotLinked
		},
		Principal: func(_ context.Context, linkID string) (*auth.Principal, error) {
			f.built.Add(1)
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.dead[linkID] {
				return nil, auth.ErrUnauthorized
			}
			return &auth.Principal{User: &auth.User{ID: "usr_" + linkID}, Method: auth.MethodTelegram, TelegramLinkID: linkID, Limited: f.lim[linkID]}, nil
		},
		OnRefused: func(_ context.Context, _ string, n int64) { f.mu.Lock(); f.refusd = append(f.refusd, n); f.mu.Unlock() },
		Clock:     f.clk,
	})
	return f
}

// initData signs fresh initData for user, age old (each with its own query_id, as Telegram's launches have).
func (f *fx) initData(user int64, age time.Duration) string {
	fl := fieldsAt(user, f.clk.Now().Add(-age))
	fl["query_id"] = "q" + strconv.FormatInt(f.seq.Add(1), 10)
	return signed(f.tok, fl)
}

func (f *fx) exchange(raw string) (*Session, error) {
	return f.svc.Exchange(context.Background(), raw, "192.0.2.1")
}

func (f *fx) mustExchange(user int64) string {
	f.t.Helper()
	s, err := f.exchange(f.initData(user, 0))
	if err != nil {
		f.t.Fatalf("exchange: %v", err)
	}
	return s.Token
}

func code(err error) apperr.Code {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// An exchange issues an opaque session for the user's active link; the session resolves to that link's principal,
// built anew on every request.
func TestExchangeAndAuthenticate(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	s, err := f.exchange(f.initData(42, 10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.Token, TokenPrefix) || len(s.Token) != len(TokenPrefix)+43 || !s.ExpiresAt.Equal(now0.Add(time.Hour)) ||
		s.Idle != 15*time.Minute {
		t.Fatalf("session %+v", s)
	}
	before := f.built.Load()
	for range 3 {
		p, err := f.svc.Authenticate(context.Background(), s.Token)
		if err != nil || p.TelegramLinkID != "tgl_a" {
			t.Fatalf("authenticate: %+v %v", p, err)
		}
	}
	if n := f.built.Load() - before; n != 3 {
		t.Fatalf("principal built %d times for 3 requests, want 3 (per request)", n)
	}
	// Only the token's hash is held.
	for k := range f.svc.sessions {
		if k != sha256.Sum256([]byte(s.Token)) {
			t.Fatal("session not keyed by the token's hash")
		}
	}
	for _, bad := range []string{"", "x", s.Token + "x", s.Token[:len(s.Token)-1], strings.Replace(s.Token, TokenPrefix, "spaceman_tgx_", 1),
		TokenPrefix + strings.Repeat("A", 43)} {
		if _, err := f.svc.Authenticate(context.Background(), bad); !errors.Is(err, auth.ErrUnauthorized) {
			t.Errorf("token %q: %v", bad, err)
		}
	}
}

// Forged, wrong-hash, old, future-dated and replayed initData are all refused with the same 401; the refusals are
// reported in aggregate, once per address per minute.
func TestExchangeRefusals(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	other := testToken(t)
	raw := f.initData(42, 0)
	if _, err := f.exchange(raw); err != nil {
		t.Fatal(err)
	}
	// Replay: the same string, and the same signed content reordered.
	parts := strings.Split(raw, "&")
	parts[0], parts[len(parts)-1] = parts[len(parts)-1], parts[0]
	for name, r := range map[string]string{
		"replayed":           raw,
		"replayed reordered": strings.Join(parts, "&"),
		"forged":             signed(other, fieldsAt(42, now0)),
		"wrong hash":         encode(fieldsAt(42, now0), strings.Repeat("0", 64)),
		"older than 5 min":   f.initData(42, 5*time.Minute+time.Second),
		"future auth_date":   f.initData(42, -time.Minute),
		"malformed":          "user=1",
	} {
		if _, err := f.exchange(r); code(err) != apperr.CodeUnauthorized {
			t.Errorf("%s: %v, want unauthorized", name, err)
		}
	}
	if f.svc.Len() != 1 {
		t.Fatalf("%d sessions, want 1", f.svc.Len())
	}
	f.mu.Lock()
	got := append([]int64(nil), f.refusd...)
	f.mu.Unlock()
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("refusals reported %v, want one report of the first", got)
	}
	f.clk.Advance(time.Minute)
	_, _ = f.exchange("user=1")
	f.mu.Lock()
	got = append([]int64(nil), f.refusd...)
	f.mu.Unlock()
	if len(got) != 2 || got[1] != 7 {
		t.Fatalf("refusals reported %v, want the next report to carry the 7 since", got)
	}
}

// The max age follows the setting; a used initData stays used even after the setting is raised.
func TestExchangeMaxAgeSetting(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	raw := f.initData(42, 2*time.Minute)
	f.mu.Lock()
	f.limits.InitDataMaxAge = time.Minute
	f.mu.Unlock()
	if _, err := f.exchange(raw); code(err) != apperr.CodeUnauthorized {
		t.Fatalf("older than the setting: %v", err)
	}
	f.mu.Lock()
	f.limits.InitDataMaxAge = 3 * time.Minute
	f.mu.Unlock()
	if _, err := f.exchange(raw); err != nil {
		t.Fatalf("within the raised setting: %v", err)
	}
	f.mu.Lock()
	f.limits.InitDataMaxAge = time.Hour
	f.mu.Unlock()
	f.clk.Advance(50 * time.Minute)
	if _, err := f.exchange(raw); code(err) != apperr.CodeUnauthorized {
		t.Fatalf("replay within the largest max age: %v", err)
	}
}

// Without an active link, or for a refused principal, there is no session; with no bot there is no exchange.
func TestExchangeNeedsLinkAndBot(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	if _, err := f.exchange(f.initData(99, 0)); code(err) != apperr.CodeForbidden {
		t.Fatalf("unlinked: %v", err)
	}
	f.mu.Lock()
	f.dead["tgl_b"] = true
	f.mu.Unlock()
	if _, err := f.exchange(f.initData(43, 0)); code(err) != apperr.CodeForbidden {
		t.Fatalf("disabled user: %v", err)
	}
	f.bot.Store(nil)
	if _, err := f.exchange(f.initData(42, 0)); code(err) != apperr.CodeUnauthorized {
		t.Fatalf("no bot: %v", err)
	}
	// Signed by the old token after a token change: refused.
	f.bot.Store(&Bot{ID: 123456789, Secret: SecretKey(testToken(t))})
	if _, err := f.exchange(f.initData(42, 0)); code(err) != apperr.CodeUnauthorized {
		t.Fatalf("old token: %v", err)
	}
	if f.svc.Len() != 0 {
		t.Fatal("a session was made")
	}
}

// Sessions end after the idle timeout (stream re-checks don't count as use) and the absolute lifetime, when the bot
// goes away or changes, and when the link's principal is refused (revoked link, disabled user).
func TestSessionLifetime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFx(t)
	tok := f.mustExchange(42)
	f.clk.Advance(14 * time.Minute)
	if _, err := f.svc.Validate(ctx, tok); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(time.Minute)
	if _, err := f.svc.Validate(ctx, tok); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("idle 15 min with only stream re-checks: %v", err)
	}

	tok = f.mustExchange(42)
	for range 4 {
		f.clk.Advance(14 * time.Minute)
		if _, err := f.svc.Authenticate(ctx, tok); err != nil {
			t.Fatalf("in use: %v", err)
		}
	}
	f.clk.Advance(4 * time.Minute) // 60 min after the exchange
	if _, err := f.svc.Authenticate(ctx, tok); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("absolute lifetime: %v", err)
	}

	// A shortened setting applies to open sessions at once.
	tok = f.mustExchange(42)
	f.clk.Advance(6 * time.Minute)
	f.mu.Lock()
	f.limits.Idle = 5 * time.Minute
	f.mu.Unlock()
	if _, err := f.svc.Authenticate(ctx, tok); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("shortened idle: %v", err)
	}
	f.mu.Lock()
	f.limits = DefaultLimits
	f.mu.Unlock()

	tok = f.mustExchange(42)
	f.bot.Store(&Bot{ID: 987654321, Secret: SecretKey(testToken(t))})
	if _, err := f.svc.Authenticate(ctx, tok); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("another bot: %v", err)
	}
	f.bot.Store(&Bot{ID: 123456789, Secret: SecretKey(f.tok)})
	if _, err := f.svc.Authenticate(ctx, tok); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("a session survived a bot change")
	}

	tok = f.mustExchange(42)
	f.bot.Store(nil)
	if _, err := f.svc.Authenticate(ctx, tok); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("bot off: %v", err)
	}
	f.bot.Store(&Bot{ID: 123456789, Secret: SecretKey(f.tok)})

	tok = f.mustExchange(42)
	f.mu.Lock()
	f.dead["tgl_a"] = true
	f.mu.Unlock()
	if _, err := f.svc.Authenticate(ctx, tok); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("revoked link: %v", err)
	}
	f.mu.Lock()
	f.dead["tgl_a"] = false
	f.mu.Unlock()
	if _, err := f.svc.Authenticate(ctx, tok); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("a session outlived its link's refusal")
	}

	a, b := f.mustExchange(42), f.mustExchange(43)
	f.svc.RevokeLink("tgl_a")
	if _, err := f.svc.Authenticate(ctx, a); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("RevokeLink left the session")
	}
	if _, err := f.svc.Authenticate(ctx, b); err != nil {
		t.Fatalf("RevokeLink ended another link's session: %v", err)
	}
}

// One link holds at most sessionsPerLink sessions (the oldest goes); exchanges are rate limited per address and per
// Telegram user.
func TestSessionBoundsAndRateLimits(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	var toks []string
	for range sessionsPerLink + 2 {
		toks = append(toks, f.mustExchange(42))
		f.clk.Advance(time.Second)
	}
	if f.svc.Len() != sessionsPerLink {
		t.Fatalf("%d sessions for one link, want %d", f.svc.Len(), sessionsPerLink)
	}
	if _, err := f.svc.Authenticate(context.Background(), toks[0]); err == nil {
		t.Fatal("the oldest session survived")
	}
	if _, err := f.svc.Authenticate(context.Background(), toks[len(toks)-1]); err != nil {
		t.Fatal(err)
	}
	// Per Telegram user: 10 a minute (7 used above).
	for i := range 3 {
		if _, err := f.exchange(f.initData(42, 0)); err != nil {
			t.Fatalf("exchange %d: %v", i, err)
		}
		f.clk.Advance(time.Second)
	}
	if _, err := f.exchange(f.initData(42, 0)); code(err) != apperr.CodeRateLimited {
		t.Fatalf("11th exchange for one user: %v", err)
	}
	// Per address: 20 a minute, whatever the user.
	g := newFx(t)
	for i := range 20 {
		if _, err := g.svc.Exchange(context.Background(), "junk", "2001:db8::1"); code(err) != apperr.CodeUnauthorized {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := g.svc.Exchange(context.Background(), g.initData(42, 0), "2001:db8::2"); code(err) != apperr.CodeRateLimited {
		t.Fatalf("21st attempt from the same /64: %v", err)
	}
	if _, err := g.svc.Exchange(context.Background(), g.initData(42, 0), "192.0.2.9"); err != nil {
		t.Fatalf("another address: %v", err)
	}
}

// The replay cache drops the oldest entries when full and forgets expired ones; a dropped unexpired entry stays refused
// through the watermark.
func TestReplayCacheBounds(t *testing.T) {
	t.Parallel()
	c := newReplayCache(3)
	key := func(b byte) (k [sha256.Size]byte) { k[0] = b; return k }
	auth := func(b byte) time.Time { return now0.Add(time.Duration(b) * time.Second) }
	add := func(b byte, a time.Time, now time.Time) bool { return c.add(key(b), a, a.Add(time.Hour), now) }
	for i := range byte(3) {
		if !add(i, auth(i), now0) {
			t.Fatal("fresh entry refused")
		}
	}
	if add(1, auth(1), now0) {
		t.Fatal("replay accepted")
	}
	if !add(3, auth(3), now0) || c.len() != 3 {
		t.Fatalf("full cache: len %d", c.len())
	}
	if add(0, auth(0), now0) {
		t.Fatal("the dropped oldest entry was accepted again")
	}
	if add(3, auth(3), now0) {
		t.Fatal("a newer entry was dropped")
	}
	later := now0.Add(2 * time.Hour)
	if !add(3, later, later) || c.len() != 1 {
		t.Fatalf("expired entries kept: len %d", c.len())
	}
}

// Evicting an unexpired entry raises a watermark, so a flush can't make the victim's initData acceptable again; fresher
// initData still passes.
func TestReplayWatermark(t *testing.T) {
	t.Parallel()
	c := newReplayCache(replayLimit)
	key := func(i int) (k [sha256.Size]byte) { k[0], k[1], k[2] = byte(i), byte(i>>8), 1; return k }
	victimAuth := now0
	if !c.add(key(0), victimAuth, victimAuth.Add(replayWindow), now0) {
		t.Fatal("victim refused")
	}
	for i := 1; i <= 10000; i++ {
		a := now0.Add(time.Duration(i) * time.Millisecond)
		if !c.add(key(i), a, a.Add(replayWindow), now0) {
			t.Fatalf("flush entry %d refused", i)
		}
	}
	if c.add(key(0), victimAuth, victimAuth.Add(replayWindow), now0) {
		t.Fatal("the victim's initData was accepted again after a flush")
	}
	fresh := now0.Add(time.Minute)
	if !c.add(key(20000), fresh, fresh.Add(replayWindow), now0) {
		t.Fatal("fresh initData refused")
	}
}

// Rotating addresses inside one /64 is one refusing client: one audit row a minute, not one per address; and the
// refused map and the rows written stay bounded under a /48 rotation.
func TestRefusedAuditPer64AndBounded(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	rows := 0
	f.svc.o.OnRefused = func(context.Context, string, int64) { f.mu.Lock(); rows++; f.mu.Unlock() }
	for i := range 25 {
		_, _ = f.svc.Exchange(context.Background(), "hash=bad", fmt.Sprintf("2001:db8:1:2::%x", i+1))
	}
	if rows > 1 {
		t.Fatalf("25 refusals from one /64 wrote %d audit rows", rows)
	}
	rows = 0
	for i := range 3 * refusedLimit {
		_, _ = f.svc.Exchange(context.Background(), "hash=bad", fmt.Sprintf("2001:db8:3:%x::1", i&0xffff))
	}
	f.svc.mu.Lock()
	n := len(f.svc.refused)
	f.svc.mu.Unlock()
	if n > refusedLimit {
		t.Fatalf("refused map holds %d entries (limit %d)", n, refusedLimit)
	}
	if rows > refusedRowsPerMinute {
		t.Fatalf("%d audit rows in one minute (limit %d)", rows, refusedRowsPerMinute)
	}
}

// Every refusal is accounted for: the per-minute row cap and the eviction of a full refused map must not hide one
// address's refusals; what the cap withholds is reported in one overflow row a minute.
func TestRefusedAuditConserved(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	var recorded, refused int64
	f.svc.o.OnRefused = func(_ context.Context, _ string, n int64) { f.mu.Lock(); recorded += n; f.mu.Unlock() }
	// refuse counts only the refusals that answered errInvalid: a 429 is not a refused exchange.
	refuse := func(ip string) {
		if _, err := f.svc.Exchange(context.Background(), "hash=bad", ip); errors.Is(err, errInvalid) {
			refused++
		}
	}
	refuse("192.0.2.7") // the address gets its row first ...
	for range 10 {      // ... then 10 more refusals inside its window: they are held back in its counter
		refuse("192.0.2.7")
	}
	for i := range refusedRowsPerMinute - 1 { // a probe uses up the rest of the minute's budget with junk /64s
		refuse(fmt.Sprintf("2001:db8:%x::1", i+1))
	}
	for i := range refusedLimit + 1 { // and a flood of other addresses evicts the address while it holds its 10
		refuse(fmt.Sprintf("2001:db8:ffff:%x::1", i))
	}
	f.svc.mu.Lock()
	_, held := f.svc.refused["192.0.2.7"]
	f.svc.mu.Unlock()
	if held {
		t.Fatal("the address was not evicted; the test does not exercise eviction")
	}
	f.clk.Advance(refusedWindow + time.Second)
	refuse("192.0.2.8") // the next minute: its row carries the overflow row with it
	f.mu.Lock()
	defer f.mu.Unlock()
	if recorded != refused {
		t.Fatalf("%d refusals, %d recorded", refused, recorded)
	}
}

// A new key for the same bot ID (a token rotation) ends the sessions the old key opened.
func TestSessionEndsOnKeyRotation(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	tok := f.mustExchange(42)
	if _, err := f.svc.Authenticate(context.Background(), tok); err != nil {
		t.Fatalf("before rotation: %v", err)
	}
	f.bot.Store(&Bot{ID: 123456789, Secret: SecretKey(testToken(t) + "x")})
	if _, err := f.svc.Authenticate(context.Background(), tok); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("after rotation: %v", err)
	}
}

// A link whose user is Limited (MFA required, no factor) gets no principal from LinkPrincipal, as a Mini App session
// gets none from Validate: a download minted before the user became Limited is refused on redemption.
func TestLinkPrincipalRefusesLimited(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	p, err := f.svc.LinkPrincipal(context.Background(), "tgl_a")
	if err != nil || p.TelegramLinkID != "tgl_a" {
		t.Fatalf("LinkPrincipal = %v, %v", p, err)
	}
	f.mu.Lock()
	f.lim["tgl_a"] = true
	f.mu.Unlock()
	if _, err := f.svc.LinkPrincipal(context.Background(), "tgl_a"); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("Limited link: %v, want ErrUnauthorized", err)
	}
}

// A user whose MFA is required and not set up gets no session, and a 403 that says to finish it in the browser (not the
// generic "reopen" 401: reopening cannot help).
func TestExchangeRefusesLimitedWithMessage(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.mu.Lock()
	f.lim["tgl_a"] = true
	f.mu.Unlock()
	_, err := f.exchange(f.initData(42, 0))
	if code(err) != apperr.CodeForbidden || !strings.Contains(err.Error(), "browser") {
		t.Fatalf("Limited exchange: %v, want a forbidden error naming the browser", err)
	}
	if f.svc.Len() != 0 {
		t.Fatalf("a session was opened for a Limited link")
	}
}
