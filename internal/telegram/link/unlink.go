package link

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// How a link ended, for the audit trail.
const (
	viaWeb   = "web"
	viaBot   = "bot"
	viaAdmin = "admin"
)

func errLinkNotFound() error { return apperr.New(apperr.CodeNotFound, "Telegram link not found") }

// publishAfterCommit tells the dispatcher's cache (and any other link cache) over the in-process eventbus that a link
// changed once tx commits. Telegram's own state never reaches the change feed (plan §4.2): a NULL-space change would go
// to every watch stream.
func (s *Service) publishAfterCommit(tx *store.Tx, linkID string) {
	if s.o.Bus == nil {
		return
	}
	tx.AfterCommit(func() { s.o.Bus.Publish(eventbus.Event{Topic: eventbus.TopicTelegramLink, Key: linkID}) })
}

// grantsOf reads a link's capped grants.
func grantsOf(ctx context.Context, q *db.Queries, linkID string) ([]authz.Grant, error) {
	rows, err := q.ListRoleGrantsBySubject(ctx, auth.TelegramSubject(linkID))
	if err != nil {
		return nil, err
	}
	out := make([]authz.Grant, 0, len(rows))
	for _, r := range rows {
		out = append(out, authz.Grant{Subject: r.Subject, Role: authz.Role(r.Role), Domain: r.Domain, Source: r.Source, SourceRef: r.SourceRef})
	}
	return out, nil
}

// ListMine returns the calling user's live links.
func (s *Service) ListMine(ctx context.Context, p *auth.Principal) ([]*Link, error) {
	if p == nil || p.User == nil || p.ViaTelegram() {
		return nil, apperr.New(apperr.CodeForbidden, "only a signed-in person lists their own Telegram links")
	}
	rows, err := s.o.Store.R().ListLiveTelegramLinksByUser(ctx, p.User.ID)
	if err != nil {
		return nil, err
	}
	return s.views(ctx, rows)
}

// List returns every live link (the administrator's view).
func (s *Service) List(ctx context.Context) ([]*Link, error) {
	rows, err := s.o.Store.R().ListLiveTelegramLinks(ctx)
	if err != nil {
		return nil, err
	}
	return s.views(ctx, rows)
}

func (s *Service) views(ctx context.Context, rows []db.TelegramLink) ([]*Link, error) {
	now := s.o.Clock.Now()
	out := make([]*Link, 0, len(rows))
	for _, r := range rows {
		g, err := grantsOf(ctx, s.o.Store.R(), r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, linkOut(r, g, now))
	}
	return out, nil
}

// revoke ends a link in one transaction: the link, its grants, the audit record and, after commit, the cache's
// invalidation and a notice in the chat. check decides whether this caller may end that link; ending an already ended
// link changes nothing.
func (s *Service) revoke(ctx context.Context, linkID, via string, actor audit.Actor, notice string, check func(db.TelegramLink) bool) error {
	return s.o.Store.W().TxNamed(ctx, "telegram.link.revoke", func(tx *store.Tx) error {
		l, err := tx.Q.GetTelegramLink(ctx, linkID)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !check(l) {
			return errLinkNotFound()
		} else if err != nil {
			return err
		}
		ended, err := s.o.Auth.RevokeTelegramLinkTx(ctx, tx, linkID, via+":"+actor.ID)
		if err != nil || !ended {
			return err
		}
		if _, err := s.o.Audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "telegram.link.revoked",
			Target: audit.Target{Type: "telegram_link", ID: linkID}, Details: map[string]any{"path": via, "user_id": l.UserID, "tg_user_id": l.TgUserID}}); err != nil {
			return err
		}
		s.publishAfterCommit(tx, linkID)
		if s.o.Notify != nil && notice != "" {
			chat := l.ChatID
			tx.AfterCommit(func() { s.o.Notify(chat, notice) })
		}
		return nil
	})
}

// UnlinkMine ends one of the calling user's own links from the web (another user's link is not found).
func (s *Service) UnlinkMine(ctx context.Context, p *auth.Principal, linkID string) error {
	if p == nil || p.User == nil || p.ViaTelegram() {
		return apperr.New(apperr.CodeForbidden, "only a signed-in person unlinks their own Telegram account")
	}
	return s.revoke(ctx, linkID, viaWeb, userActor(p), textUnlinkedOther, func(l db.TelegramLink) bool { return l.UserID == p.User.ID })
}

// UnlinkAny ends any link: the administrator's path (the API authorizes user:write with a step-up).
func (s *Service) UnlinkAny(ctx context.Context, actor audit.Actor, linkID string) error {
	return s.revoke(ctx, linkID, viaAdmin, actor, textUnlinkedOther, func(db.TelegramLink) bool { return true })
}

