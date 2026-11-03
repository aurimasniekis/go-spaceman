package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Options configure the service.
type Options struct {
	PasswordMinBytes int
	SessionIdle      time.Duration
	SessionAbsolute  time.Duration
	ReauthWindow     time.Duration
	// RequireTOTP forces TOTP enrollment for every local password account (production default, SM-AUTH-001). Accounts
	// with an operator or admin grant must enroll regardless.
	RequireTOTP bool
	Issuer      string
	Clock       clock.Clock
	// HashConcurrency bounds simultaneous Argon2id computations (CPU/memory DoS guard).
	HashConcurrency int
	// Metrics counts login attempts by method and outcome. Nil disables.
	Metrics *metrics.Metrics
}

// Service manages users and their authentication.
type Service struct {
	s     *store.Store
	kr    *keyring.Keyring
	audit *audit.Writer
	bus   *eventbus.Bus
	az    *authz.Engine
	opts  Options
	sem   chan struct{}
	rev   *Revocations
	// attempts serializes credential attempts per throttle key (attempts.go).
	attempts attemptLocks
}

// New returns the user authentication service. az is reloaded after every committed grant change.
func New(s *store.Store, kr *keyring.Keyring, aw *audit.Writer, bus *eventbus.Bus, az *authz.Engine, o Options) *Service {
	if o.PasswordMinBytes <= 0 {
		o.PasswordMinBytes = 12
	}
	if o.SessionIdle <= 0 {
		o.SessionIdle = 30 * time.Minute
	}
	if o.SessionAbsolute <= 0 {
		o.SessionAbsolute = 12 * time.Hour
	}
	if o.ReauthWindow <= 0 {
		o.ReauthWindow = 5 * time.Minute
	}
	if o.Issuer == "" {
		o.Issuer = "SpaceMan"
	}
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.HashConcurrency <= 0 {
		o.HashConcurrency = 4
	}
	return &Service{s: s, kr: kr, audit: aw, bus: bus, az: az, opts: o, sem: make(chan struct{}, o.HashConcurrency), rev: NewRevocations()}
}

// Authz returns the authorization engine.
func (svc *Service) Authz() *authz.Engine { return svc.az }

// User is a user account (no secrets).
type User struct {
	ID             string        `json:"id"`
	Email          string        `json:"email,omitempty"`
	DisplayName    string        `json:"display_name"`
	Enabled        bool          `json:"enabled"`
	ProvisionedVia string        `json:"provisioned_via"`
	Grants         []authz.Grant `json:"grants"`
	// SSOGrants come from identity provider group mappings, are replaced at every SSO sign-in and apply only to SSO
	// sessions.
	SSOGrants   []authz.Grant `json:"sso_grants"`
	HasPassword bool          `json:"has_password"`
	MFAEnrolled bool          `json:"mfa_enrolled"`
	MFARequired bool          `json:"mfa_required"`
	Version     int64         `json:"version"`
	CreatedAt   time.Time     `json:"created_at"`
	LastLoginAt *time.Time    `json:"last_login_at,omitempty"`
}

// Subject is the user's authorization subject.
func (u *User) Subject() string { return UserSubject(u.ID) }

// IsGlobalAdmin reports whether the user holds admin in the "*" domain.
func (u *User) IsGlobalAdmin() bool {
	return slices.ContainsFunc(u.Grants, func(g authz.Grant) bool { return g.Role == authz.RoleAdmin && g.Domain == authz.Global })
}

// Subject constructors (role_grants.subject).
func UserSubject(id string) string  { return "user:" + id }
func TokenSubject(id string) string { return "token:" + id }
func RootSubject(id string) string  { return "root:" + id }

// GrantInput is one requested role assignment.
type GrantInput struct {
	Role   authz.Role `json:"role" enum:"viewer,operator,admin"`
	Domain string     `json:"domain" doc:"A space ID, or \"*\" for every space and global administration"`
}

// ErrInvalidCredentials is the single response for every failed login.
var ErrInvalidCredentials = apperr.New(apperr.CodeUnauthorized, "invalid credentials")

// NormalizeEmail lower-cases and validates an email address.
func NormalizeEmail(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	if len(e) > 254 {
		return "", apperr.New(apperr.CodeInvalid, "email too long")
	}
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || !strings.Contains(e, "@") {
		return "", apperr.New(apperr.CodeInvalid, "invalid email address")
	}
	return e, nil
}

