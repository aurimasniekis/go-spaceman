package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// RecoveryCodeCount is how many single-use recovery codes are issued (Hallpass HP-AUTH-007).
const RecoveryCodeCount = 10

func totpAAD(opID string) []byte { return keyring.FieldAAD("mfa_totp", "secret_enc", opID) }

func (svc *Service) recoveryMAC(opID, code string) ([]byte, error) {
	norm := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	return svc.kr.MAC(keyring.PurposeToken, []byte("recovery|"+opID+"|"+norm))
}

// recoveryMACs returns the lookup MACs of a recovery code under every non-retired token-key version.
func (svc *Service) recoveryMACs(opID, code string) ([][]byte, error) {
	norm := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	return svc.kr.MACAll(keyring.PurposeToken, []byte("recovery|"+opID+"|"+norm))
}

// TOTPEnrollment is returned when enrollment starts; the secret is shown once for the authenticator app.
type TOTPEnrollment struct {
	Secret string `json:"secret"`
	URI    string `json:"otpauth_uri"`
}

// TOTPPendingTTL bounds an enrollment in progress: the pending secret must be confirmed within it.
const TOTPPendingTTL = 15 * time.Minute

// ErrCompleteMFAFirst refuses enrollment on a session that still has to verify its existing second factor: a password
// alone must never be enough to replace or add a factor (SR-01).
var ErrCompleteMFAFirst = apperr.New(apperr.CodeForbidden, "verify your current second factor before changing it")

func mfaKey(opID string) string { return "mfa:" + opID }

// enrollGate decides whether p may start or confirm TOTP enrollment (Hallpass HP-AUTH-014, SR-01, SR-03):
//   - a Limited session may enroll only when policy requires a first factor (NeedsEnroll);
//   - a Limited session with a confirmed factor must verify it first (VerifyMFA);
//   - a full session replacing a confirmed factor needs recent authentication with MFA;
//   - a full session adding a first factor needs recent authentication.
func (svc *Service) enrollGate(p *Principal) error {
	switch {
	case p.Limited && p.NeedsEnroll:
		return nil
	case p.Limited:
		return ErrCompleteMFAFirst
	case p.User.MFAEnrolled:
		return svc.RequireRecentAuth(p, true)
	default:
		return svc.RequireRecentAuth(p, false)
	}
}

// BeginTOTP starts (or restarts) TOTP enrollment. The new secret is kept as a pending enrollment (TOTPPendingTTL);
// a confirmed factor stays in force until ConfirmTOTP replaces it, so an abandoned re-enrollment never removes it
// (SR-04).
func (svc *Service) BeginTOTP(ctx context.Context, p *Principal) (*TOTPEnrollment, error) {
	if err := svc.enrollGate(p); err != nil {
		return nil, err
	}
	secret, err := NewTOTPSecret()
	if err != nil {
		return nil, err
	}
	// Sealed exactly like mfa_totp.secret_enc, so confirmation moves the ciphertext unchanged.
	sealed, err := svc.kr.Seal(keyring.PurposeField, totpAAD(p.User.ID), secret)
	if err != nil {
		return nil, err
	}
	err = svc.s.W().TxNamed(ctx, "auth.totp_begin", func(tx *store.Tx) error {
		if err := tx.Q.UpsertTOTPPending(ctx, db.UpsertTOTPPendingParams{UserID: p.User.ID, SecretEnc: sealed, CreatedAt: tx.NowMicros(),
			ExpiresAt: tx.Now.Add(TOTPPendingTTL).UnixMicro()}); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorUser, ID: p.User.ID}, Action: "user.mfa.enrollment_started",
			Target: audit.Target{Type: "user", ID: p.User.ID}, Details: map[string]any{"replaces_factor": p.User.MFAEnrolled}})
		return err
	})
	if err != nil {
		return nil, err
	}
	account := p.User.Email
	if account == "" {
		account = p.User.DisplayName
	}
	return &TOTPEnrollment{Secret: EncodeTOTPSecret(secret), URI: TOTPURI(secret, svc.opts.Issuer, account)}, nil
}

var errStartEnrollment = apperr.New(apperr.CodePrecondition, "start TOTP enrollment first")

