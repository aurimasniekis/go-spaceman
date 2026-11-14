package miniapp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
)

// TokenPrefix starts every Mini App session token; 43 base64url characters (32 random bytes) follow.
const TokenPrefix = "spaceman_tgs_" //nolint:gosec // G101: a token prefix, not a credential

const tokenLen = len(TokenPrefix) + 43

// Bounds (plan §3.4.2).
const (
	// replayWindow is how long a used initData is remembered: the largest init_data_max_age_seconds the settings allow
	// plus the future skew, so it is too old to be accepted again by the time it is forgotten.
	replayWindow = time.Hour + MaxFutureSkew
	// replayLimit caps the replay cache; the oldest entries are dropped first.
	replayLimit = 10000
	// sessionLimit caps the sessions held in memory; sessionsPerLink caps one link's (the oldest is dropped).
	sessionLimit    = 20000
	sessionsPerLink = 5
	// refusedWindow is the most often refused exchanges from one address are reported (OnRefused).
	refusedWindow = time.Minute
	// refusedLimit caps the per-address refusal counters (the oldest is dropped); refusedRowsPerMinute caps the audit
	// rows OnRefused is called for across all addresses; what the cap withholds, and what an evicted counter held, is
	// reported as one overflow row a minute (OverflowKey) that the cap does not block.
	refusedLimit         = 10000
	refusedRowsPerMinute = 60
)

// OverflowKey is the clientIP OnRefused gets for the overflow row: the refusals that no per-address row reported
// (withheld by the rows-a-minute cap or lost with an evicted counter), over all addresses.
const OverflowKey = "*"

// Bot is the running bot as the Mini App needs it: its ID (links belong to one bot) and the key its initData is
// signed with (SecretKey of its token). The token itself never reaches this package.
type Bot struct {
	ID     int64
	Secret []byte
}

// Limits are the settings' lifetimes (init_data_max_age_seconds, miniapp_session_idle_seconds,
// miniapp_session_absolute_seconds), read on every use so a change applies at once.
type Limits struct {
	InitDataMaxAge time.Duration
	Idle           time.Duration
	Absolute       time.Duration
}

// DefaultLimits are the built-in settings.
var DefaultLimits = Limits{InitDataMaxAge: 5 * time.Minute, Idle: 15 * time.Minute, Absolute: time.Hour}

// ErrNotLinked is ActiveLink's answer for a Telegram user without an active link to the bot.
var ErrNotLinked = errors.New("miniapp: no active link")

// Options wire the service to central.
type Options struct {
	// Bot returns the running bot, or false while no bot runs (every exchange and session is refused then).
	Bot func() (Bot, bool)
	// Limits returns the current lifetimes (nil: DefaultLimits).
	Limits func() Limits
	// ActiveLink returns the ID of the Telegram user's active link to the bot, or ErrNotLinked.
	ActiveLink func(ctx context.Context, botID, tgUserID int64) (string, error)
	// Principal builds a link's principal (auth.Service.TelegramPrincipal with the current policy). It runs on every
	// request, so a revoked or narrowed link, a disabled user, a grants or a link_sso_grants change applies to the next
	// request of an open session.
	Principal func(ctx context.Context, linkID string) (*auth.Principal, error)
	// OnRefused records refused exchanges (forged, malformed, expired or replayed initData) as one audit row per address
	// per minute at most, with the number refused since the last row. It must not block for long.
	OnRefused func(ctx context.Context, clientIP string, refused int64)
	Clock     clock.Clock
	Log       *slog.Logger
}

// Session is what an exchange returns.
type Session struct {
	Token     string
	ExpiresAt time.Time
	Idle      time.Duration
}

type session struct {
	linkID  string
	botID   int64
	keyGen  [sha256.Size]byte // hash of the bot's signing key when the session opened: a rotated token ends it
	created time.Time
	last    time.Time
}

type refusedCount struct {
	pending  int64
	reported time.Time
}

// Service exchanges initData for sessions and resolves session tokens (api.TelegramSessions). Sessions live in memory
// only (a single central, SM-INV-001): a restart ends them all.
type Service struct {
	o       Options
	replay  *replayCache
	ipLim   *keyedLimiter
	userLim *keyedLimiter

	mu       sync.Mutex
	sessions map[[sha256.Size]byte]*session
	refused  map[string]*refusedCount
	// refusedOrder is refused's insertion order (for dropping the oldest); rowsStart/rows count the audit rows reported
	// in the current minute. suppressed counts refusals no row has reported yet; overflowAt is when the last overflow
	// row was reported.
	refusedOrder []string
	rowsStart    time.Time
	rows         int
	suppressed   int64
	overflowAt   time.Time
}

// New returns the service.
func New(o Options) *Service {
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{o: o, replay: newReplayCache(replayLimit),
		ipLim:    newKeyedLimiter(rate.Limit(20.0/60), 20), // per address: 20 a minute
		userLim:  newKeyedLimiter(rate.Limit(10.0/60), 10), // per Telegram user: 10 a minute
		sessions: map[[sha256.Size]byte]*session{}, refused: map[string]*refusedCount{}}
}

