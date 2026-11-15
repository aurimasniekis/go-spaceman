package link

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

// A link's own preferences (ADR-0083 amendment TG-10-4). A Telegram principal reads and replaces the notification
// overrides of the one link it acts through, and its user's time zone. The link is never named by the request: it is
// the authenticated principal's TelegramLinkID, and it must be its user's and active (ownLink). Nothing here reaches
// another link, a space's or the global Telegram settings, grants, the link's cap or spaces, or any other field of the
// user's preferences.

// Prefs is a link's notification preferences as its own Telegram principal sees them.
type Prefs struct {
	LinkID    string                        `json:"link_id" doc:"The link these preferences belong to: always the caller's own"`
	Overrides settings.LinkPrefs            `json:"overrides" doc:"What this link changed; every field left out follows the defaults"`
	Defaults  settings.NotificationDefaults `json:"defaults" doc:"The administrator's defaults the link follows where it changed nothing"`
	Effective settings.EffectivePrefs       `json:"effective" doc:"What the link is notified about: the defaults with its overrides applied"`
	Timezone  string                        `json:"timezone" doc:"Your IANA time zone, shared by your links and the web Account page (empty: unset, so the digest and quiet hours are off)"`
	Topics    TopicsPrefs                   `json:"topics" doc:"Topics mode (a topic per space in the private chat)"`
	Version   int64                         `json:"version" doc:"The link's version; send it as If-Match to save"`
}

// TopicsPrefs is the read-only state of topics mode (ADR-0083 decision 16). The switch is per space (a space's
// Telegram settings, space_config:write); a link only sees which of its spaces use it and whether its chat refused.
type TopicsPrefs struct {
	Available bool     `json:"available" doc:"Whether topics mode is supported (the bot is running); it is turned on per space, not per link"`
	Spaces    []string `json:"spaces" doc:"IDs of the spaces you can read that have topics on, sorted; each gets a topic in your private chat"`
	Refused   bool     `json:"refused" doc:"True while the bot is not asking your private chat for topics again because Telegram refused one (topics are off for the bot in that chat); messages go to the main chat"`
}

// PrefsInput replaces a link's overrides. A field left out (or null) follows the defaults again.
type PrefsInput struct {
	settings.LinkPrefs
	Timezone *string `json:"timezone,omitempty" maxLength:"64" doc:"Your IANA time zone, such as Europe/Vilnius; empty clears it, left out keeps it. It is your user's, so it applies to all your links and the web"`
}

// ownLinkID returns the link a principal may manage the preferences of: the link its authenticated Telegram session
// or chat update acts through, and nothing else. Any other principal (a web session, an API or root token, the
// SpaceMan MCP) has no link preferences.
func ownLinkID(p *auth.Principal) (string, error) {
	if !p.ViaTelegram() || p.User == nil || p.TokenID != "" || p.RootTokenID != "" {
		return "", apperr.New(apperr.CodeForbidden, "only a Telegram link has link preferences")
	}
	return p.TelegramLinkID, nil
}

