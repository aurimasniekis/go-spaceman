package oidc

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// linkInviteTTL bounds admin-issued link invitations.
const linkInviteTTL = 72 * time.Hour

func actorRef(a audit.Actor) string {
	if a.ID == "" {
		return a.Type
	}
	return a.Type + ":" + a.ID
}

// IssueLinkInvite creates a single-use invitation that links the next SSO login through providerID to userID (an
// audited admin action; callers require recent authentication, Hallpass HP-OIDC-009). The token is shown once.
func (svc *Service) IssueLinkInvite(ctx context.Context, actor audit.Actor, userID, providerID string) (string, time.Time, error) {
	token, err := random32()
	if err != nil {
		return "", time.Time{}, err
	}
	exp := svc.opts.Clock.Now().Add(linkInviteTTL)
	data, _ := json.Marshal(map[string]string{"provider_id": providerID})
	err = svc.s.W().TxNamed(ctx, "oidc.link_invite", func(tx *store.Tx) error {
		if _, err := tx.Q.GetUser(ctx, userID); err != nil {
			return apperr.New(apperr.CodeNotFound, "user not found")
		}
		if _, err := tx.Q.GetOIDCProvider(ctx, providerID); err != nil {
			return apperr.New(apperr.CodeNotFound, "provider not found")
		}
		if err := tx.Q.InsertOneTimeToken(ctx, db.InsertOneTimeTokenParams{Hash: hashOf("link", token), Kind: "link_code",
			UserID: sql.NullString{String: userID, Valid: true}, DataJson: string(data), ExpiresAt: exp.UnixMicro(), CreatedAt: tx.NowMicros(),
			CreatedBy: actorRef(actor)}); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "user.identity.link_invited", Target: audit.Target{Type: "user", ID: userID},
			Details: map[string]any{"provider_id": providerID, "expires_at": exp}})
		return err
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return token, exp, nil
}

// checkLinkToken consumes a link invitation (single use) and returns its user.
func (svc *Service) checkLinkToken(ctx context.Context, token, providerID string) (string, error) {
	var uid string
	bad := apperr.New(apperr.CodeUnauthorized, "link invitation is invalid or expired")
	err := svc.s.W().TxNamed(ctx, "oidc.use_link_invite", func(tx *store.Tx) error {
		row, err := tx.Q.GetOneTimeToken(ctx, hashOf("link", token))
		if err != nil || row.Kind != "link_code" || !row.UserID.Valid {
			return bad
		}
		var data struct {
			ProviderID string `json:"provider_id"`
		}
		if json.Unmarshal([]byte(row.DataJson), &data) != nil || data.ProviderID != providerID {
			return bad
		}
		n, err := tx.Q.UseOneTimeToken(ctx, db.UseOneTimeTokenParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, Hash: row.Hash})
		if err != nil {
			return err
		}
		if n == 0 {
			return bad
		}
		uid = row.UserID.String
		return nil
	})
	return uid, err
}

// LinkIdentity links an expected identity (issuer + subject) to a user directly: an audited admin action that
// requires recent authentication at the API layer (Hallpass HP-OIDC-009). With Porch the subject is the stable
// `u_…` user ID, never an email.
func (svc *Service) LinkIdentity(ctx context.Context, actor audit.Actor, userID, providerID, subject string) error {
	if subject == "" || len(subject) > 255 {
		return invalid("subject is required (at most 255 bytes)")
	}
	p, err := svc.GetProvider(ctx, providerID)
	if err != nil {
		return err
	}
	return svc.s.W().TxNamed(ctx, "oidc.link_admin", func(tx *store.Tx) error {
		if _, err := tx.Q.GetUser(ctx, userID); err != nil {
			return apperr.New(apperr.CodeNotFound, "user not found")
		}
		if ident, err := tx.Q.GetUserIdentity(ctx, db.GetUserIdentityParams{Issuer: p.Issuer, Subject: subject}); err == nil {
			if ident.UserID == userID {
				return nil
			}
			return apperr.New(apperr.CodeConflict, "this identity is already linked to another user")
		}
		if err := tx.Q.InsertUserIdentity(ctx, db.InsertUserIdentityParams{Issuer: p.Issuer, Subject: subject, ProviderID: p.ID, UserID: userID,
			LinkedVia: "admin", CreatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		svc.auth.PublishUserChangedTx(tx, userID)
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "user.identity.linked", Target: audit.Target{Type: "user", ID: userID},
			Details: map[string]any{"provider": p.Slug, "via": "admin"}})
		return err
	})
}

