// Package link links a Telegram account to a SpaceMan user (ADR-0083 decision 2, plan §3.1, TG-4b).
//
// The protocol has three steps, and a link exists only after all of them:
//
//  1. The web: a signed-in user, in a browser session with a recent re-authentication (an OIDC step-up for SSO users),
//     creates a single-use code (create-telegram-link-code). It is 32 random bytes as base64url; only its SHA-256 is
//     stored, and it expires after link_code_ttl_seconds (default 10 minutes). Creating a code cancels the user's
//     unfinished ones.
//  2. Telegram: the user sends /start <code> in a private chat with the bot. The first Telegram account to present an
//     unused, unexpired code redeems it (it is single-use from then on), and the bot asks that account to confirm,
//     naming central's host and the display name and masked e-mail of the account the code belongs to. [Confirm]
//     records the Telegram confirmation; [This wasn't me] cancels the code and is audited.
//  3. The web again: only after the Telegram confirmation does the code's page show the Telegram account, and only
//     the browser session that created the code, again with a recent re-authentication, may confirm the link
//     (confirm-telegram-link). The link is then active, with grants capped by the link policy. A re-authentication
//     rotates the session; the rotation carries the code to the new session (auth), while a logout, a revocation or
//     another sign-in leaves it unconfirmable.
//
// Why both sides confirm: a leaked code can't bind an attacker's Telegram account, because the victim must confirm in
// their own web session that names the attacker's Telegram account; an attacker's own code sent to a victim can't bind
// the victim's Telegram account to the attacker's SpaceMan account without the victim confirming in Telegram a prompt
// that names the attacker's account, and even then the link exists only as the attacker's own, capped account.
//
// Link codes and links write no change-feed entries (ADR-0078, plan §4.2): the page short-polls the code.
package link

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// Code sizes: 32 random bytes as unpadded base64url are 43 characters, within the 64 characters of a /start payload.
const (
	codeBytes = 32
	codeLen   = 43
)

// Callback data of the bot's confirmation buttons: the prefix and the code's ID (base64url SHA-256, 43 characters),
// 52 bytes in all (Telegram allows 64). The ID is not the code: it can't redeem anything.
const (
	callbackPrefix = "tglink:"
	callbackOK     = callbackPrefix + "c:"
	callbackRefuse = callbackPrefix + "x:"
)

// Code states as the API shows them. The stored states are new, redeemed, confirmed and cancelled; a code is shown
// expired once its lifetime passed before it was confirmed, and telegram_confirmed once redeemed and confirmed in the
// bot.
const (
	StateNew               = "new"                // waiting for /start
	StateRedeemed          = "redeemed"           // a Telegram account sent /start; waiting for its confirmation in the bot
	StateTelegramConfirmed = "telegram_confirmed" // confirmed in the bot; waiting for the web confirmation
	StateConfirmed         = "confirmed"          // linked
	StateCancelled         = "cancelled"
	StateExpired           = "expired"
)

// Bot messages.
const (
	textInvalid         = "This link code is invalid, already used or expired. Create a new one in SpaceMan's web UI: Account → Telegram → Link Telegram."
	textAlreadyLinked   = "This Telegram account is already linked to SpaceMan. Unlink it first to link it to another account."
	textStale           = "This link request is no longer valid."
	textTelegramConfirm = "Confirmed. Now finish in the browser where you started: it shows this Telegram account and asks you to confirm once more."
	textRefused         = "Cancelled: nothing was linked. If you didn't start this, someone sent you their own link: ignore it."
	textUsed            = "This link code was already used, so it is cancelled now. If you didn't link it yourself, don't use it."
	textUsedLinked      = "This link code was already used and can't be used again. If you didn't link it yourself, don't use it."
	textReused          = "A link request for this Telegram account was cancelled: another Telegram account presented the same code. If you didn't share it, nobody was linked."
	textUnlinked        = "Unlinked. This Telegram account no longer acts on SpaceMan."
	textUnlinkedOther   = "This Telegram account was unlinked from SpaceMan."
)

