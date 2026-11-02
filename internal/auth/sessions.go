package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Authentication methods recorded on sessions.
const (
	MethodPassword   = "password"
	MethodOIDC       = "oidc"
	MethodBreakGlass = "breakglass"
	// MethodToken marks a request authenticated with an automation token (Authorization: Bearer).
	MethodToken = "token"
	// MethodRoot marks a request authenticated with the bootstrap root token.
	MethodRoot = "root"
)

// Principal is the authenticated user behind a request.
type Principal struct {
	// User is the acting user; nil for the root token.
	User      *User
	SessionID string // display ID (safe to show)
	Method    string
	// Provider is the OIDC provider slug for SSO sessions ("" otherwise).
	Provider  string
	AuthAt    time.Time
	MFAAt     *time.Time
	CSRFToken string
	ExpiresAt time.Time
	// Limited is set while MFA verification or enrollment is still required; only MFA endpoints may be used.
	Limited     bool
	NeedsEnroll bool
	// TokenID is set when the request used an automation token instead of a browser session.
	TokenID string
	// TokenParentID is set when that token was itself issued by an API token (ADR-0058).
	TokenParentID string
	// RootTokenID is set when the request used the bootstrap root token (SM-AUTH-005).
	RootTokenID string
	tokenHash   []byte
	csrfSecret  []byte
}

// MFASatisfied reports whether this session completed MFA (or does not need it).
func (p *Principal) MFASatisfied() bool { return !p.Limited }

// Subjects returns the authorization subjects that must all permit a request: the user for sessions; the token and
// its user for API tokens (a token can never exceed its creator); the root token alone for root.
func (p *Principal) Subjects() []string {
	switch {
	case p == nil:
		return nil
	case p.RootTokenID != "":
		return []string{RootSubject(p.RootTokenID)}
	case p.User == nil:
		return nil
	case p.TokenID != "":
		return []string{TokenSubject(p.TokenID), UserSubject(p.User.ID)}
	default:
		return []string{UserSubject(p.User.ID)}
	}
}

// IsRoot reports whether the request used the root token.
func (p *Principal) IsRoot() bool { return p != nil && p.RootTokenID != "" }

// ActorID identifies the principal in audit records.
func (p *Principal) ActorID() string {
	switch {
	case p == nil:
		return ""
	case p.RootTokenID != "":
		return p.RootTokenID
	case p.User != nil:
		return p.User.ID
	}
	return ""
}

func hashSessionToken(token string) []byte {
	h := sha256.Sum256([]byte("spaceman/session/v1|" + token))
	return h[:]
}

// SessionMeta describes the client.
type SessionMeta struct {
	IP        string
	UserAgent string
	// Provider is the OIDC provider slug that authenticated the session (SSO only).
	Provider string
}

// createSession inserts a new session inside tx and returns the bearer token (cookie value).
func (svc *Service) createSession(ctx context.Context, tx *store.Tx, userID, method string, mfaAt *time.Time, meta SessionMeta) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	var mfa sql.NullInt64
	if mfaAt != nil {
		mfa = sql.NullInt64{Int64: mfaAt.UnixMicro(), Valid: true}
	}
	if len(meta.UserAgent) > 256 {
		meta.UserAgent = meta.UserAgent[:256]
	}
	err = tx.Q.InsertWebSession(ctx, db.InsertWebSessionParams{
		IDHash: hashSessionToken(token), UserID: userID, CsrfSecret: secret, AuthMethod: method, MfaAt: mfa,
		AuthAt: tx.NowMicros(), CreatedAt: tx.NowMicros(), LastSeenAt: tx.NowMicros(),
		AbsoluteExpiresAt: tx.Now.Add(svc.opts.SessionAbsolute).UnixMicro(), Ip: meta.IP, UserAgent: meta.UserAgent, DisplayID: ids.New(ids.WebSession),
		AuthProvider: meta.Provider,
	})
	if err != nil {
		return "", err
	}
	if err := tx.Q.TouchUserLogin(ctx, db.TouchUserLoginParams{LastLoginAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: userID}); err != nil {
		return "", err
	}
	return token, nil
}

// ErrUnauthorized means no valid session.
var ErrUnauthorized = apperr.New(apperr.CodeUnauthorized, "authentication required")

// Authenticate resolves a session token to a principal, enforcing revocation, idle and absolute expiry (Hallpass HP-AUTH-012).
// It marks the session active (idle timer), so call it only for user-initiated requests.
func (svc *Service) Authenticate(ctx context.Context, token string) (*Principal, error) {
	return svc.authenticate(ctx, token, true)
}

// Validate is Authenticate without marking the session active: background re-checks of long-lived requests (the SSE
// stream) use it so they never defeat the idle timeout (SR-09).
func (svc *Service) Validate(ctx context.Context, token string) (*Principal, error) {
	return svc.authenticate(ctx, token, false)
}

