package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// TelegramSubject is the authorization subject of a Telegram link ("telegram:<link id>", ADR-0083 §3.1). It holds the
// link's capped grants; a Telegram principal acts as it and as its user (Principal.Subjects), so the link can never
// do more than the user currently may.
func TelegramSubject(linkID string) string { return "telegram:" + linkID }

// TelegramLinkActive is the only link state a Telegram principal is built for.
const TelegramLinkActive = "active"

// TelegramOptions are the policy inputs of TelegramPrincipal.
type TelegramOptions struct {
	// SSOGrants is the link_sso_grants setting: a link confirmed from an SSO session acts with the user's SSO subject
	// until that session's absolute expiry. Off (the default), every link acts with the user's local subject only.
	SSOGrants bool
}

// TelegramPrincipal builds the principal of a person acting through their Telegram link (ADR-0083 §3.2). The link must
// be active and its user enabled; anything else is ErrUnauthorized. Callers build it for every update or request, so a
// revoked link, a disabled user or a passed sso_until applies at once.
//
// The principal never carries a token ID or a root token ID, and its AuthAt and MFAAt stay zero: it never passes a
// step-up check (RequireRecentAuth), even if a refusal in front of that check were missing. A user who must still
// enroll MFA gets a Limited principal, as their web session would be.
func (svc *Service) TelegramPrincipal(ctx context.Context, linkID string, o TelegramOptions) (*Principal, error) {
	if linkID == "" || len(linkID) > 64 {
		return nil, ErrUnauthorized
	}
	gen := svc.rev.gen.Load()
	l, err := svc.s.R().GetTelegramLinkAuth(ctx, linkID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if l.State != TelegramLinkActive {
		return nil, ErrUnauthorized
	}
	u, err := svc.Get(ctx, l.UserID)
	if err != nil || !u.Enabled {
		return nil, ErrUnauthorized
	}
	sso := o.SSOGrants && l.ViaSso == 1 && l.SsoUntil.Valid && svc.opts.Clock.Now().Before(time.UnixMicro(l.SsoUntil.Int64))
	enroll := u.MFARequired && !u.MFAEnrolled
	return &Principal{User: u, Method: MethodTelegram, TelegramLinkID: l.ID, TelegramSSO: sso, NeedsEnroll: enroll, Limited: enroll,
		revGen: gen}, nil
}

// TelegramLinkChangedAfterCommit has the open streams of a Telegram link re-check their authorization once tx commits.
// Every change that revokes, suspends or narrows a link calls it (TG-4b).
func (svc *Service) TelegramLinkChangedAfterCommit(tx *store.Tx, linkID string) {
	svc.revokedAfterCommit(tx, revocation{telegramLinkID: linkID})
}
