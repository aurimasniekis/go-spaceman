package auth

import (
	"context"
	"database/sql"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// CreateSessionTx starts a session for a user authenticated by an external identity provider, inside the
// caller's transaction (the OIDC login commits identity, role sync and session atomically).
func (svc *Service) CreateSessionTx(ctx context.Context, tx *store.Tx, userID, method string, meta SessionMeta) (string, error) {
	return svc.createSession(ctx, tx, userID, method, nil, meta)
}

// ReauthExternal records a completed upstream step-up (Hallpass HP-OIDC-013): the session is rotated with a fresh
// authentication time.
func (svc *Service) ReauthExternal(ctx context.Context, p *Principal, provider string, meta SessionMeta) (string, error) {
	var token string
	err := svc.s.W().TxNamed(ctx, "auth.reauth_external", func(tx *store.Tx) error {
		var err error
		token, err = svc.rotateWithAuthTx(ctx, tx, p, nil, meta)
		if err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorUser, ID: p.User.ID}, Action: "user.reauthenticated",
			SourceIP: meta.IP, Details: map[string]any{"method": p.Method, "provider": provider}})
		return err
	})
	return token, err
}

// ErrTOTPStepUpNotAllowed refuses the TOTP step-up fallback outside SSO sessions: a password session
// re-authenticates with its password (SR-03).
var ErrTOTPStepUpNotAllowed = apperr.New(apperr.CodePrecondition, "TOTP step-up is only available for SSO sessions whose provider allows it")

// ReauthTOTP is the step-up fallback for SSO sessions whose provider cannot prove a fresh login (no auth_time):
// the user's local TOTP is required (stepup.fallback = local_totp, Hallpass HP-OIDC-013). Callers must check that the
// session's provider allows the fallback (oidc.Service.ReauthTOTP does). Wrong codes share the MFA throttle.
func (svc *Service) ReauthTOTP(ctx context.Context, p *Principal, code string, meta SessionMeta) (string, error) {
	if p.Method != MethodOIDC || p.Limited {
		return "", ErrTOTPStepUpNotAllowed
	}
	if !p.User.MFAEnrolled {
		return "", apperr.New(apperr.CodePrecondition, "enroll TOTP in SpaceMan to re-authenticate")
	}
	ok, _, err := svc.verifySecondFactor(ctx, p.User.ID, code, meta.IP, "stepup")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrMFARequired
	}
	now := svc.opts.Clock.Now()
	var token string
	err = svc.s.W().TxNamed(ctx, "auth.reauth_totp", func(tx *store.Tx) error {
		if err := tx.Q.DeleteThrottle(ctx, mfaKey(p.User.ID)); err != nil {
			return err
		}
		var err error
		if token, err = svc.rotateWithAuthTx(ctx, tx, p, &now, meta); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorUser, ID: p.User.ID}, Action: "user.reauthenticated",
			SourceIP: meta.IP, Details: map[string]any{"method": p.Method, "factor": "totp"}})
		return err
	})
	return token, err
}

// rotateWithAuthTx revokes the current session and issues a new one with auth_at = now (same method).
func (svc *Service) rotateWithAuthTx(ctx context.Context, tx *store.Tx, p *Principal, mfaAt *time.Time, meta SessionMeta) (string, error) {
	n, err := tx.Q.RevokeWebSession(ctx, db.RevokeWebSessionParams{RevokedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
		RevokeReason: sql.NullString{String: "rotated", Valid: true}, IDHash: p.tokenHash})
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", ErrUnauthorized
	}
	meta.Provider = p.Provider
	return svc.createSession(ctx, tx, p.User.ID, p.Method, mfaAt, meta)
}
