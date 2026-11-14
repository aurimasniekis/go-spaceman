package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
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
	// A link that was not re-confirmed from the web within its interval acts for nobody until it is (TG-4b).
	if l.ReconfirmDueAt.Valid && !svc.opts.Clock.Now().Before(time.UnixMicro(l.ReconfirmDueAt.Int64)) {
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

// GrantTelegramLinkTx writes a newly confirmed link's capped grants (ADR-0083 decision 3) inside the caller's
// transaction: the grants userID holds now, read in tx (the local subject's, plus the SSO subject's when sso is set),
// each lowered to maxRole (authz.Cap), checked with authz.Covers against those grants and stored as grants of the
// subject telegram:<linkID> with source "telegram", the path API tokens use. The engine reloads after commit. It
// returns the grants written; a user without grants gets none, so the link can do nothing.
func (svc *Service) GrantTelegramLinkTx(ctx context.Context, tx *store.Tx, linkID, userID string, sso bool, maxRole authz.Role, actor audit.Actor) ([]authz.Grant, error) {
	if linkID == "" || userID == "" || !maxRole.Valid() {
		return nil, apperr.New(apperr.CodeInvalid, "invalid Telegram link cap")
	}
	subjects := []string{UserSubject(userID)}
	if sso {
		subjects = append(subjects, SSOSubject(userID))
	}
	var holder []authz.Grant
	for _, sub := range subjects {
		rows, err := tx.Q.ListRoleGrantsBySubject(ctx, sub)
		if err != nil {
			return nil, err
		}
		holder = append(holder, grantsFromRows(rows)...)
	}
	capped := authz.Cap(holder, maxRole)
	in := make([]GrantInput, 0, len(capped))
	for _, g := range capped {
		if !authz.Covers(holder, g) {
			return nil, apperr.New(apperr.CodeForbidden, "a Telegram link cannot hold more than its user")
		}
		in = append(in, GrantInput{Role: g.Role, Domain: g.Domain})
	}
	subject := TelegramSubject(linkID)
	if err := tx.Q.DeleteRoleGrantsBySubject(ctx, subject); err != nil {
		return nil, err
	}
	if err := svc.insertGrants(ctx, tx, subject, in, "telegram", actor); err != nil {
		return nil, err
	}
	for i := range capped {
		capped[i].Subject, capped[i].Source = subject, "telegram"
	}
	return capped, nil
}

// RevokeTelegramLinkTx ends a Telegram link inside the caller's transaction: the link is revoked, its capped grants
// are deleted, and after commit the authorization engine reloads and the link's open streams re-check (Mini App
// sessions end at once). It reports whether the link was live; revoking a revoked or unknown link changes nothing.
func (svc *Service) RevokeTelegramLinkTx(ctx context.Context, tx *store.Tx, linkID, revokedBy string) (bool, error) {
	n, err := tx.Q.RevokeTelegramLink(ctx, db.RevokeTelegramLinkParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, RevokedBy: revokedBy, ID: linkID})
	if err != nil || n == 0 {
		return false, err
	}
	if err := tx.Q.DeleteRoleGrantsBySubject(ctx, TelegramSubject(linkID)); err != nil {
		return false, err
	}
	svc.TelegramLinkChangedAfterCommit(tx, linkID)
	svc.reloadAfterCommit(ctx, tx)
	return true, nil
}