func (svc *Service) limits() Limits {
	if svc.o.Limits == nil {
		return DefaultLimits
	}
	return svc.o.Limits()
}

func (svc *Service) bot() (Bot, bool) {
	if svc.o.Bot == nil {
		return Bot{}, false
	}
	b, ok := svc.o.Bot()
	return b, ok && b.ID != 0 && len(b.Secret) > 0
}

var (
	errInvalid   = apperr.New(apperr.CodeUnauthorized, "invalid or expired Telegram data: close and reopen the app")
	errNotLinked = apperr.New(apperr.CodeForbidden, "this Telegram account is not linked to SpaceMan: link it from your account page first")
	errFull      = apperr.New(apperr.CodeRateLimited, "too many Mini App sessions; try again later")
)

// Exchange validates initData and opens a session for its user's active link (plan §3.4.2). Forged, malformed,
// expired, future-dated and replayed initData are all refused alike (401) and reported in aggregate; a valid initData
// is used up even when the exchange then fails. clientIP is the request's address, for the rate limit.
func (svc *Service) Exchange(ctx context.Context, initData, clientIP string) (*Session, error) {
	if !svc.ipLim.allow(addrKey(clientIP)) {
		return nil, apperr.ErrRateLimited
	}
	b, ok := svc.bot()
	if !ok {
		return nil, errInvalid
	}
	lim, now := svc.limits(), svc.o.Clock.Now()
	d, err := Validate(initData, b.Secret, now, lim.InitDataMaxAge)
	if err != nil {
		svc.o.Log.Debug("telegram: Mini App initData refused", slog.String("reason", err.Error()))
		svc.refuse(ctx, clientIP, now)
		return nil, errInvalid
	}
	if !svc.userLim.allow(strconv.FormatInt(d.UserID, 10)) {
		return nil, apperr.ErrRateLimited
	}
	if !svc.replay.add(d.Hash, d.AuthDate, d.AuthDate.Add(replayWindow), now) {
		svc.o.Log.Debug("telegram: Mini App initData replayed")
		svc.refuse(ctx, clientIP, now)
		return nil, errInvalid
	}
	linkID, err := svc.o.ActiveLink(ctx, b.ID, d.UserID)
	if errors.Is(err, ErrNotLinked) {
		return nil, errNotLinked
	}
	if err != nil {
		return nil, err
	}
	if _, err := svc.o.Principal(ctx, linkID); err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			return nil, errNotLinked
		}
		return nil, err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	tok := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if !svc.makeRoomLocked(linkID, lim, now) {
		return nil, errFull
	}
	svc.sessions[sha256.Sum256([]byte(tok))] = &session{linkID: linkID, botID: b.ID, keyGen: keyGen(b), created: now, last: now}
	return &Session{Token: tok, ExpiresAt: now.Add(lim.Absolute), Idle: lim.Idle}, nil
}

func expired(s *session, lim Limits, now time.Time) bool {
	return now.Sub(s.created) >= lim.Absolute || now.Sub(s.last) >= lim.Idle
}

// makeRoomLocked drops expired sessions when the store is full and the oldest of the link's beyond sessionsPerLink-1.
// It reports false when there is still no room.
func (svc *Service) makeRoomLocked(linkID string, lim Limits, now time.Time) bool {
	var oldestKey [sha256.Size]byte
	var oldest *session
	n := 0
	for k, s := range svc.sessions {
		if s.linkID != linkID {
			continue
		}
		if expired(s, lim, now) {
			delete(svc.sessions, k)
			continue
		}
		n++
		if oldest == nil || s.created.Before(oldest.created) {
			oldestKey, oldest = k, s
		}
	}
	if n >= sessionsPerLink {
		delete(svc.sessions, oldestKey)
	}
	if len(svc.sessions) >= sessionLimit {
		for k, s := range svc.sessions {
			if expired(s, lim, now) {
				delete(svc.sessions, k)
			}
		}
	}
	return len(svc.sessions) < sessionLimit
}

// keyGen identifies the bot's signing key: sessions opened under another key (a token rotation keeps the bot ID) end.
func keyGen(b Bot) [sha256.Size]byte { return sha256.Sum256(b.Secret) }