// Unlink is /unlink in the bot (updates.Unlinker): the linked account ends its own link. The reply is the bot's text.
func (s *Service) Unlink(ctx context.Context, l updates.Link) string {
	actor := audit.Actor{Type: audit.ActorUser, ID: l.UserID, Via: "telegram"}
	err := s.revoke(ctx, l.ID, viaBot, actor, "", func(r db.TelegramLink) bool { return r.UserID == l.UserID })
	if err != nil {
		s.o.Log.Warn("telegram: unlinking from the bot failed", "error", err.Error())
		return "Unlinking failed. Try again, or unlink in SpaceMan's web UI: Account → Telegram."
	}
	return textUnlinked
}

var _ updates.Unlinker = (*Service)(nil)

// Reconfirm extends one of the caller's links by link_reconfirm_days from now (a link that wasn't re-confirmed by its
// due time does nothing until it is). It needs a browser session with a recent re-authentication.
func (s *Service) Reconfirm(ctx context.Context, p *auth.Principal, linkID string) (*Link, error) {
	if err := webSession(p); err != nil {
		return nil, err
	}
	days := s.o.Settings().LinkReconfirmDays
	var out *Link
	err := s.o.Store.W().TxNamed(ctx, "telegram.link.reconfirm", func(tx *store.Tx) error {
		l, err := tx.Q.GetTelegramLink(ctx, linkID)
		if errors.Is(err, sql.ErrNoRows) || err == nil && l.UserID != p.User.ID {
			return errLinkNotFound()
		} else if err != nil {
			return err
		}
		due := tx.Now.Add(time.Duration(days) * 24 * time.Hour)
		n, err := tx.Q.ReconfirmTelegramLink(ctx, db.ReconfirmTelegramLinkParams{ReconfirmDueAt: sql.NullInt64{Int64: due.UnixMicro(), Valid: true},
			Now: tx.NowMicros(), ID: linkID})
		if err != nil {
			return err
		}
		if n != 1 {
			return apperr.New(apperr.CodeConflict, "the Telegram link was unlinked")
		}
		if _, err := s.o.Audit.Append(ctx, tx, audit.Event{Actor: userActor(p), Action: "telegram.link.reconfirmed",
			Target: audit.Target{Type: "telegram_link", ID: linkID}, Details: map[string]any{"due_at": due.UTC().Format(time.RFC3339)}}); err != nil {
			return err
		}
		l, err = tx.Q.GetTelegramLink(ctx, linkID)
		if err != nil {
			return err
		}
		g, err := grantsOf(ctx, tx.Q, linkID)
		if err != nil {
			return err
		}
		out = linkOut(l, g, tx.Now)
		s.publishAfterCommit(tx, linkID)
		return nil
	})
	return out, err
}

// saveTimezoneIfUnset stores zone as the user's time zone unless they already have one (plan §2.5: the browser's zone
// pre-fills an empty setting and never overwrites a chosen one). An empty or invalid zone is ignored. It reports
// whether it saved. The stored preferences keep every other field: only "timezone" is set, and absent fields read as
// their defaults.
func saveTimezoneIfUnset(ctx context.Context, tx *store.Tx, userID, zone string) (bool, error) {
	if zone == "" { // validated by the caller, outside the transaction (loading a zone reads files)
		return false, nil
	}
	raw, err := json.Marshal(zone)
	if err != nil {
		return false, err
	}
	row, err := tx.Q.GetNotificationPrefs(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		n, err := tx.Q.InsertNotificationPrefs(ctx, db.InsertNotificationPrefsParams{UserID: userID, PrefsJson: `{"timezone":` + string(raw) + `}`, UpdatedAt: tx.NowMicros()})
		return n == 1, err
	} else if err != nil {
		return false, err
	}
	m := map[string]json.RawMessage{}
	if json.Unmarshal([]byte(row.PrefsJson), &m) != nil {
		return false, nil //nolint:nilerr // unreadable stored preferences are left for the user to fix, the link still confirms
	}
	if m == nil { // stored "null"
		m = map[string]json.RawMessage{}
	}
	var cur string
	_ = json.Unmarshal(m["timezone"], &cur)
	if cur != "" {
		return false, nil
	}
	m["timezone"] = raw
	b, err := json.Marshal(m)
	if err != nil {
		return false, err
	}
	n, err := tx.Q.UpdateNotificationPrefs(ctx, db.UpdateNotificationPrefsParams{PrefsJson: string(b), UpdatedAt: tx.NowMicros(), UserID: userID, Version: row.Version})
	return n == 1, err
}