// ConfirmTOTP completes a pending enrollment with a valid code for the pending secret: the secret becomes the
// user's factor, fresh recovery codes are issued (returned once) and the user's other sessions are revoked
// (MFA change, HP-AUTH-010). Failures share the MFA throttle and are audited (Hallpass HP-AUTH-004).
//
// Enrollment never refreshes the authentication time (SR-03). A Limited session that had to enroll stays Limited
// and completes sign-in with VerifyMFA and a later code. A full session is rotated with mfa_at = now (SR-27); the
// returned token replaces it ("" when the session is unchanged).
func (svc *Service) ConfirmTOTP(ctx context.Context, p *Principal, code string, meta SessionMeta) ([]string, string, error) {
	if err := svc.enrollGate(p); err != nil {
		return nil, "", err
	}
	opID := p.User.ID
	defer svc.attempts.lock(mfaKey(opID))()
	if t, err := svc.throttled(ctx, mfaKey(opID)); err != nil {
		return nil, "", err
	} else if t {
		return nil, "", ErrThrottled
	}
	pend, err := svc.s.R().GetTOTPPending(ctx, opID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", errStartEnrollment
	} else if err != nil {
		return nil, "", err
	}
	now := svc.opts.Clock.Now()
	if !now.Before(time.UnixMicro(pend.ExpiresAt)) {
		return nil, "", errStartEnrollment
	}
	secret, err := svc.kr.Open(keyring.PurposeField, totpAAD(opID), pend.SecretEnc)
	if err != nil {
		return nil, "", err
	}
	step, ok := VerifyTOTP(secret, code, now, 0)
	if !ok {
		if err := svc.recordMFAFailure(ctx, opID, meta.IP, "enroll"); err != nil {
			return nil, "", err
		}
		return nil, "", apperr.New(apperr.CodeInvalid, "invalid code; check your authenticator's clock")
	}
	codes, err := svc.newRecoveryCodes()
	if err != nil {
		return nil, "", err
	}
	var token string
	err = svc.s.W().TxNamed(ctx, "auth.totp_confirm", func(tx *store.Tx) error {
		// Single use: the pending row must still be the one the code was checked against.
		n, err := tx.Q.TakeTOTPPending(ctx, db.TakeTOTPPendingParams{UserID: opID, SecretEnc: pend.SecretEnc, Now: tx.NowMicros()})
		if err != nil {
			return err
		}
		if n != 1 {
			return errStartEnrollment
		}
		if err := tx.Q.SetConfirmedTOTP(ctx, db.SetConfirmedTOTPParams{UserID: opID, SecretEnc: pend.SecretEnc,
			Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, LastStep: step}); err != nil {
			return err
		}
		if err := svc.storeRecoveryCodes(ctx, tx, opID, codes); err != nil {
			return err
		}
		if err := tx.Q.DeleteThrottle(ctx, mfaKey(opID)); err != nil {
			return err
		}
		if err := svc.revokeUserSessionsTx(ctx, tx, opID, "mfa_changed", p.tokenHash); err != nil {
			return err
		}
		if !p.Limited {
			mfaAt := tx.Now
			if token, err = svc.rotateTx(ctx, tx, p, p.AuthAt, &mfaAt, meta); err != nil {
				return err
			}
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorUser, ID: opID}, Action: "user.mfa.enrolled",
			Target: audit.Target{Type: "user", ID: opID}, SourceIP: meta.IP, Details: map[string]any{"replaced_factor": p.User.MFAEnrolled}})
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return codes, token, nil
}

// recordMFAFailure counts a wrong second-factor code against the user's shared MFA throttle key and audits it
// (Hallpass HP-AUTH-004, SR-05). purpose names the flow: login, reauth, stepup or enroll.
func (svc *Service) recordMFAFailure(ctx context.Context, opID, ip, purpose string) error {
	return svc.s.W().TxNamed(ctx, "auth.mfa_failed", func(tx *store.Tx) error {
		if err := svc.recordFailure(ctx, tx, mfaKey(opID)); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorUser, ID: opID}, Action: "user.mfa.failed",
			Outcome: audit.OutcomeFailure, SourceIP: ip, Details: map[string]any{"purpose": purpose}})
		return err
	})
}