// refuse counts a refused exchange from ip's rate-limit key (an IPv6 /64 is one client) and reports the count at most
// once per refusedWindow per key and refusedRowsPerMinute in all; what isn't reported rolls into the next aggregate.
func (svc *Service) refuse(ctx context.Context, ip string, now time.Time) {
	key := addrKey(ip)
	svc.mu.Lock()
	r := svc.refused[key]
	if r == nil {
		for len(svc.refused) >= refusedLimit && len(svc.refusedOrder) > 0 {
			svc.suppressed += svc.refused[svc.refusedOrder[0]].pending // an evicted counter's held-back refusals
			delete(svc.refused, svc.refusedOrder[0])
			svc.refusedOrder = svc.refusedOrder[1:]
		}
		r = &refusedCount{}
		svc.refused[key] = r
		svc.refusedOrder = append(svc.refusedOrder, key)
	}
	r.pending++
	var report int64
	if r.reported.IsZero() || now.Sub(r.reported) >= refusedWindow {
		if svc.rowsStart.IsZero() || now.Sub(svc.rowsStart) >= refusedWindow {
			svc.rowsStart, svc.rows = now, 0
		}
		if svc.rows < refusedRowsPerMinute {
			svc.rows++
			report, r.pending, r.reported = r.pending, 0, now
		} else {
			// The cap refuses this key a row: its refusals move to the overflow count.
			svc.suppressed += r.pending
			r.pending = 0
		}
	}
	var overflow int64
	if svc.suppressed > 0 && (svc.overflowAt.IsZero() || now.Sub(svc.overflowAt) >= refusedWindow) {
		overflow, svc.suppressed, svc.overflowAt = svc.suppressed, 0, now
	}
	svc.mu.Unlock()
	if svc.o.OnRefused != nil {
		if report > 0 {
			svc.o.OnRefused(ctx, key, report)
		}
		if overflow > 0 {
			svc.o.OnRefused(ctx, OverflowKey, overflow)
		}
	}
}

// Authenticate resolves a session token to its link's principal and marks the session used (api.TelegramSessions).
func (svc *Service) Authenticate(ctx context.Context, token string) (*auth.Principal, error) {
	return svc.resolve(ctx, token, true)
}

// Validate is Authenticate without marking the session used: stream re-checks must not defeat the idle timeout.
func (svc *Service) Validate(ctx context.Context, token string) (*auth.Principal, error) {
	return svc.resolve(ctx, token, false)
}

func (svc *Service) resolve(ctx context.Context, token string, touch bool) (*auth.Principal, error) {
	if len(token) != tokenLen || !strings.HasPrefix(token, TokenPrefix) {
		return nil, auth.ErrUnauthorized
	}
	key := sha256.Sum256([]byte(token))
	b, botOK := svc.bot()
	lim, now := svc.limits(), svc.o.Clock.Now()
	svc.mu.Lock()
	s := svc.sessions[key]
	switch {
	case s == nil:
		svc.mu.Unlock()
		return nil, auth.ErrUnauthorized
	case !botOK || b.ID != s.botID || keyGen(b) != s.keyGen || expired(s, lim, now):
		// The bot is off, another one or has a new key, or the session timed out.
		delete(svc.sessions, key)
		svc.mu.Unlock()
		return nil, auth.ErrUnauthorized
	}
	if touch {
		s.last = now
	}
	linkID := s.linkID
	svc.mu.Unlock()
	p, err := svc.o.Principal(ctx, linkID)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			// The link was revoked, suspended or blocked, or its user disabled: the session ends with it.
			svc.mu.Lock()
			delete(svc.sessions, key)
			svc.mu.Unlock()
		}
		return nil, err
	}
	if !p.ViaTelegram() || p.TelegramLinkID != linkID || p.Limited {
		return nil, auth.ErrUnauthorized
	}
	return p, nil
}

// RevokeLink ends every session of a link.
func (svc *Service) RevokeLink(linkID string) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	for k, s := range svc.sessions {
		if s.linkID == linkID {
			delete(svc.sessions, k)
		}
	}
}

// Len returns the number of sessions held (tests and metrics).
func (svc *Service) Len() int {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return len(svc.sessions)
}

// addrKey is an address's rate-limit key: IPv6 addresses count per /64, as a host usually holds a whole one.
func addrKey(ip string) string {
	if a, err := netip.ParseAddr(ip); err == nil && a.Is6() && !a.Is4In6() {
		pfx, _ := a.Prefix(64)
		return pfx.String()
	}
	return ip
}

// keyedLimiter is a token bucket per key, with idle keys forgotten.
type keyedLimiter struct {
	mu      sync.Mutex
	lim     rate.Limit
	burst   int
	buckets map[string]*keyedBucket
	lastGC  time.Time
}

type keyedBucket struct {
	l    *rate.Limiter
	seen time.Time
}

func newKeyedLimiter(lim rate.Limit, burst int) *keyedLimiter {
	return &keyedLimiter{lim: lim, burst: burst, buckets: map[string]*keyedBucket{}}
}

func (l *keyedLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastGC) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > 30*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}
	b := l.buckets[key]
	if b == nil {
		b = &keyedBucket{l: rate.NewLimiter(l.lim, l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	return b.l.AllowN(now, 1)
}

// LinkPrincipal builds a link's principal with the current grants and policy, for a request that is authorized by
// something other than a session token (a signed download link). A revoked link, a disabled user and the like are
// auth.ErrUnauthorized, and so is a Limited principal (MFA required and not enrolled), as for a session.
func (svc *Service) LinkPrincipal(ctx context.Context, linkID string) (*auth.Principal, error) {
	p, err := svc.o.Principal(ctx, linkID)
	if err != nil {
		return nil, err
	}
	if !p.ViaTelegram() || p.TelegramLinkID != linkID || p.Limited {
		return nil, auth.ErrUnauthorized
	}
	return p, nil
}