// Options configure the service.
type Options struct {
	Store *store.Store
	Auth  *auth.Service
	Audit *audit.Writer
	// Settings returns the current Telegram settings (the link policy and the code lifetime).
	Settings func() settings.Settings
	// PublicURL is central's public_url: the bot names its host in the confirmation.
	PublicURL string
	// Notify sends a text to a private chat once a link is confirmed (the bot's outbound sender); nil sends nothing.
	Notify func(chatID int64, text string)
	// Bus receives telegram.link events when a link is created, revoked or re-confirmed, so the dispatcher's cache
	// (Cache) re-reads it. Nil publishes nothing.
	Bus *eventbus.Bus
	// TopicsAvailable reports whether topics can be used now: the bot is running and has topics enabled (nil: yes).
	// TopicsEnabled reports whether messages to a private chat go to topics now under a layout (the topics Manager's EnabledFor),
	// TopicsRefused whether a chat refused topics lately and CanRead whether the subjects may read a space. They feed
	// the chat layout state of the link preferences.
	TopicsAvailable func() bool
	TopicsEnabled   func(mode string, chatID int64) bool
	TopicsRefused   func(chatID int64) bool
	CanRead         func(subjects []string, spaceID string) bool
	// VisibleSpaces lists the spaces enabled for Telegram; a space hidden from Telegram is not named in the chat layout state (nil: no filter).
	VisibleSpaces func() []string
	Clock         clock.Clock
	Log           *slog.Logger
}

// Service runs the linking protocol.
type Service struct {
	o    Options
	host string
}

// New returns the service.
func New(o Options) *Service {
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Settings == nil {
		o.Settings = settings.Defaults
	}
	host := "this SpaceMan"
	if u, err := url.Parse(o.PublicURL); err == nil && u.Host != "" {
		host = u.Host
	}
	return &Service{o: o, host: host}
}

var _ updates.Linker = (*Service)(nil)

// TelegramUser is the Telegram account that redeemed a code.
type TelegramUser struct {
	ID       int64  `json:"id" doc:"Telegram user ID"`
	Username string `json:"username" doc:"Telegram username without @ (empty when the account has none)"`
	Name     string `json:"name" doc:"The account's name as Telegram shows it"`
}

// Code is a link code as its owner sees it. The code itself appears only in the answer that created it.
type Code struct {
	ID        string    `json:"id" doc:"The code's ID (not the code): use it to read, cancel or confirm"`
	State     string    `json:"state" enum:"new,redeemed,telegram_confirmed,confirmed,cancelled,expired"`
	ExpiresAt time.Time `json:"expires_at"`
	// Code and DeepLink are returned once, by create-telegram-link-code.
	Code     string `json:"code,omitempty" doc:"The single-use code, returned only when it is created"`
	DeepLink string `json:"deep_link,omitempty" doc:"t.me link that sends /start <code> to the bot, returned only when the code is created"`
	// Telegram is the account waiting for the web confirmation (only once it confirmed in the bot).
	Telegram *TelegramUser `json:"telegram,omitempty" doc:"The Telegram account that wants to link, once it confirmed in the bot"`
	// ReuseAttempted marks a code that leaked: another Telegram account presented it after one had redeemed it.
	ReuseAttempted bool `json:"reuse_attempted,omitempty" doc:"Another Telegram account presented this code after one had redeemed it: it leaked and was cancelled. Create a new code, and treat the old one as seen by someone else"`
}

// Link is a confirmed link.
type Link struct {
	ID             string        `json:"id"`
	UserID         string        `json:"user_id"`
	Telegram       TelegramUser  `json:"telegram"`
	State          string        `json:"state" enum:"active,suspended,blocked,revoked"`
	MaxRole        string        `json:"max_role" enum:"viewer,operator,admin" doc:"The link's cap: it never has a stronger role than this in any space"`
	Grants         []authz.Grant `json:"grants" doc:"The link's capped grants; each request is also checked against the user's current grants"`
	ViaSSO         bool          `json:"via_sso" doc:"Confirmed from an SSO session with SSO grants allowed through Telegram"`
	SSOUntil       *time.Time    `json:"sso_until,omitempty" doc:"Until when the user's SSO grants count for the link"`
	ConfirmedAt    time.Time     `json:"confirmed_at"`
	ReconfirmDueAt *time.Time    `json:"reconfirm_due_at,omitempty" doc:"The link stops working at this time until it is re-confirmed from the web"`
	// NeedsReconfirm is set once reconfirm_due_at has passed.
	NeedsReconfirm bool `json:"needs_reconfirm" doc:"The re-confirmation is overdue: the link does nothing until it is re-confirmed"`
	// TimezoneSaved is set only by the confirmation: the browser's zone was saved because the user had none.
	TimezoneSaved bool `json:"timezone_saved,omitempty" doc:"The time zone sent with the confirmation was saved because you had none"`
}

func errNotFound() error { return apperr.New(apperr.CodeNotFound, "link code not found") }

