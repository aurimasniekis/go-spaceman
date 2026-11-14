package notify

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

// Link is an active, confirmed link as the notifier needs it, held in memory.
type Link struct {
	ID          string
	UserID      string
	ChatID      int64
	ViaSSO      bool
	SSOUntil    time.Time
	ReconfirmAt time.Time // zero: none
	Prefs       Prefs
}

// Prefs are a link's preference overrides.
type Prefs struct {
	settings.LinkPrefs
}

// Effective applies the defaults.
func (p Prefs) Effective(d settings.NotificationDefaults) settings.EffectivePrefs {
	return p.LinkPrefs.Effective(d)
}

// IsMuted reports whether the link muted a space.
func (p Prefs) IsMuted(spaceID string) bool {
	if p.MutedSpaces == nil {
		return false
	}
	for _, s := range *p.MutedSpaces {
		if s == spaceID {
			return true
		}
	}
	return false
}

// Usable: a link whose re-confirmation is overdue acts for nobody, like its principal (auth.TelegramPrincipal).
func (l *Link) Usable(now time.Time) bool {
	return l.ReconfirmAt.IsZero() || now.Before(l.ReconfirmAt)
}

// Subjects are the authorization subjects of the link's principal (Principal.Subjects): the link's capped grants and
// its user's, the SSO subject only while the policy and the link's SSO session allow it.
func (l *Link) Subjects(ssoGrants bool, now time.Time) []string {
	if ssoGrants && l.ViaSSO && now.Before(l.SSOUntil) {
		return []string{auth.TelegramSubject(l.ID), auth.SSOSubject(l.UserID)}
	}
	return []string{auth.TelegramSubject(l.ID), auth.UserSubject(l.UserID)}
}

// UserCheck tells whether a user may act through Telegram at all: enabled and not waiting to enroll MFA (the
// principal of a link is Limited then, and the API refuses a limited principal).
type UserCheck func(ctx context.Context, userID string) (ok bool, err error)

// Links is the in-memory list of the links that may be notified. It follows telegram.link, grants.changed and
// user.changed on the in-process bus (which only reduces latency) and reloads on an interval; a failed read empties
// it until a read works, so a revoked link is never notified because the database was unreachable.
type Links struct {
	store    *store.Store
	bus      *eventbus.Bus
	users    UserCheck
	log      *slog.Logger
	interval time.Duration

	mu    sync.RWMutex
	links []Link
}

// NewLinks returns an empty list; Start fills it.
func NewLinks(st *store.Store, bus *eventbus.Bus, users UserCheck, log *slog.Logger, interval time.Duration) *Links {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if interval <= 0 {
		interval = time.Minute
	}
	return &Links{store: st, bus: bus, users: users, log: log, interval: interval}
}

// Snapshot returns the current links. The slice is replaced, never modified, so it is safe to read without a lock.
func (l *Links) Snapshot() []Link {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.links
}

// Set replaces the links (tests).
func (l *Links) Set(links []Link) {
	l.mu.Lock()
	l.links = links
	l.mu.Unlock()
}

// Start loads the links and follows the bus until ctx ends.
func (l *Links) Start(ctx context.Context) {
	if l.store == nil {
		return
	}
	sub := l.bus.Subscribe(16, eventbus.TopicTelegramLink, eventbus.TopicGrantsChanged, eventbus.TopicUserChanged)
	l.reload(ctx)
	go func() {
		defer sub.Close()
		tick := time.NewTicker(l.interval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sub.C:
				l.reload(ctx)
			case <-tick.C:
				l.reload(ctx)
			}
		}
	}()
}

func (l *Links) reload(ctx context.Context) {
	out, err := l.load(ctx)
	if err != nil {
		l.log.Warn("telegram notifier: loading the links failed; nobody is notified until it works", slog.String("error", err.Error()))
		out = nil
	}
	l.Set(out)
}

func (l *Links) load(ctx context.Context) ([]Link, error) {
	rows, err := l.store.R().ListLiveTelegramLinks(ctx)
	if err != nil {
		return nil, err
	}
	var out []Link
	okUser := map[string]bool{}
	for _, r := range rows {
		if r.State != auth.TelegramLinkActive {
			continue
		}
		ok, seen := okUser[r.UserID]
		if !seen {
			if ok, err = l.users(ctx, r.UserID); err != nil {
				return nil, err
			}
			okUser[r.UserID] = ok
		}
		if !ok {
			continue
		}
		out = append(out, fromRow(r.ID, r.UserID, r.ChatID, r.ViaSso == 1, r.SsoUntil, r.ReconfirmDueAt, r.PrefsJson))
	}
	return out, nil
}

func fromRow(id, userID string, chat int64, sso bool, until, due sql.NullInt64, prefs string) Link {
	l := Link{ID: id, UserID: userID, ChatID: chat, ViaSSO: sso, Prefs: Prefs{settings.ParseLinkPrefs(prefs)}}
	if until.Valid {
		l.SSOUntil = time.UnixMicro(until.Int64)
	}
	if due.Valid {
		l.ReconfirmAt = time.UnixMicro(due.Int64)
	}
	return l
}