// ownLink reads the principal's own link: it must exist, belong to the principal's user and be active. Anything else
// is not found, so a principal can't tell another link exists.
func ownLink(ctx context.Context, q *db.Queries, p *auth.Principal) (db.TelegramLink, error) {
	id, err := ownLinkID(p)
	if err != nil {
		return db.TelegramLink{}, err
	}
	l, err := q.GetTelegramLink(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (l.UserID != p.User.ID || l.State != auth.TelegramLinkActive) {
		return db.TelegramLink{}, errLinkNotFound()
	}
	return l, err
}

func (s *Service) prefsOut(l db.TelegramLink, p *auth.Principal, zone string) *Prefs {
	d := s.o.Settings().NotificationDefaults
	o := settings.ParseLinkPrefs(l.PrefsJson)
	return &Prefs{LinkID: l.ID, Overrides: o, Defaults: d, Effective: o.Effective(d), Timezone: zone, Topics: s.topicsOut(l, p), Version: l.Version}
}

// topicsOut reports topics mode for the link: only spaces the link may read are named, so it learns nothing about others.
func (s *Service) topicsOut(l db.TelegramLink, p *auth.Principal) TopicsPrefs {
	out := TopicsPrefs{Spaces: []string{}}
	if s.o.TopicSpaces == nil {
		return out
	}
	out.Available = true
	subj := p.Subjects()
	for _, id := range s.o.TopicSpaces() {
		if s.o.CanRead != nil && s.o.CanRead(subj, id) {
			out.Spaces = append(out.Spaces, id)
		}
	}
	out.Refused = s.o.TopicsRefused != nil && s.o.TopicsRefused(l.ChatID)
	return out
}

// userZone reads the user's stored time zone ("" when none or unreadable).
func userZone(ctx context.Context, q *db.Queries, userID string) (string, error) {
	row, err := q.GetNotificationPrefs(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return settings.ParseUserZone(row.PrefsJson), nil
}

// GetPrefs returns the preferences of the principal's own link.
func (s *Service) GetPrefs(ctx context.Context, p *auth.Principal) (*Prefs, error) {
	q := s.o.Store.R()
	l, err := ownLink(ctx, q, p)
	if err != nil {
		return nil, err
	}
	zone, err := userZone(ctx, q, l.UserID)
	if err != nil {
		return nil, err
	}
	return s.prefsOut(l, p, zone), nil
}

// normalizeMuted trims, de-duplicates and checks muted space IDs. Muting only ever removes notifications, so a space
// the link can't read is accepted (refusing it would tell the caller which spaces exist).
func normalizeMuted(in *[]string, v *apperr.Validation) *[]string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(*in))
	for _, id := range *in {
		id = strings.TrimSpace(id)
		if !authz.ValidDomain(id) || id == authz.Global {
			v.Add("muted_spaces", "must list space IDs")
			return nil
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return &out
}

// SetPrefs replaces the overrides of the principal's own link, and its user's time zone when in.Timezone is set.
// version is the link's version the caller read (If-Match).
func (s *Service) SetPrefs(ctx context.Context, p *auth.Principal, actor audit.Actor, version int64, in PrefsInput) (*Prefs, error) {
	if _, err := ownLinkID(p); err != nil {
		return nil, err
	}
	v := &apperr.Validation{}
	in.MutedSpaces = normalizeMuted(in.MutedSpaces, v)
	var zone *string
	if in.Timezone != nil {
		z := strings.TrimSpace(*in.Timezone)
		// Validated here, outside the write transaction (loading a zone may read files).
		if !settings.ValidZone(z) {
			v.Add("timezone", "must be an IANA time zone such as UTC or Europe/Vilnius, or empty")
		}
		zone = &z
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	if err := in.Validate(s.o.Settings().NotificationDefaults); err != nil {
		return nil, err
	}
	raw := in.Marshal()
	var out *Prefs
	err := s.o.Store.W().TxNamed(ctx, "telegram.link.prefs", func(tx *store.Tx) error {
		l, err := ownLink(ctx, tx.Q, p)
		if err != nil {
			return err
		}
		n, err := tx.Q.SetTelegramLinkPrefs(ctx, db.SetTelegramLinkPrefsParams{PrefsJson: raw, Now: tx.NowMicros(), ID: l.ID, UserID: p.User.ID, Version: version})
		if err != nil {
			return err
		}
		if n != 1 {
			return apperr.New(apperr.CodePrecondition, "the link changed since version %d; reload it", version)
		}
		zoneSaved := false
		if zone != nil {
			if zoneSaved, err = putTimezone(ctx, tx, l.UserID, *zone, false); err != nil {
				return err
			}
		}
		if _, err := s.o.Audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "telegram.link.prefs_saved",
			Target: audit.Target{Type: "telegram_link", ID: l.ID},
			Details: map[string]any{"overrides": raw != "{}", "muted_spaces": in.MutedSpaces != nil && len(*in.MutedSpaces) > 0,
				"timezone_changed": zoneSaved}}); err != nil {
			return err
		}
		if l, err = tx.Q.GetTelegramLink(ctx, l.ID); err != nil {
			return err
		}
		cur, err := userZone(ctx, tx.Q, l.UserID)
		if err != nil {
			return err
		}
		out = s.prefsOut(l, p, cur)
		// The notifier and the dispatcher re-read the link (and the user's zone) once this commits.
		s.publishAfterCommit(tx, l.ID)
		return nil
	})
	return out, err
}