// webSession refuses anything but a person's own browser session: the protocol relies on the web session's recent
// re-authentication, which API tokens skip, so tokens, the root token, Telegram and the SpaceMan MCP never link.
func webSession(p *auth.Principal) error {
	if p == nil || p.User == nil || p.SessionID == "" || p.TokenID != "" || p.RootTokenID != "" || p.ViaTelegram() || p.ViaMCP() ||
		p.Method != auth.MethodPassword && p.Method != auth.MethodOIDC {
		return apperr.New(apperr.CodeForbidden, "Your Telegram links are managed only from a signed-in browser session")
	}
	return nil
}

func hashCode(code string) []byte {
	h := sha256.Sum256([]byte(code))
	return h[:]
}

func codeID(hash []byte) string { return base64.RawURLEncoding.EncodeToString(hash) }

// parseID decodes a code ID (base64url SHA-256); anything else is not found.
func parseID(id string) ([]byte, error) {
	if len(id) != codeLen {
		return nil, errNotFound()
	}
	h, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(h) != sha256.Size {
		return nil, errNotFound()
	}
	return h, nil
}

// view shows a stored code at now.
func view(r db.TelegramLinkCode, now time.Time) *Code {
	c := &Code{ID: codeID(r.CodeHash), State: r.State, ExpiresAt: time.UnixMicro(r.ExpiresAt).UTC(), ReuseAttempted: r.ReuseAttemptedAt.Valid}
	open := r.State == StateNew || r.State == StateRedeemed
	switch {
	case open && !now.Before(time.UnixMicro(r.ExpiresAt)):
		c.State = StateExpired
	case r.State == StateRedeemed && r.TgConfirmedAt.Valid:
		c.State = StateTelegramConfirmed
	}
	if (c.State == StateTelegramConfirmed || c.State == StateConfirmed) && r.RedeemedTgUserID.Valid {
		c.Telegram = &TelegramUser{ID: r.RedeemedTgUserID.Int64, Username: r.RedeemedTgUsername, Name: r.RedeemedTgName}
	}
	return c
}

// codeAudit is the actor of the web steps.
func userActor(p *auth.Principal) audit.Actor {
	a := audit.Actor{Type: audit.ActorUser, ID: p.User.ID}
	if p.Method == auth.MethodOIDC {
		a.Via = "sso:" + p.Provider
	}
	return a
}

// botActor is the actor of the steps taken in the bot: the account isn't linked yet, so it isn't a user's action.
var botActor = audit.Actor{Type: audit.ActorSystem, ID: "telegram_bot"}