// validGrants checks roles and domains and returns them sorted and de-duplicated.
func validGrants(in []GrantInput) ([]GrantInput, error) {
	v := &apperr.Validation{}
	for i, g := range in {
		if !g.Role.Valid() {
			v.Add("grants", "entry %d: unknown role %q", i, g.Role)
		}
		if !authz.ValidDomain(g.Domain) {
			v.Add("grants", "entry %d: domain must be a space ID or \"*\"", i)
		}
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b GrantInput) int {
		if c := strings.Compare(a.Domain, b.Domain); c != 0 {
			return c
		}
		return strings.Compare(string(a.Role), string(b.Role))
	})
	return slices.Compact(out), nil
}

// Get returns a user.
func (svc *Service) Get(ctx context.Context, id string) (*User, error) {
	r, err := svc.s.R().GetUser(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.CodeNotFound, "user not found")
	}
	if err != nil {
		return nil, err
	}
	return svc.hydrate(ctx, svc.s.R(), r)
}

// List returns all users.
func (svc *Service) List(ctx context.Context) ([]*User, error) {
	rows, err := svc.s.R().ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*User, 0, len(rows))
	for _, r := range rows {
		u, err := svc.hydrate(ctx, svc.s.R(), r)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func grantsFromRows(rows []db.RoleGrant) []authz.Grant {
	out := make([]authz.Grant, 0, len(rows))
	for _, r := range rows {
		out = append(out, authz.Grant{Subject: r.Subject, Role: authz.Role(r.Role), Domain: r.Domain, Source: r.Source, SourceRef: r.SourceRef})
	}
	return out
}

func (svc *Service) hydrate(ctx context.Context, q *db.Queries, r db.User) (*User, error) {
	u := &User{ID: r.ID, Email: r.Email.String, DisplayName: r.DisplayName, Enabled: r.Enabled == 1, ProvisionedVia: r.ProvisionedVia,
		Version: r.Version, CreatedAt: time.UnixMicro(r.CreatedAt).UTC()}
	if r.LastLoginAt.Valid {
		t := time.UnixMicro(r.LastLoginAt.Int64).UTC()
		u.LastLoginAt = &t
	}
	rows, err := q.ListRoleGrantsBySubject(ctx, UserSubject(r.ID))
	if err != nil {
		return nil, err
	}
	u.Grants = grantsFromRows(rows)
	sso, err := q.ListRoleGrantsBySubject(ctx, SSOSubject(r.ID))
	if err != nil {
		return nil, err
	}
	u.SSOGrants = grantsFromRows(sso)
	if _, err := q.GetLocalCredential(ctx, r.ID); err == nil {
		u.HasPassword = true
	}
	if t, err := q.GetTOTP(ctx, r.ID); err == nil && t.ConfirmedAt.Valid {
		u.MFAEnrolled = true
	}
	u.MFARequired = r.MfaRequired == 1 || (u.HasPassword && (svc.opts.RequireTOTP || privileged(u.Grants)))
	return u, nil
}

// privileged reports whether any grant is operator or admin: local accounts holding one must enroll TOTP regardless
// of the global require_totp setting (as Hallpass requires MFA for administrators and approvers).
func privileged(gs []authz.Grant) bool {
	return slices.ContainsFunc(gs, func(g authz.Grant) bool { return g.Role == authz.RoleOperator || g.Role == authz.RoleAdmin })
}

// CreateInput creates a user.
type CreateInput struct {
	Email       string       `json:"email,omitempty"`
	DisplayName string       `json:"display_name"`
	Grants      []GrantInput `json:"grants"`
	Password    string       `json:"password,omitempty" doc:"Optional; otherwise invite or reset"`
	MFARequired bool         `json:"mfa_required,omitempty"`
}

// Create adds a user. New users have only the grants explicitly given. The API layer enforces user:write in the global
// domain, so only global administrators (or the root token) can create users.
func (svc *Service) Create(ctx context.Context, actor audit.Actor, in CreateInput) (*User, error) {
	via := "admin"
	if actor.Type == audit.ActorRoot {
		via = "bootstrap"
	}
	return svc.create(ctx, actor, in, via)
}

func (svc *Service) create(ctx context.Context, actor audit.Actor, in CreateInput, via string) (*User, error) {
	var email sql.NullString
	if in.Email != "" {
		e, err := NormalizeEmail(in.Email)
		if err != nil {
			return nil, err
		}
		email = sql.NullString{String: e, Valid: true}
	}
	name := strings.TrimSpace(in.DisplayName)
	if name == "" || len(name) > 128 {
		return nil, apperr.New(apperr.CodeInvalid, "display name is required (at most 128 bytes)")
	}
	grants, err := validGrants(in.Grants)
	if err != nil {
		return nil, err
	}
	var phc string
	if in.Password != "" {
		if phc, err = svc.hash(ctx, in.Password); err != nil {
			return nil, err
		}
	}
	id := ids.New(ids.User)
	err = svc.s.W().TxNamed(ctx, "auth.create", func(tx *store.Tx) error {
		if email.Valid {
			if _, err := tx.Q.GetUserByEmail(ctx, email); err == nil {
				return apperr.New(apperr.CodeAlreadyExists, "a user with this email already exists")
			}
		}
		if err := tx.Q.InsertUser(ctx, db.InsertUserParams{ID: id, Email: email, DisplayName: name, Enabled: 1, ProvisionedVia: via,
			MfaRequired: b2i(in.MFARequired), CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor), UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := svc.insertGrants(ctx, tx, UserSubject(id), grants, "manual", actor); err != nil {
			return err
		}
		if phc != "" {
			if err := tx.Q.UpsertLocalCredential(ctx, db.UpsertLocalCredentialParams{UserID: id, PasswordPhc: phc,
				ParamsVersion: int64(CurrentParams.Version), ChangedAt: tx.NowMicros()}); err != nil {
				return err
			}
		}
		svc.publish(tx, id)
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "user.created", Target: audit.Target{Type: "user", ID: id},
			Details: map[string]any{"email": email.String, "grants": grants, "via": via, "has_password": phc != ""}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, id)
}

func (svc *Service) insertGrants(ctx context.Context, tx *store.Tx, subject string, grants []GrantInput, source string, actor audit.Actor) error {
	for _, g := range grants {
		if err := tx.Q.InsertRoleGrant(ctx, db.InsertRoleGrantParams{Subject: subject, Role: string(g.Role), Domain: g.Domain, Source: source,
			CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor)}); err != nil {
			return err
		}
	}
	if len(grants) > 0 {
		svc.reloadAfterCommit(ctx, tx)
	}
	return nil
}

// RemoveDomainGrants deletes every grant in one domain (a purged space) inside the caller's transaction and, once it
// commits, reloads the authorization engine and has open streams re-check their access, exactly as any other grant
// change does. Without the reload the engine kept the deleted grants in memory until the next unrelated reload.
func (svc *Service) RemoveDomainGrants(ctx context.Context, tx *store.Tx, domain string) error {
	if domain == "" || domain == "*" {
		return apperr.New(apperr.CodeInvalid, "only a space's grants can be removed this way")
	}
	n, err := tx.Q.DeleteRoleGrantsByDomain(ctx, domain)
	if err != nil {
		return err
	}
	if n > 0 {
		svc.reloadAfterCommit(ctx, tx)
	}
	return nil
}

// reloadAfterCommit refreshes the authorization engine once tx commits, so grant changes apply to the next request,
// and then has every open stream re-check its authorization (SM-AUTH-007).
func (svc *Service) reloadAfterCommit(ctx context.Context, tx *store.Tx) {
	tx.AfterCommit(func() {
		if svc.az != nil {
			_ = svc.az.Reload(context.WithoutCancel(ctx))
		}
		svc.rev.signal(revocation{all: true})
		if svc.bus != nil {
			svc.bus.Publish(eventbus.Event{Topic: eventbus.TopicGrantsChanged})
		}
	})
}

// SetGrants replaces a user's manual grants. Removing the last enabled global administrator, or under SSO-only
// enforcement the last one who can sign in with SSO, is refused (Hallpass HP-AUTH-015). Any change of
// the effective grants revokes the user's sessions: a reduction must take effect at once and an elevation is only
// granted to a new session (rotation on privilege change).
func (svc *Service) SetGrants(ctx context.Context, actor audit.Actor, id string, in []GrantInput) (*User, error) {
	grants, err := validGrants(in)
	if err != nil {
		return nil, err
	}
	err = svc.s.W().TxNamed(ctx, "auth.set_grants", func(tx *store.Tx) error {
		row, err := tx.Q.GetUser(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "user not found")
		} else if err != nil {
			return err
		}
		before, err := svc.hydrate(ctx, tx.Q, row)
		if err != nil {
			return err
		}
		guard, err := newSSOAdminGuard(ctx, tx.Q)
		if err != nil {
			return err
		}
		if err := tx.Q.DeleteRoleGrantsBySource(ctx, db.DeleteRoleGrantsBySourceParams{Subject: UserSubject(id), Source: "manual"}); err != nil {
			return err
		}
		if err := svc.insertGrants(ctx, tx, UserSubject(id), grants, "manual", actor); err != nil {
			return err
		}
		if err := ensureAdminRemains(ctx, tx); err != nil {
			return err
		}
		if err := guard.check(ctx, tx.Q); err != nil {
			return err
		}
		after, err := svc.hydrate(ctx, tx.Q, row)
		if err != nil {
			return err
		}
		svc.reloadAfterCommit(ctx, tx)
		if !sameGrants(before.Grants, after.Grants) {
			if err := svc.revokeUserSessionsTx(ctx, tx, id, "grants_changed", nil); err != nil {
				return err
			}
		}
		svc.publish(tx, id)
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "user.grants.set", Target: audit.Target{Type: "user", ID: id},
			Details: map[string]any{"before": grantSummary(before.Grants), "after": grantSummary(after.Grants)}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, id)
}

func grantSummary(gs []authz.Grant) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, string(g.Role)+"@"+g.Domain+"("+g.Source+")")
	}
	return out
}