// ErrLastSignInMethod refuses a self-service unlink that would leave the account without any way to sign in.
var ErrLastSignInMethod = apperr.New(apperr.CodePrecondition, "this is your only way to sign in; set a password or link another identity first")

// UnlinkIdentity removes a linked identity, drops the user's SSO-sourced grants (the mappings of another linked
// provider restore them at the next SSO sign-in) and revokes the user's sessions (Hallpass). self marks a user
// unlinking their own identity, which is refused when it is their last way to sign in.
func (svc *Service) UnlinkIdentity(ctx context.Context, actor audit.Actor, userID, issuer, subject string, self bool) error {
	err := svc.s.W().TxNamed(ctx, "oidc.unlink", func(tx *store.Tx) error {
		if self {
			n, err := tx.Q.CountUserIdentities(ctx, userID)
			if err != nil {
				return err
			}
			if _, err := tx.Q.GetLocalCredential(ctx, userID); errors.Is(err, sql.ErrNoRows) && n <= 1 {
				return ErrLastSignInMethod
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		n, err := tx.Q.DeleteUserIdentity(ctx, db.DeleteUserIdentityParams{Issuer: issuer, Subject: subject, UserID: userID})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeNotFound, "identity not found")
		}
		if err := svc.auth.DropSSOGrantsTx(ctx, tx, userID); err != nil {
			return err
		}
		svc.auth.PublishUserChangedTx(tx, userID)
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "user.identity.unlinked", Target: audit.Target{Type: "user", ID: userID},
			Details: map[string]any{"issuer": issuer, "self": self}})
		return err
	})
	if err != nil {
		return err
	}
	return svc.auth.RevokeAllSessions(ctx, userID, "identity_unlinked")
}

// Identities lists a user's linked identities.
func (svc *Service) Identities(ctx context.Context, userID string) ([]Identity, error) {
	rows, err := svc.s.R().ListUserIdentities(ctx, userID)
	if err != nil {
		return nil, err
	}
	names := map[string]db.OidcProvider{}
	out := make([]Identity, len(rows))
	for i, r := range rows {
		pr, ok := names[r.ProviderID]
		if !ok {
			if pr, err = svc.s.R().GetOIDCProvider(ctx, r.ProviderID); err != nil {
				return nil, err
			}
			names[r.ProviderID] = pr
		}
		out[i] = Identity{Issuer: r.Issuer, Subject: r.Subject, ProviderID: r.ProviderID, ProviderSlug: pr.Slug, ProviderName: pr.DisplayName,
			UserID: r.UserID, LinkedVia: r.LinkedVia, CreatedAt: time.UnixMicro(r.CreatedAt).UTC()}
		if r.LastLoginAt.Valid {
			t := time.UnixMicro(r.LastLoginAt.Int64).UTC()
			out[i].LastLoginAt = &t
		}
	}
	return out, nil
}

// PurgeStates removes expired pending authorizations (background job).
func (svc *Service) PurgeStates(ctx context.Context) (int64, error) {
	var n int64
	err := svc.s.W().TxNamed(ctx, "oidc.purge_states", func(tx *store.Tx) error {
		var err error
		n, err = tx.Q.PurgeAuthStates(ctx, tx.NowMicros())
		return err
	})
	return n, err
}