func (svc *Service) authenticate(ctx context.Context, token string, touch bool) (*Principal, error) {
	if token == "" || len(token) > 128 {
		return nil, ErrUnauthorized
	}
	h := hashSessionToken(token)
	row, err := svc.s.R().GetWebSession(ctx, h)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	now := svc.opts.Clock.Now()
	if row.RevokedAt.Valid || !now.Before(time.UnixMicro(row.AbsoluteExpiresAt)) || now.Sub(time.UnixMicro(row.LastSeenAt)) > svc.opts.SessionIdle {
		return nil, ErrUnauthorized
	}
	op, err := svc.Get(ctx, row.UserID)
	if err != nil || !op.Enabled {
		return nil, ErrUnauthorized
	}
	p := &Principal{User: op, SessionID: row.DisplayID, Method: row.AuthMethod, Provider: row.AuthProvider, AuthAt: time.UnixMicro(row.AuthAt).UTC(),
		ExpiresAt: time.UnixMicro(row.AbsoluteExpiresAt).UTC(), tokenHash: h, csrfSecret: row.CsrfSecret}
	if row.MfaAt.Valid {
		t := time.UnixMicro(row.MfaAt.Int64).UTC()
		p.MFAAt = &t
	}
	switch row.AuthMethod {
	case MethodPassword:
		p.NeedsEnroll = op.MFARequired && !op.MFAEnrolled
		p.Limited = p.NeedsEnroll || (op.MFAEnrolled && p.MFAAt == nil)
	case MethodBreakGlass:
		// Break-glass removes the lost factor; an account that policy requires MFA for enrolls a new one before the
		// recovery session can be used (SR-07).
		p.NeedsEnroll = op.MFARequired && !op.MFAEnrolled
		p.Limited = p.NeedsEnroll
	}
	p.CSRFToken = csrfToken(row.CsrfSecret, row.DisplayID)
	if touch && now.Sub(time.UnixMicro(row.LastSeenAt)) > time.Minute {
		_ = svc.s.W().TxNamed(ctx, "auth.touch_session", func(tx *store.Tx) error {
			return tx.Q.TouchWebSession(ctx, db.TouchWebSessionParams{LastSeenAt: tx.NowMicros(), IDHash: h})
		})
	}
	return p, nil
}

