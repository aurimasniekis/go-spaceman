package api

import (
	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
)

// Rule is the authorization rule of one operation (SM-AUTH-004).
type Rule struct {
	// Perm is checked with Casbin for every subject of the principal (AllowedAll). Zero with Public or Session.
	Perm authz.Permission
	// Public operations need no authentication (sign-in, system info).
	Public bool
	// Session operations need any authenticated principal and no specific permission (own profile, sign-out).
	Session bool
	// Limited operations are also allowed while MFA is still pending (MFA verification and enrollment).
	Limited bool
	// Space takes the Casbin domain from the {space_id} path parameter instead of "*".
	Space bool
	// Reauth requires authentication within the re-auth window; MFA additionally a fresh second factor (step-up).
	Reauth, MFA bool
	// UserOnly refuses the root token (operations that act on the caller's own user account).
	UserOnly bool
}

func public() Rule { return Rule{Public: true} }

// Global is the rule of a global operation (domain "*").
func Global(p authz.Permission) Rule { return Rule{Perm: p} }

// InSpace is the rule of a space-scoped operation: the Casbin domain is the {space_id} path parameter.
func InSpace(p authz.Permission) Rule { return Rule{Perm: p, Space: true} }

// StepUp is a global rule that also requires a recent re-authentication with MFA.
func StepUp(p authz.Permission) Rule { return Rule{Perm: p, Reauth: true, MFA: true} }

// rules is the authorization table of the core API. Feature packages add theirs through Deps.ExtraRules.
var rules = map[string]Rule{
	"get-system-info": public(),

	"login":           public(),
	"verify-mfa":      {Session: true, Limited: true, UserOnly: true},
	"start-enroll":    {Session: true, Limited: true, UserOnly: true},
	"confirm-enroll":  {Session: true, Limited: true, UserOnly: true},
	"get-me":          {Session: true, Limited: true},
	"logout":          {Session: true, Limited: true},
	"reauth-password": {Session: true, UserOnly: true},
	"list-sessions":   {Session: true, UserOnly: true},
	"revoke-session":  {Session: true, UserOnly: true},
	"change-password": {Session: true, UserOnly: true, Reauth: true},
	// A signed-in user's own notification preferences (SM-HUI-006).
	"get-notification-preferences": {Session: true, UserOnly: true},
	"set-notification-preferences": {Session: true, UserOnly: true},
	// Token redemption is public: the single-use token is the credential (throttled and audited by the auth service).
	"reset-password":            public(),
	"recovery-login":            public(),
	"regenerate-recovery-codes": {Session: true, UserOnly: true, Reauth: true, MFA: true},
	"get-break-glass":           Global(authz.SystemRead),
	"acknowledge-break-glass":   StepUp(authz.SystemWrite),

	"list-users":      Global(authz.UserRead),
	"get-user":        Global(authz.UserRead),
	"create-user":     StepUp(authz.UserWrite),
	"set-user-grants": StepUp(authz.UserWrite),
	"enable-user":     StepUp(authz.UserWrite),
	"disable-user":    StepUp(authz.UserWrite),
	"issue-reset":     StepUp(authz.UserWrite),

	"list-my-tokens":     {Session: true, UserOnly: true},
	"create-api-token":   {Session: true, UserOnly: true, Reauth: true, MFA: true},
	"revoke-my-token":    {Session: true, UserOnly: true},
	"list-all-tokens":    Global(authz.TokenAdmin),
	"revoke-any-token":   StepUp(authz.TokenAdmin),
	"get-root-token":     Global(authz.SystemWrite),
	"rotate-root-token":  StepUp(authz.SystemWrite),
	"revoke-root-token":  StepUp(authz.SystemWrite),
	"list-audit":         Global(authz.AuditRead),
	"verify-audit-chain": Global(authz.AuditRead),
}

func (d *Deps) ruleFor(op string) (Rule, bool) {
	if r, ok := rules[op]; ok {
		return r, true
	}
	r, ok := d.ExtraRules[op]
	return r, ok
}

// authorizeOps enforces the operation's rule before its handler runs. Operations without a rule are unreachable.
func (d *Deps) authorizeOps(api huma.API) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		op := ctx.Operation()
		rl, ok := d.ruleFor(op.OperationID)
		if !ok {
			writeHumaProblem(ctx, errForbidden())
			return
		}
		if rl.Public {
			next(ctx)
			return
		}
		p := PrincipalFrom(ctx.Context())
		if p == nil {
			writeHumaProblem(ctx, errUnauthenticated())
			return
		}
		if p.Limited && !rl.Limited {
			writeHumaProblem(ctx, errLimited())
			return
		}
		if rl.UserOnly && p.User == nil {
			writeHumaProblem(ctx, newProblem(403, apperr.CodeForbidden, "this operation needs a user account, not the root token", nil))
			return
		}
		if !rl.Session {
			domain := authz.Global
			if rl.Space {
				domain = ctx.Param("space_id")
				if !authz.ValidDomain(domain) || domain == authz.Global {
					writeHumaProblem(ctx, newProblem(404, apperr.CodeNotFound, "space not found", nil))
					return
				}
			}
			if d.Authz == nil || !d.Authz.AllowedAll(p.Subjects(), domain, rl.Perm) {
				// A space the caller cannot read is indistinguishable from a missing one (SM-HUI-003).
				if rl.Space && (d.Authz == nil || !d.Authz.AllowedAll(p.Subjects(), domain, authz.SpaceRead)) {
					writeHumaProblem(ctx, newProblem(404, apperr.CodeNotFound, "space not found", nil))
					return
				}
				writeHumaProblem(ctx, errForbidden())
				return
			}
		}
		if p.Method != "" && p.TokenID == "" && p.RootTokenID == "" && mutating(op.Method) && !p.VerifyCSRF(ctx.Header(CSRFHeader)) {
			writeHumaProblem(ctx, errCSRF())
			return
		}
		if (rl.Reauth || rl.MFA) && p.TokenID == "" && p.RootTokenID == "" {
			if err := d.Auth.RequireRecentAuth(p, rl.MFA); err != nil {
				writeHumaProblem(ctx, problemFor(err))
				return
			}
		}
		next(ctx)
	}
}

func writeHumaProblem(ctx huma.Context, p *Problem) {
	ctx.SetHeader("Content-Type", "application/problem+json")
	ctx.SetHeader("Cache-Control", "no-store")
	ctx.SetStatus(p.Status)
	_ = jsonEncode(ctx.BodyWriter(), p)
}

var errRateLimited = apperr.New(apperr.CodeRateLimited, "too many attempts; try again later")