// CreateCode issues a single-use link code for the calling user's browser session and cancels their unfinished ones.
// It needs a running bot with a username (the deep link names it).
func (s *Service) CreateCode(ctx context.Context, p *auth.Principal) (*Code, error) {
	if err := webSession(p); err != nil {
		return nil, err
	}
	bot, err := s.o.Store.R().GetTelegramBot(ctx)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (bot.Enabled != 1 || bot.Username == "" || bot.BotID == 0) {
		return nil, apperr.New(apperr.CodePrecondition, "the Telegram bot is not configured and running")
	} else if err != nil {
		return nil, err
	}
	raw := make([]byte, codeBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	hash := hashCode(code)
	ttl := time.Duration(s.o.Settings().LinkCodeTTLSeconds) * time.Second
	var out *Code
	err = s.o.Store.W().TxNamed(ctx, "telegram.link_code.create", func(tx *store.Tx) error {
		replaced, err := tx.Q.CancelOpenTelegramLinkCodes(ctx, p.User.ID)
		if err != nil {
			return err
		}
		r := db.TelegramLinkCode{CodeHash: hash, UserID: p.User.ID, CreatedSessionMethod: p.Method, CreatedSessionID: p.SessionID,
			CreatedAt: tx.NowMicros(), ExpiresAt: tx.Now.Add(ttl).UnixMicro(), State: StateNew}
		if err := tx.Q.InsertTelegramLinkCode(ctx, db.InsertTelegramLinkCodeParams{CodeHash: r.CodeHash, UserID: r.UserID,
			CreatedSessionMethod: r.CreatedSessionMethod, CreatedSessionID: r.CreatedSessionID, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt}); err != nil {
			return err
		}
		if _, err := s.o.Audit.Append(ctx, tx, audit.Event{Actor: userActor(p), Action: "telegram.link_code.created",
			Target: audit.Target{Type: "telegram_link_code", ID: codeID(hash)}, Details: map[string]any{"expires_in": ttl.String(), "replaced": replaced}}); err != nil {
			return err
		}
		out = view(r, tx.Now)
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Code = code
	out.DeepLink = "https://t.me/" + url.PathEscape(bot.Username) + "?start=" + code
	return out, nil
}

// ownCode reads a code of the calling user (any other user's code is not found).
func (s *Service) ownCode(ctx context.Context, q *db.Queries, p *auth.Principal, id string) (db.TelegramLinkCode, error) {
	hash, err := parseID(id)
	if err != nil {
		return db.TelegramLinkCode{}, err
	}
	r, err := q.GetTelegramLinkCode(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) || err == nil && r.UserID != p.User.ID {
		return db.TelegramLinkCode{}, errNotFound()
	}
	return r, err
}

// GetCode returns one of the calling user's codes (the page polls it).
func (s *Service) GetCode(ctx context.Context, p *auth.Principal, id string) (*Code, error) {
	if err := webSession(p); err != nil {
		return nil, err
	}
	r, err := s.ownCode(ctx, s.o.Store.R(), p, id)
	if err != nil {
		return nil, err
	}
	return view(r, s.o.Clock.Now()), nil
}

// CancelCode cancels one of the calling user's unfinished codes (cancelling a finished one is a no-op).
func (s *Service) CancelCode(ctx context.Context, p *auth.Principal, id string) (*Code, error) {
	if err := webSession(p); err != nil {
		return nil, err
	}
	var out *Code
	err := s.o.Store.W().TxNamed(ctx, "telegram.link_code.cancel", func(tx *store.Tx) error {
		r, err := s.ownCode(ctx, tx.Q, p, id)
		if err != nil {
			return err
		}
		n, err := tx.Q.CancelTelegramLinkCode(ctx, r.CodeHash)
		if err != nil {
			return err
		}
		if n == 1 {
			r.State = StateCancelled
			if _, err := s.o.Audit.Append(ctx, tx, audit.Event{Actor: userActor(p), Action: "telegram.link_code.cancelled",
				Target: audit.Target{Type: "telegram_link_code", ID: codeID(r.CodeHash)}}); err != nil {
				return err
			}
		}
		out = view(r, tx.Now)
		return nil
	})
	return out, err
}

// capRole resolves the cap a link asks for against the link policy: empty takes link_default_max_role; viewer and
// operator are allowed up to that default (it is a ceiling); admin ("same as my grants") only when
// link_allow_admin_cap is on.
func capRole(requested string, pol settings.Settings) (authz.Role, error) {
	def := authz.Role(pol.LinkDefaultMaxRole)
	if !def.Valid() || def == authz.RoleAdmin {
		def = authz.RoleViewer // never wider than the documented choices, whatever is stored
	}
	switch authz.Role(requested) {
	case "":
		return def, nil
	case authz.RoleViewer:
		return authz.RoleViewer, nil
	case authz.RoleOperator:
		if def == authz.RoleOperator {
			return authz.RoleOperator, nil
		}
		return "", apperr.New(apperr.CodeForbidden, "the link policy caps new links at viewer")
	case authz.RoleAdmin:
		if pol.LinkAllowAdminCap {
			return authz.RoleAdmin, nil
		}
		return "", apperr.New(apperr.CodeForbidden, "the link policy does not allow the admin cap")
	}
	v := &apperr.Validation{}
	v.Add("max_role", "must be viewer, operator or admin")
	return "", v.Err()
}

// Confirm activates a link from the web: the code must belong to the caller, have been created by this browser
// session (or the session it was rotated from), have been redeemed with /start and confirmed in the bot, and not have expired. The Telegram account must
// not have another live link, and the user must be under links_per_user. maxRole is the cap the user picked (empty:
// the policy default). timezone is the browser's IANA zone: it is saved as the user's zone only if they have none
// (plan §2.5), and an invalid one is ignored. Once it commits, the bot tells the chat it is linked and the dispatcher's
// cache learns the link.
func (s *Service) Confirm(ctx context.Context, p *auth.Principal, id, maxRole, timezone string) (*Link, error) {
	if err := webSession(p); err != nil {
		return nil, err
	}
	pol := s.o.Settings()
	role, err := capRole(maxRole, pol)
	if err != nil {
		return nil, err
	}
	sso := p.Method == auth.MethodOIDC && pol.LinkSSOGrants
	if !settings.ValidZone(timezone) {
		timezone = "" // an invalid zone is ignored; validating here keeps zone-file reads out of the write transaction
	}
	var out *Link
	err = s.o.Store.W().TxNamed(ctx, "telegram.link.confirm", func(tx *store.Tx) error {
		r, err := s.ownCode(ctx, tx.Q, p, id)
		if err != nil {
			return err
		}
		if r.CreatedSessionID == "" || r.CreatedSessionID != p.SessionID {
			return apperr.New(apperr.CodeForbidden, "confirm the link in the browser session that created the code")
		}
		switch view(r, tx.Now).State {
		case StateTelegramConfirmed:
		case StateNew, StateRedeemed:
			return apperr.New(apperr.CodePrecondition, "the link has not been confirmed in Telegram yet")
		case StateExpired:
			return apperr.New(apperr.CodePrecondition, "the link code expired; create a new one")
		default:
			return apperr.New(apperr.CodeConflict, "the link code was already used or cancelled")
		}
		bot, err := tx.Q.GetTelegramBot(ctx)
		if errors.Is(err, sql.ErrNoRows) || err == nil && bot.BotID == 0 {
			return apperr.New(apperr.CodePrecondition, "the Telegram bot is not configured")
		} else if err != nil {
			return err
		}
		tgUser := r.RedeemedTgUserID.Int64
		if _, err := tx.Q.GetLiveTelegramLinkByTgUser(ctx, tgUser); err == nil {
			return apperr.New(apperr.CodeConflict, "this Telegram account is already linked")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		n, err := tx.Q.CountLiveTelegramLinksByUser(ctx, p.User.ID)
		if err != nil {
			return err
		}
		if n >= pol.LinksPerUser {
			return apperr.New(apperr.CodeConflict, "you already have as many Telegram links as the policy allows; unlink one first")
		}
		if done, err := tx.Q.CompleteTelegramLinkCode(ctx, db.CompleteTelegramLinkCodeParams{CodeHash: r.CodeHash, Now: tx.NowMicros()}); err != nil {
			return err
		} else if done != 1 {
			return apperr.New(apperr.CodeConflict, "the link code was already used or cancelled")
		}
		linkID := ids.New(ids.TelegramLink)
		var ssoUntil sql.NullInt64
		if sso {
			ssoUntil = sql.NullInt64{Int64: p.ExpiresAt.UnixMicro(), Valid: true}
		}
		reconfirm := sql.NullInt64{Int64: tx.Now.Add(time.Duration(pol.LinkReconfirmDays) * 24 * time.Hour).UnixMicro(), Valid: true}
		if err := tx.Q.InsertTelegramLink(ctx, db.InsertTelegramLinkParams{ID: linkID, UserID: p.User.ID, BotID: bot.BotID, TgUserID: tgUser,
			TgUsername: r.RedeemedTgUsername, TgName: r.RedeemedTgName, ChatID: tgUser, ViaSso: b2i(sso), SsoUntil: ssoUntil,
			MaxRole: string(role), Now: tx.NowMicros(), ReconfirmDueAt: reconfirm}); err != nil {
			return err
		}
		actor := userActor(p)
		grants, err := s.o.Auth.GrantTelegramLinkTx(ctx, tx, linkID, p.User.ID, sso, role, actor)
		if err != nil {
			return err
		}
		if _, err := s.o.Audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "telegram.link.created",
			Target: audit.Target{Type: "telegram_link", ID: linkID}, Details: map[string]any{"code_id": codeID(r.CodeHash), "tg_user_id": tgUser,
				"max_role": string(role), "grants": grants, "via_sso": sso}}); err != nil {
			return err
		}
		l, err := tx.Q.GetTelegramLink(ctx, linkID)
		if err != nil {
			return err
		}
		out = linkOut(l, grants, tx.Now)
		if out.TimezoneSaved, err = saveTimezoneIfUnset(ctx, tx, p.User.ID, timezone); err != nil {
			return err
		}
		s.publishAfterCommit(tx, linkID)
		if s.o.Notify != nil {
			text := "Linked: this Telegram account now acts as " + displayName(p.User) + " on SpaceMan at " + s.host + "."
			tx.AfterCommit(func() { s.o.Notify(tgUser, text) })
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func linkOut(l db.TelegramLink, grants []authz.Grant, now time.Time) *Link {
	out := &Link{ID: l.ID, UserID: l.UserID, Telegram: TelegramUser{ID: l.TgUserID, Username: l.TgUsername, Name: l.TgName}, State: l.State,
		MaxRole: l.MaxRole, Grants: grants, ViaSSO: l.ViaSso == 1}
	if out.Grants == nil {
		out.Grants = []authz.Grant{}
	}
	if l.SsoUntil.Valid {
		v := time.UnixMicro(l.SsoUntil.Int64).UTC()
		out.SSOUntil = &v
	}
	if l.ConfirmedAt.Valid {
		out.ConfirmedAt = time.UnixMicro(l.ConfirmedAt.Int64).UTC()
	}
	if l.ReconfirmDueAt.Valid {
		v := time.UnixMicro(l.ReconfirmDueAt.Int64).UTC()
		out.ReconfirmDueAt = &v
		out.NeedsReconfirm = l.State == "active" && !now.Before(v)
	}
	return out
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Claims reports whether an update is a linking step: /start with a payload, or a tap on a link confirmation button.
func (s *Service) Claims(u *botapi.Update) bool {
	switch {
	case u.Message != nil:
		_, ok := startPayload(u.Message.Text)
		return ok
	case u.CallbackQuery != nil:
		return strings.HasPrefix(u.CallbackQuery.Data, callbackPrefix)
	}
	return false
}

// startPayload returns the payload of "/start <payload>" (a deep link's start parameter).
func startPayload(text string) (string, bool) {
	rest, ok := strings.CutPrefix(text, "/start ")
	if !ok {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	return rest, rest != ""
}

// HandleUnlinked runs a linking step from the bot (updates.Linker). from is the update's sender in a private chat
// (the dispatcher checked it). Every failure has the same vague answer, so the bot reveals nothing about codes.
func (s *Service) HandleUnlinked(ctx context.Context, u botapi.Update, from botapi.User) updates.LinkReply {
	switch {
	case u.Message != nil:
		payload, _ := startPayload(u.Message.Text)
		return s.redeem(ctx, payload, from)
	case u.CallbackQuery != nil:
		data := u.CallbackQuery.Data
		if id, ok := strings.CutPrefix(data, callbackOK); ok {
			return s.confirmInTelegram(ctx, id, from)
		}
		if id, ok := strings.CutPrefix(data, callbackRefuse); ok {
			return s.refuse(ctx, id, from)
		}
		return updates.LinkReply{CallbackText: textStale, ClearKeyboard: true}
	}
	return updates.LinkReply{}
}

// errRollback undoes a transaction whose reply is already decided.
var errRollback = errors.New("telegram link: rolled back")

func refusal(text string) updates.LinkReply { return updates.LinkReply{Text: text, Bounded: true} }

// redeem handles /start <code>: the first Telegram account to present an unused, unexpired code takes it, and is
// asked to confirm in the bot.
func (s *Service) redeem(ctx context.Context, code string, from botapi.User) updates.LinkReply {
	if len(code) != codeLen {
		return refusal(textInvalid)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(code); err != nil || len(raw) != codeBytes {
		return refusal(textInvalid)
	}
	hash := hashCode(code)
	var reply updates.LinkReply
	err := s.o.Store.W().TxNamed(ctx, "telegram.link_code.redeem", func(tx *store.Tx) error {
		if _, err := tx.Q.GetLiveTelegramLinkByTgUser(ctx, from.ID); err == nil {
			reply = refusal(textAlreadyLinked) // the code stays unused
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		n, err := tx.Q.RedeemTelegramLinkCode(ctx, db.RedeemTelegramLinkCodeParams{TgUserID: sql.NullInt64{Int64: from.ID, Valid: true},
			TgUsername: clean(from.Username, 64), TgName: clean(strings.TrimSpace(from.FirstName+" "+from.LastName), 128),
			Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, CodeHash: hash})
		if err != nil {
			return err
		}
		if n != 1 {
			reply, err = s.reused(ctx, tx, hash, from)
			return err
		}
		r, err := tx.Q.GetTelegramLinkCode(ctx, hash)
		if err != nil {
			return err
		}
		ur, err := tx.Q.GetUser(ctx, r.UserID)
		if err != nil {
			return err
		}
		if ur.Enabled != 1 {
			reply = refusal(textInvalid)
			return errRollback // a disabled user's code is never redeemed
		}
		if _, err := s.o.Audit.Append(ctx, tx, audit.Event{Actor: botActor, Action: "telegram.link_code.redeemed",
			Target: audit.Target{Type: "telegram_link_code", ID: codeID(hash)}, Details: map[string]any{"user_id": r.UserID, "tg_user_id": from.ID}}); err != nil {
			return err
		}
		reply = s.prompt(codeID(hash), displayName(&auth.User{DisplayName: ur.DisplayName, ID: ur.ID}), maskEmail(ur.Email.String))
		return nil
	})
	if err != nil && !errors.Is(err, errRollback) {
		s.o.Log.Warn("telegram: redeeming a link code failed", slog.String("error", err.Error()))
		return refusal(textInvalid)
	}
	return reply
}

// reused handles a /start whose code could not be redeemed. A code another Telegram account already redeemed, and
// that is still within its lifetime, has been seen by someone besides its owner: it is cancelled and flagged (the
// owner's confirm page warns, a code still waiting for the web confirmation can no longer be confirmed), the account
// that redeemed it is told, the sender is told the code was used, and the attempt is audited. Anything else is the
// same vague refusal as a code that never existed.
func (s *Service) reused(ctx context.Context, tx *store.Tx, hash []byte, from botapi.User) (updates.LinkReply, error) {
	r, err := tx.Q.GetTelegramLinkCode(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return refusal(textInvalid), nil
	} else if err != nil {
		return updates.LinkReply{}, err
	}
	if !r.RedeemedTgUserID.Valid || r.RedeemedTgUserID.Int64 == from.ID || !tx.Now.Before(time.UnixMicro(r.ExpiresAt)) ||
		r.State != StateRedeemed && r.State != StateConfirmed {
		return refusal(textInvalid), nil
	}
	cancelled := false
	if r.State == StateRedeemed {
		n, err := tx.Q.MarkTelegramLinkCodeReused(ctx, db.MarkTelegramLinkCodeReusedParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			CodeHash: hash, TgUserID: sql.NullInt64{Int64: from.ID, Valid: true}})
		if err != nil {
			return updates.LinkReply{}, err
		}
		cancelled = n == 1
	}
	if _, err := s.o.Audit.Append(ctx, tx, audit.Event{Actor: botActor, Action: "telegram.link_code.reused", Outcome: audit.OutcomeDenied,
		Target: audit.Target{Type: "telegram_link_code", ID: codeID(hash)}, Details: map[string]any{"user_id": r.UserID, "tg_user_id": from.ID,
			"redeemed_by_tg_user_id": r.RedeemedTgUserID.Int64, "cancelled": cancelled}}); err != nil {
		return updates.LinkReply{}, err
	}
	if cancelled && s.o.Notify != nil {
		redeemer := r.RedeemedTgUserID.Int64
		tx.AfterCommit(func() { s.o.Notify(redeemer, textReused) })
	}
	if !cancelled {
		return refusal(textUsedLinked), nil // already confirmed: the link stands, nothing was cancelled
	}
	return refusal(textUsed), nil
}

// prompt is the bot's confirmation: central's host and the account the code belongs to, in bold, with the buttons.
func (s *Service) prompt(id, name, email string) updates.LinkReply {
	var b strings.Builder
	var ents []botapi.MessageEntity
	bold := func(t string) {
		ents = append(ents, botapi.MessageEntity{Type: "bold", Offset: utf16Len(b.String()), Length: utf16Len(t)})
		b.WriteString(t)
	}
	b.WriteString("Link this Telegram account to SpaceMan at ")
	bold(s.host)
	b.WriteString(" as ")
	bold(name)
	// The e-mail is on its own labelled line: the name is user-controlled (one line, no newlines: see clean), so it
	// can't pass for, or imitate, the masked address.
	b.WriteString("?\nE-mail: ")
	if email != "" {
		b.WriteString(email)
	} else {
		b.WriteString("(no e-mail)")
	}
	b.WriteString("\n\nOnly confirm if you started this in your own browser just now. If someone sent you this link, tap \"This wasn't me\".")
	return updates.LinkReply{Text: b.String(), Entities: ents, Markup: &botapi.InlineKeyboardMarkup{InlineKeyboard: [][]botapi.InlineKeyboardButton{{
		{Text: "Confirm", CallbackData: callbackOK + id, Style: botapi.StyleSuccess},
		{Text: "This wasn't me", CallbackData: callbackRefuse + id, Style: botapi.StyleDanger},
	}}}}
}

// confirmInTelegram records [Confirm]: only from the Telegram account that redeemed the code, once, before it expires.
func (s *Service) confirmInTelegram(ctx context.Context, id string, from botapi.User) updates.LinkReply {
	stale := updates.LinkReply{CallbackText: textStale, ClearKeyboard: true}
	hash, err := parseID(id)
	if err != nil {
		return stale
	}
	var reply updates.LinkReply
	err = s.o.Store.W().TxNamed(ctx, "telegram.link_code.telegram_confirm", func(tx *store.Tx) error {
		n, err := tx.Q.ConfirmTelegramLinkCodeInTelegram(ctx, db.ConfirmTelegramLinkCodeInTelegramParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			CodeHash: hash, TgUserID: sql.NullInt64{Int64: from.ID, Valid: true}})
		if err != nil {
			return err
		}
		if n != 1 {
			reply = stale
			return nil
		}
		if _, err := s.o.Audit.Append(ctx, tx, audit.Event{Actor: botActor, Action: "telegram.link_code.telegram_confirmed",
			Target: audit.Target{Type: "telegram_link_code", ID: id}, Details: map[string]any{"tg_user_id": from.ID}}); err != nil {
			return err
		}
		reply = updates.LinkReply{Text: textTelegramConfirm, CallbackText: "Confirmed", ClearKeyboard: true}
		return nil
	})
	if err != nil {
		s.o.Log.Warn("telegram: confirming a link code failed", slog.String("error", err.Error()))
		return stale
	}
	return reply
}

// refuse records [This wasn't me]: the code is cancelled (only by the Telegram account that redeemed it, before the
// web confirmation) and the refusal is audited, so the account's owner can see someone tried.
func (s *Service) refuse(ctx context.Context, id string, from botapi.User) updates.LinkReply {
	stale := updates.LinkReply{CallbackText: textStale, ClearKeyboard: true}
	hash, err := parseID(id)
	if err != nil {
		return stale
	}
	var reply updates.LinkReply
	err = s.o.Store.W().TxNamed(ctx, "telegram.link_code.refuse", func(tx *store.Tx) error {
		r, err := tx.Q.GetTelegramLinkCode(ctx, hash)
		if errors.Is(err, sql.ErrNoRows) {
			reply = stale
			return nil
		} else if err != nil {
			return err
		}
		if r.State != StateRedeemed || !r.RedeemedTgUserID.Valid || r.RedeemedTgUserID.Int64 != from.ID {
			reply = stale
			return nil
		}
		if n, err := tx.Q.CancelTelegramLinkCode(ctx, hash); err != nil {
			return err
		} else if n != 1 {
			reply = stale
			return nil
		}
		if _, err := s.o.Audit.Append(ctx, tx, audit.Event{Actor: botActor, Action: "telegram.link_code.refused", Outcome: audit.OutcomeDenied,
			Target: audit.Target{Type: "telegram_link_code", ID: id}, Details: map[string]any{"user_id": r.UserID, "tg_user_id": from.ID}}); err != nil {
			return err
		}
		reply = updates.LinkReply{Text: textRefused, CallbackText: "Cancelled", ClearKeyboard: true}
		return nil
	})
	if err != nil {
		s.o.Log.Warn("telegram: refusing a link code failed", slog.String("error", err.Error()))
		return stale
	}
	return reply
}

// displayName is the name the bot shows for a SpaceMan account.
func displayName(u *auth.User) string {
	if n := clean(u.DisplayName, 64); n != "" {
		return n
	}
	return u.ID
}

// maskEmail keeps an address's first character and its domain: j***@example.com.
func maskEmail(e string) string {
	local, domain, ok := strings.Cut(clean(e, 254), "@")
	if !ok || local == "" || domain == "" {
		return ""
	}
	r, _ := utf8.DecodeRuneInString(local)
	return string(r) + "***@" + domain
}

// maxMarks is how many combining marks may follow one another: real scripts stack two or three, and a longer run
// ("zalgo") only smears over the lines around it.
const maxMarks = 2

// fillers are the characters that draw as empty space but are not spaces (Braille blank, Hangul fillers), which
// would otherwise let a name pass as blank or push the real text out of view.
func isFiller(r rune) bool {
	switch r {
	case 0x2800, 0x3164, 0x115F, 0x1160, 0xFFA0:
		return true
	}
	return false
}

// clean makes untrusted text safe to show on one line: control and format characters and invisible fillers become
// spaces, a run of combining marks keeps its first maxMarks, runs of spaces collapse, and it is cut to max runes.
func clean(s string, maxRunes int) string {
	var b strings.Builder
	marks := 0
	for _, r := range s {
		switch {
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError || isFiller(r):
			r = ' '
		case unicode.Is(unicode.M, r): // Mn, Mc and Me: interleaving the kinds must not reset the count
			if marks++; marks > maxMarks {
				continue
			}
			b.WriteRune(r)
			continue
		}
		marks = 0
		b.WriteRune(r)
	}
	s = strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(s) > maxRunes {
		s = string([]rune(s)[:maxRunes])
	}
	return s
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }
