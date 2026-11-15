package auth

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Token lifetimes.
const (
	SetupTokenTTL    = 30 * time.Minute
	InviteTTL        = 72 * time.Hour
	ResetTTL         = time.Hour
	RecoveryLoginTTL = 30 * time.Minute
)

// ErrTokenInvalid is returned for unknown, expired or used tokens (one message for all, enumeration-resistant).
var ErrTokenInvalid = apperr.New(apperr.CodeUnauthorized, "this link or token is invalid or has expired")

func (svc *Service) issueToken(ctx context.Context, tx *store.Tx, kind, userID string, ttl time.Duration, data map[string]any, actor audit.Actor) (string, time.Time, error) {
	token, err := randomToken()
	if err != nil {
		return "", time.Time{}, err
	}
	mac, err := svc.tokenMAC(token)
	if err != nil {
		return "", time.Time{}, err
	}
	exp := tx.Now.Add(ttl)
	if data == nil {
		data = map[string]any{}
	}
	if err := tx.Q.InsertOneTimeToken(ctx, db.InsertOneTimeTokenParams{Hash: mac, Kind: kind, UserID: sql.NullString{String: userID, Valid: userID != ""},
		DataJson: mustJSON(data), ExpiresAt: exp.UnixMicro(), CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor)}); err != nil {
		return "", time.Time{}, err
	}
	return token, exp, nil
}

// consumeToken validates and marks a token used inside tx, returning its row.
func (svc *Service) consumeToken(ctx context.Context, tx *store.Tx, kind, token string) (db.OneTimeToken, error) {
	macs, err := svc.tokenMACs(token)
	if err != nil {
		return db.OneTimeToken{}, err
	}
	for _, mac := range macs {
		row, err := tx.Q.GetOneTimeToken(ctx, mac)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return db.OneTimeToken{}, err
		}
		if row.Kind != kind {
			return db.OneTimeToken{}, ErrTokenInvalid
		}
		n, err := tx.Q.UseOneTimeToken(ctx, db.UseOneTimeTokenParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, Hash: mac})
		if err != nil {
			return db.OneTimeToken{}, err
		}
		if n != 1 {
			return db.OneTimeToken{}, ErrTokenInvalid
		}
		return row, nil
	}
	return db.OneTimeToken{}, ErrTokenInvalid
}

func (svc *Service) insertLocalUser(ctx context.Context, tx *store.Tx, email, name, phc, via string, grants []GrantInput, mfa bool, actor audit.Actor) (string, error) {
	if name == "" {
		name = email
	}
	if len(name) > 128 {
		return "", apperr.New(apperr.CodeInvalid, "display name too long")
	}
	if _, err := tx.Q.GetUserByEmail(ctx, sql.NullString{String: email, Valid: true}); err == nil {
		return "", apperr.New(apperr.CodeAlreadyExists, "a user with this email already exists")
	}
	id := newUserID()
	if err := tx.Q.InsertUser(ctx, db.InsertUserParams{ID: id, Email: sql.NullString{String: email, Valid: true}, DisplayName: name, Enabled: 1,
		ProvisionedVia: via, MfaRequired: b2i(mfa), CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor), UpdatedAt: tx.NowMicros()}); err != nil {
		return "", err
	}
	if err := svc.insertGrants(ctx, tx, UserSubject(id), grants, "manual", actor); err != nil {
		return "", err
	}
	if err := tx.Q.UpsertLocalCredential(ctx, db.UpsertLocalCredentialParams{UserID: id, PasswordPhc: phc,
		ParamsVersion: int64(CurrentParams.Version), ChangedAt: tx.NowMicros()}); err != nil {
		return "", err
	}
	_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "user.created", Target: audit.Target{Type: "user", ID: id},
		Details: map[string]any{"email": email, "grants": grants, "via": via, "has_password": true}})
	return id, err
}

// Invite creates a single-use invitation with preset grants (Hallpass HP-AUTH-011).
func (svc *Service) Invite(ctx context.Context, actor audit.Actor, email string, in []GrantInput) (string, time.Time, error) {
	e, err := NormalizeEmail(email)
	if err != nil {
		return "", time.Time{}, err
	}
	grants, err := validGrants(in)
	if err != nil {
		return "", time.Time{}, err
	}
	var token string
	var exp time.Time
	err = svc.s.W().TxNamed(ctx, "auth.invite", func(tx *store.Tx) error {
		if _, err := tx.Q.GetUserByEmail(ctx, sql.NullString{String: e, Valid: true}); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "a user with this email already exists")
		}
		var err error
		token, exp, err = svc.issueToken(ctx, tx, "invite", "", InviteTTL, map[string]any{"email": e, "grants": grants}, actor)
		if err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "user.invited", Details: map[string]any{"email": e, "grants": grants}})
		return err
	})
	return token, exp, err
}

// AcceptInvitation creates the invited user with a password.
func (svc *Service) AcceptInvitation(ctx context.Context, token, displayName, password string) (*User, error) {
	phc, err := svc.hash(ctx, password)
	if err != nil {
		return nil, err
	}
	var id string
	err = svc.s.W().TxNamed(ctx, "auth.accept_invite", func(tx *store.Tx) error {
		row, err := svc.consumeToken(ctx, tx, "invite", token)
		if err != nil {
			return err
		}
		var data struct {
			Email  string       `json:"email"`
			Grants []GrantInput `json:"grants"`
		}
		if err := json.Unmarshal([]byte(row.DataJson), &data); err != nil {
			return err
		}
		grants, err := validGrants(data.Grants)
		if err != nil {
			return err
		}
		id, err = svc.insertLocalUser(ctx, tx, data.Email, displayName, phc, "invite", grants, false, audit.Actor{Type: audit.ActorUser})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, id)
}

// IssueResetToken creates a single-use password reset token for an user (administrator action, HP-AUTH-009).
// Without configured email delivery the administrator hands it over out of band.
func (svc *Service) IssueResetToken(ctx context.Context, actor audit.Actor, userID string) (string, time.Time, error) {
	var token string
	var exp time.Time
	err := svc.s.W().TxNamed(ctx, "auth.reset_token", func(tx *store.Tx) error {
		if _, err := tx.Q.GetUser(ctx, userID); errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "user not found")
		} else if err != nil {
			return err
		}
		if _, err := tx.Q.RevokeOneTimeTokens(ctx, db.RevokeOneTimeTokensParams{UsedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, Kind: "reset",
			UserID: sql.NullString{String: userID, Valid: true}}); err != nil {
			return err
		}
		var err error
		token, exp, err = svc.issueToken(ctx, tx, "reset", userID, ResetTTL, nil, actor)
		if err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "user.reset_token.issued", Target: audit.Target{Type: "user", ID: userID}})
		return err
	})
	return token, exp, err
}

// ResetPassword sets a new password with a reset token and revokes every session of the user (Hallpass HP-AUTH-010).
func (svc *Service) ResetPassword(ctx context.Context, token, password string) error {
	phc, err := svc.hash(ctx, password)
	if err != nil {
		return err
	}
	return svc.s.W().TxNamed(ctx, "auth.reset_password", func(tx *store.Tx) error {
		row, err := svc.consumeToken(ctx, tx, "reset", token)
		if err != nil {
			return err
		}
		opID := row.UserID.String
		if err := tx.Q.UpsertLocalCredential(ctx, db.UpsertLocalCredentialParams{UserID: opID, PasswordPhc: phc,
			ParamsVersion: int64(CurrentParams.Version), ChangedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := svc.revokeUserSessionsTx(ctx, tx, opID, "password_reset", nil); err != nil {
			return err
		}
		if r, err := tx.Q.GetUser(ctx, opID); err == nil {
			for _, k := range []string{throttleKeyAccount(r.Email.String), "acct-op:" + opID, mfaKey(opID)} {
				if err := tx.Q.DeleteThrottle(ctx, k); err != nil {
					return err
				}
			}
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorUser, ID: opID}, Action: "user.password.reset",
			Target: audit.Target{Type: "user", ID: opID}})
		return err
	})
}

// IssueRecoveryLogin is the break-glass path (Hallpass HP-AUTH-016): it must only be reachable from the local CLI, which
// requires the data-directory lock and the KEK. With newAdminEmail set it creates a new administrator instead.
func (svc *Service) IssueRecoveryLogin(ctx context.Context, userID, newAdminEmail string) (string, time.Time, error) {
	var token string
	var exp time.Time
	actor := audit.Actor{Type: audit.ActorCLI, ID: "breakglass"}
	// The new administrator's random password is hashed before the transaction: Argon2id takes far longer than the
	// write lock may be held (HP-DB-004).
	var email, phc string
	if newAdminEmail != "" {
		var err error
		if email, err = NormalizeEmail(newAdminEmail); err != nil {
			return "", time.Time{}, err
		}
		pw, err := randomToken()
		if err != nil {
			return "", time.Time{}, err
		}
		if phc, err = HashPassword([]byte(pw), CurrentParams); err != nil {
			return "", time.Time{}, err
		}
	}
	err := svc.s.W().TxNamed(ctx, "auth.breakglass", func(tx *store.Tx) error {
		target := userID
		if newAdminEmail != "" {
			var err error
			if target, err = svc.insertLocalUser(ctx, tx, email, email, phc, "recovery", []GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}, true, actor); err != nil {
				return err
			}
		} else {
			row, err := tx.Q.GetUser(ctx, userID)
			if errors.Is(err, sql.ErrNoRows) {
				return apperr.New(apperr.CodeNotFound, "user not found")
			} else if err != nil {
				return err
			}
			if row.Enabled != 1 {
				if _, err := tx.Q.SetUserEnabled(ctx, db.SetUserEnabledParams{Enabled: 1, Now: tx.NowMicros(), ID: userID}); err != nil {
					return err
				}
			}
		}
		var err error
		token, exp, err = svc.issueToken(ctx, tx, "recovery_login", target, RecoveryLoginTTL, nil, actor)
		if err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "breakglass.used", Outcome: audit.OutcomeSuccess,
			Target: audit.Target{Type: "user", ID: target}, Details: map[string]any{"new_admin": newAdminEmail != ""}})
		return err
	})
	return token, exp, err
}

// Invitation is a pending (unused, unexpired) invitation. ID is the hex HMAC of the token, never the token itself.
type Invitation struct {
	ID        string       `json:"id"`
	Email     string       `json:"email"`
	Grants    []GrantInput `json:"grants"`
	ExpiresAt time.Time    `json:"expires_at"`
	CreatedAt time.Time    `json:"created_at"`
	CreatedBy string       `json:"created_by"`
}

// ListInvitations returns pending invitations, newest first (Hallpass HP-AUTH-011).
func (svc *Service) ListInvitations(ctx context.Context) ([]Invitation, error) {
	rows, err := svc.s.R().ListPendingInvitations(ctx, svc.opts.Clock.Now().UnixMicro())
	if err != nil {
		return nil, err
	}
	out := make([]Invitation, 0, len(rows))
	for _, r := range rows {
		var data struct {
			Email  string       `json:"email"`
			Grants []GrantInput `json:"grants"`
		}
		_ = json.Unmarshal([]byte(r.DataJson), &data)
		out = append(out, Invitation{ID: hex.EncodeToString(r.Hash), Email: data.Email, Grants: data.Grants,
			ExpiresAt: time.UnixMicro(r.ExpiresAt).UTC(), CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), CreatedBy: r.CreatedBy})
	}
	return out, nil
}

// RevokeInvitation invalidates a pending invitation so it can no longer be accepted (Hallpass HP-AUTH-011, audited).
func (svc *Service) RevokeInvitation(ctx context.Context, actor audit.Actor, id string) error {
	hash, err := hex.DecodeString(id)
	if err != nil || len(hash) == 0 {
		return apperr.New(apperr.CodeNotFound, "invitation not found")
	}
	return svc.s.W().TxNamed(ctx, "auth.revoke_invitation", func(tx *store.Tx) error {
		row, err := tx.Q.GetInvitation(ctx, hash)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "invitation not found")
		}
		if err != nil {
			return err
		}
		n, err := tx.Q.RevokeInvitation(ctx, db.RevokeInvitationParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, Hash: hash})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeNotFound, "invitation is no longer pending")
		}
		var data struct {
			Email string `json:"email"`
		}
		_ = json.Unmarshal([]byte(row.DataJson), &data)
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "user.invitation.revoked", Details: map[string]any{"email": data.Email}})
		return err
	})
}