func sameGrants(a, b []authz.Grant) bool { return slices.Equal(grantSummary(a), grantSummary(b)) }

// ErrLastAdmin protects against administrator lockout.
var ErrLastAdmin = apperr.New(apperr.CodePrecondition, "this change would leave no enabled global administrator")

func ensureAdminRemains(ctx context.Context, tx *store.Tx) error {
	n, err := tx.Q.CountEnabledGlobalAdmins(ctx)
	if err != nil {
		return err
	}
	users, err := tx.Q.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n == 0 && users > 0 {
		return ErrLastAdmin
	}
	return nil
}

// SetEnabled enables or disables a user; disabling revokes all sessions and is refused for the last enabled global
// administrator and, under SSO-only enforcement, for the last one who can sign in with SSO. Disabling also stops the user's API tokens (they are checked against the user on every request).
func (svc *Service) SetEnabled(ctx context.Context, actor audit.Actor, id string, enabled bool) (*User, error) {
	err := svc.s.W().TxNamed(ctx, "auth.set_enabled", func(tx *store.Tx) error {
		guard, err := newSSOAdminGuard(ctx, tx.Q)
		if err != nil {
			return err
		}
		n, err := tx.Q.SetUserEnabled(ctx, db.SetUserEnabledParams{Enabled: b2i(enabled), Now: tx.NowMicros(), ID: id})
		if err != nil || n == 0 {
			if _, gerr := tx.Q.GetUser(ctx, id); errors.Is(gerr, sql.ErrNoRows) {
				return apperr.New(apperr.CodeNotFound, "user not found")
			}
			return err
		}
		if err := ensureAdminRemains(ctx, tx); err != nil {
			return err
		}
		if err := guard.check(ctx, tx.Q); err != nil {
			return err
		}
		if !enabled {
			if err := svc.revokeUserSessionsTx(ctx, tx, id, "disabled", nil); err != nil {
				return err
			}
		}
		action := "user.enabled"
		if !enabled {
			action = "user.disabled"
		}
		svc.publish(tx, id)
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: action, Target: audit.Target{Type: "user", ID: id}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, id)
}

func (svc *Service) publish(tx *store.Tx, id string) {
	tx.AfterCommit(func() {
		if svc.bus != nil {
			svc.bus.Publish(eventbus.Event{Topic: eventbus.TopicUserChanged, Key: id})
		}
	})
}

// hash normalizes and hashes a password under the concurrency limit.
func (svc *Service) hash(ctx context.Context, pw string) (string, error) {
	n, err := NormalizePassword(pw, svc.opts.PasswordMinBytes)
	if err != nil {
		return "", err
	}
	if err := svc.acquire(ctx); err != nil {
		return "", err
	}
	defer svc.release()
	return HashPassword(n, CurrentParams)
}

func (svc *Service) acquire(ctx context.Context) error {
	select {
	case svc.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (svc *Service) release() { <-svc.sem }

// randomToken returns a 256-bit URL-safe random token.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// tokenMAC derives the stored lookup key of a token (HMAC with the token key).
func (svc *Service) tokenMAC(token string) ([]byte, error) {
	return svc.kr.MAC(keyring.PurposeToken, []byte("auth|"+token))
}

func (svc *Service) tokenMACs(token string) ([][]byte, error) {
	return svc.kr.MACAll(keyring.PurposeToken, []byte("auth|"+token))
}

func throttleKeyAccount(email string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return "acct:" + hex.EncodeToString(h[:12])
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func actorRef(a audit.Actor) string {
	if a.ID == "" {
		return a.Type
	}
	return a.Type + ":" + a.ID
}

func newUserID() string { return ids.New(ids.User) }