// verifySecondFactor is the one entry point for second-factor checks (VerifyMFA, Reauth, ReauthTOTP): it honours
// the shared MFA throttle, verifies a TOTP code (no step reuse) or consumes a recovery code, and records and
// audits failures (Hallpass HP-AUTH-004, SR-05). ok is false for a wrong code.
func (svc *Service) verifySecondFactor(ctx context.Context, opID, code, ip, purpose string) (ok bool, method string, err error) {
	defer svc.attempts.lock(mfaKey(opID))()
	if t, err := svc.throttled(ctx, mfaKey(opID)); err != nil {
		return false, "", err
	} else if t {
		return false, "", ErrThrottled
	}
	ok, method, err = svc.checkSecondFactor(ctx, opID, strings.TrimSpace(code))
	if err != nil {
		return false, "", err
	}
	if !ok {
		if err := svc.recordMFAFailure(ctx, opID, ip, purpose); err != nil {
			return false, "", err
		}
	}
	return ok, method, nil
}

// DisableTOTP removes the second factor. It requires recent authentication with MFA and is refused when policy
// requires MFA for the user.
func (svc *Service) DisableTOTP(ctx context.Context, p *Principal) error {
	if err := svc.RequireRecentAuth(p, true); err != nil {
		return err
	}
	if p.User.MFARequired {
		return apperr.New(apperr.CodeForbidden, "MFA is mandatory for this account")
	}
	return svc.s.W().TxNamed(ctx, "auth.totp_disable", func(tx *store.Tx) error {
		if _, err := tx.Q.DeleteTOTP(ctx, p.User.ID); err != nil {
			return err
		}
		if err := tx.Q.DeleteTOTPPending(ctx, p.User.ID); err != nil {
			return err
		}
		if err := tx.Q.DeleteRecoveryCodes(ctx, p.User.ID); err != nil {
			return err
		}
		if err := svc.revokeUserSessionsTx(ctx, tx, p.User.ID, "mfa_changed", p.tokenHash); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorUser, ID: p.User.ID}, Action: "user.mfa.disabled",
			Target: audit.Target{Type: "user", ID: p.User.ID}})
		return err
	})
}

// RegenerateRecoveryCodes replaces all recovery codes (recent authentication required).
func (svc *Service) RegenerateRecoveryCodes(ctx context.Context, p *Principal) ([]string, error) {
	if err := svc.RequireRecentAuth(p, true); err != nil {
		return nil, err
	}
	if !p.User.MFAEnrolled {
		return nil, apperr.New(apperr.CodePrecondition, "enroll TOTP first")
	}
	codes, err := svc.newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	err = svc.s.W().TxNamed(ctx, "auth.recovery_regen", func(tx *store.Tx) error {
		if err := svc.storeRecoveryCodes(ctx, tx, p.User.ID, codes); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorUser, ID: p.User.ID}, Action: "user.recovery_codes.regenerated"})
		return err
	})
	return codes, err
}

const recoveryAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

func (svc *Service) newRecoveryCodes() ([]string, error) {
	codes := make([]string, RecoveryCodeCount)
	for i := range codes {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		var sb strings.Builder
		for j, c := range b {
			if j == 6 {
				sb.WriteByte('-')
			}
			sb.WriteByte(recoveryAlphabet[int(c)%len(recoveryAlphabet)])
		}
		codes[i] = sb.String()
	}
	return codes, nil
}

func (svc *Service) storeRecoveryCodes(ctx context.Context, tx *store.Tx, opID string, codes []string) error {
	if err := tx.Q.DeleteRecoveryCodes(ctx, opID); err != nil {
		return err
	}
	for _, c := range codes {
		mac, err := svc.recoveryMAC(opID, c)
		if err != nil {
			return err
		}
		if err := tx.Q.InsertRecoveryCode(ctx, db.InsertRecoveryCodeParams{UserID: opID, CodeHash: mac, CreatedAt: tx.NowMicros()}); err != nil {
			return err
		}
	}
	return nil
}
