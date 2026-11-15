package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
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
	// Zone is the user's own IANA time zone (nil: none set, so no quiet hours and no digest).
	Zone *time.Location
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
	// OnChange is called after a reload that changed what the digest depends on (the links, their zones or preferences),
	// so the deadline loop looks at the next digest time again. Set it before Start.
	OnChange func()
	sig      string

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
	if l.OnChange != nil {
		if sig := digestSignature(out); sig != l.sig {
			l.sig = sig
			l.OnChange()
		}
	}
}

// digestSignature summarizes what the digest schedule depends on.
func digestSignature(links []Link) string {
	var b strings.Builder
	for i := range links {
		b.WriteString(links[i].ID)
		b.WriteByte('|')
		if links[i].Zone != nil {
			b.WriteString(links[i].Zone.String())
		}
		b.WriteByte('|')
		b.WriteString(links[i].Prefs.Marshal())
		b.WriteByte('\n')
	}
	return b.String()
}

func (l *Links) load(ctx context.Context) ([]Link, error) {
	rows, err := l.store.R().ListLiveTelegramLinks(ctx)
	if err != nil {
		return nil, err
	}
	zones, err := l.zones(ctx)
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
		lk := fromRow(r.ID, r.UserID, r.ChatID, r.ViaSso == 1, r.SsoUntil, r.ReconfirmDueAt, r.PrefsJson)
		lk.Zone = zones[r.UserID]
		out = append(out, lk)
	}
	return out, nil
}

// zones reads the time zones of the users with a link, in one query. A zone that isn't valid counts as none.
func (l *Links) zones(ctx context.Context) (map[string]*time.Location, error) {
	rows, err := l.store.R().ListNotificationPrefsOfLinkedUsers(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]*time.Location{}
	out := make(map[string]*time.Location, len(rows))
	for _, r := range rows {
		var p struct {
			Timezone string `json:"timezone"`
		}
		if json.Unmarshal([]byte(r.PrefsJson), &p) != nil || p.Timezone == "" {
			continue
		}
		loc, seen := byName[p.Timezone]
		if !seen {
			loc, _ = settings.LoadZone(p.Timezone)
			byName[p.Timezone] = loc
		}
		if loc != nil {
			out[r.UserID] = loc
		}
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