func csrfToken(secret []byte, displayID string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("spaceman/csrf/v1|" + displayID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// VerifyCSRF checks a request's CSRF token in constant time (Hallpass HP-AUTH-013).
func (p *Principal) VerifyCSRF(token string) bool {
	return token != "" && hmac.Equal([]byte(token), []byte(csrfToken(p.csrfSecret, p.SessionID)))
}

// ErrReauthRequired and ErrMFARequired drive the UI's re-authentication flow (Hallpass HP-AUTH-014).
var (
	ErrReauthRequired = apperr.New(apperr.CodeReauth, "recent authentication required")
	ErrMFARequired    = apperr.New(apperr.CodeMFARequired, "multi-factor authentication required")
)

// RequireRecentAuth checks that the session authenticated within the reauth window, and with MFA when the user
// has MFA (or needMFA is set).
func (svc *Service) RequireRecentAuth(p *Principal, needMFA bool) error {
	now := svc.opts.Clock.Now()
	if now.Sub(p.AuthAt) > svc.opts.ReauthWindow {
		return ErrReauthRequired
	}
	if (needMFA || p.User.MFAEnrolled) && p.Method == MethodPassword {
		if p.MFAAt == nil || now.Sub(*p.MFAAt) > svc.opts.ReauthWindow {
			return ErrMFARequired
		}
	}
	return nil
}

// Rotate replaces the session token (after login completion, MFA, reauth or privilege change) and revokes the old one.
func (svc *Service) Rotate(ctx context.Context, p *Principal, meta SessionMeta) (string, error) {
	var token string
	err := svc.s.W().TxNamed(ctx, "auth.rotate_session", func(tx *store.Tx) error {
		old, err := tx.Q.GetWebSession(ctx, p.tokenHash)
		if err != nil {
			return err
		}
		if _, err := tx.Q.RevokeWebSession(ctx, db.RevokeWebSessionParams{RevokedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			RevokeReason: sql.NullString{String: "rotated", Valid: true}, IDHash: p.tokenHash}); err != nil {
			return err
		}
		var mfa *time.Time
		if old.MfaAt.Valid {
			t := time.UnixMicro(old.MfaAt.Int64)
			mfa = &t
		}
		meta.Provider = old.AuthProvider
		token, err = svc.createSession(ctx, tx, old.UserID, old.AuthMethod, mfa, meta)
		if err != nil {
			return err
		}
		// Preserve the original authentication time and absolute expiry across rotation.
		return tx.Q.UpdateWebSessionAuth(ctx, db.UpdateWebSessionAuthParams{AuthAt: old.AuthAt, MfaAt: old.MfaAt, IDHash: hashSessionToken(token)})
	})
	return token, err
}

// rotateTx revokes p's session and issues a replacement for the same user, method and provider inside tx, with
// the given authentication times (mfaAt nil: no MFA). It fails with ErrUnauthorized when p's session was already
// revoked, so a session can be rotated only once.
func (svc *Service) rotateTx(ctx context.Context, tx *store.Tx, p *Principal, authAt time.Time, mfaAt *time.Time, meta SessionMeta) (string, error) {
	n, err := tx.Q.RevokeWebSession(ctx, db.RevokeWebSessionParams{RevokedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
		RevokeReason: sql.NullString{String: "rotated", Valid: true}, IDHash: p.tokenHash})
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", ErrUnauthorized
	}
	meta.Provider = p.Provider
	token, err := svc.createSession(ctx, tx, p.User.ID, p.Method, mfaAt, meta)
	if err != nil {
		return "", err
	}
	var mfa sql.NullInt64
	if mfaAt != nil {
		mfa = sql.NullInt64{Int64: mfaAt.UnixMicro(), Valid: true}
	}
	return token, tx.Q.UpdateWebSessionAuth(ctx, db.UpdateWebSessionAuthParams{AuthAt: authAt.UnixMicro(), MfaAt: mfa, IDHash: hashSessionToken(token)})
}

// Logout revokes the session (Hallpass HP-OIDC-015: provider logout is separate and not global).
func (svc *Service) Logout(ctx context.Context, token string) error {
	return svc.s.W().TxNamed(ctx, "auth.logout", func(tx *store.Tx) error {
		_, err := tx.Q.RevokeWebSession(ctx, db.RevokeWebSessionParams{RevokedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			RevokeReason: sql.NullString{String: "logout", Valid: true}, IDHash: hashSessionToken(token)})
		return err
	})
}

// SessionInfo describes an active session for management views.
type SessionInfo struct {
	ID        string    `json:"id"`
	Method    string    `json:"auth_method"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen_at"`
	ExpiresAt time.Time `json:"expires_at"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	Current   bool      `json:"current"`
}

// ListSessions returns an user's active sessions.
func (svc *Service) ListSessions(ctx context.Context, userID, currentDisplayID string) ([]SessionInfo, error) {
	rows, err := svc.s.R().ListUserSessions(ctx, db.ListUserSessionsParams{UserID: userID, AbsoluteExpiresAt: svc.opts.Clock.Now().UnixMicro()})
	if err != nil {
		return nil, err
	}
	out := make([]SessionInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, SessionInfo{ID: r.DisplayID, Method: r.AuthMethod, CreatedAt: time.UnixMicro(r.CreatedAt).UTC(),
			LastSeen: time.UnixMicro(r.LastSeenAt).UTC(), ExpiresAt: time.UnixMicro(r.AbsoluteExpiresAt).UTC(), IP: r.Ip, UserAgent: r.UserAgent,
			Current: r.DisplayID == currentDisplayID})
	}
	return out, nil
}

// RevokeSession revokes one of an user's sessions by display ID.
func (svc *Service) RevokeSession(ctx context.Context, userID, displayID, reason string) error {
	return svc.s.W().TxNamed(ctx, "auth.revoke_session", func(tx *store.Tx) error {
		n, err := tx.Q.RevokeWebSessionByDisplayID(ctx, db.RevokeWebSessionByDisplayIDParams{RevokedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			RevokeReason: sql.NullString{String: reason, Valid: true}, DisplayID: displayID, UserID: userID})
		if err == nil && n == 0 {
			return apperr.New(apperr.CodeNotFound, "session not found")
		}
		return err
	})
}

// RevokeAllSessions revokes every session of an user (admin action or security event).
func (svc *Service) RevokeAllSessions(ctx context.Context, userID, reason string) error {
	return svc.s.W().TxNamed(ctx, "auth.revoke_all", func(tx *store.Tx) error {
		_, err := tx.Q.RevokeUserSessions(ctx, db.RevokeUserSessionsParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			Reason: sql.NullString{String: reason, Valid: true}, UserID: userID, ExceptHash: []byte{}})
		return err
	})
}

// PurgeExpired deletes expired sessions and one-time tokens (periodic job).
func (svc *Service) PurgeExpired(ctx context.Context) error {
	return svc.s.W().TxNamed(ctx, "auth.purge", func(tx *store.Tx) error {
		if _, err := tx.Q.PurgeWebSessions(ctx, tx.Now.Add(-24*time.Hour).UnixMicro()); err != nil {
			return err
		}
		if _, err := tx.Q.PurgeTOTPPending(ctx, tx.NowMicros()); err != nil {
			return err
		}
		_, err := tx.Q.PurgeOneTimeTokens(ctx, tx.Now.Add(-7*24*time.Hour).UnixMicro())
		return err
	})
}
