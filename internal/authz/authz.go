// Package authz is SpaceMan's authorization engine (SM-AUTH-003/004, ADR-0004): Casbin RBAC with domains.
//
// A request is (subject, domain, object, action). Subjects are "user:<usr_…>", "token:<atk_…>" and "root:<rtk_…>".
// Domains are space IDs, or "*" for global administration. Grants g(subject, role, domain) are rows of the role_grants
// table; a grant in domain "*" applies to every space and to global objects. Role permissions (p rows) are defined in
// code (policy.go) so they are versioned with the binary and reviewed like code.
//
// The adapter is read-only: grants change only inside store write transactions, and Reload runs after commit.
// Authorization never depends on model output: agents are not Casbin subjects at all (ADR-0012).
package authz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	"github.com/casbin/casbin/v2/persist"

	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Global is the domain of global objects and of grants that apply to every space.
const Global = "*"

// modelText is the Casbin model. Role permissions are domain-independent (p = role, obj, act); domain scoping
// comes from the grant. A grant in "*" also matches every space domain.
const modelText = `
[request_definition]
r = sub, dom, obj, act

[policy_definition]
p = sub, obj, act

[role_definition]
g = _, _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = (g(r.sub, p.sub, r.dom) || g(r.sub, p.sub, "*")) && (p.obj == "*" || r.obj == p.obj) && (p.act == "*" || r.act == p.act)
`

// Grant is one role assignment.
type Grant struct {
	Subject   string `json:"subject"`
	Role      Role   `json:"role"`
	Domain    string `json:"domain"`
	Source    string `json:"source"`
	SourceRef string `json:"source_ref,omitempty"`
}

// GrantLoader reads every grant (the store reader in production).
type GrantLoader interface {
	ListRoleGrants(ctx context.Context) ([]db.RoleGrant, error)
}

// Engine evaluates permissions. It is safe for concurrent use.
type Engine struct {
	mu     sync.Mutex // serializes reloads
	e      *casbin.SyncedEnforcer
	loader GrantLoader
}

// New builds the engine and loads the grants.
func New(ctx context.Context, loader GrantLoader) (*Engine, error) {
	m, err := model.NewModelFromString(modelText)
	if err != nil {
		return nil, fmt.Errorf("authz model: %w", err)
	}
	ad := &adapter{ctx: ctx, loader: loader}
	e, err := casbin.NewSyncedEnforcer(m, ad)
	if err != nil {
		return nil, fmt.Errorf("authz enforcer: %w", err)
	}
	e.EnableAutoSave(false)
	return &Engine{e: e, loader: loader}, nil
}

// Reload re-reads every grant. Call it after a transaction that changed role_grants committed.
func (en *Engine) Reload(ctx context.Context) error {
	en.mu.Lock()
	defer en.mu.Unlock()
	if ad, ok := en.e.GetAdapter().(*adapter); ok {
		ad.ctx = ctx
	}
	return en.e.LoadPolicy()
}

// Allowed reports whether subject may perform perm in domain. Every error is a denial.
func (en *Engine) Allowed(subject, domain string, perm Permission) bool {
	if subject == "" || domain == "" || !perm.valid() {
		return false
	}
	if perm.GlobalOnly && domain != Global {
		return false
	}
	ok, err := en.e.Enforce(subject, domain, perm.Object, perm.Action)
	return err == nil && ok
}

// AllowedAll reports whether every subject may perform perm in domain. Tokens are authorized as both the token and
// the user who created it, so a token can never exceed its creator's current grants.
func (en *Engine) AllowedAll(subjects []string, domain string, perm Permission) bool {
	if len(subjects) == 0 {
		return false
	}
	for _, s := range subjects {
		if !en.Allowed(s, domain, perm) {
			return false
		}
	}
	return true
}

// Domains returns the domains in which subject holds any grant ("*" first when present). The global human-action page
// and other cross-space views filter by this list (SM-HUI-003).
func (en *Engine) Domains(subject string) []string {
	var out []string
	rows, err := en.e.GetFilteredGroupingPolicy(0, subject)
	if err != nil {
		return nil
	}
	for _, r := range rows {
		if len(r) >= 3 && !slices.Contains(out, r[2]) {
			out = append(out, r[2])
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		switch {
		case a == b:
			return 0
		case a == Global:
			return -1
		case b == Global:
			return 1
		}
		return strings.Compare(a, b)
	})
	return out
}

// RolesIn returns subject's roles effective in domain (grants in the domain plus global grants).
func (en *Engine) RolesIn(subject, domain string) []Role {
	var out []Role
	rows, err := en.e.GetFilteredGroupingPolicy(0, subject)
	if err != nil {
		return nil
	}
	for _, r := range rows {
		if len(r) >= 3 && (r[2] == domain || r[2] == Global) && !slices.Contains(out, Role(r[1])) {
			out = append(out, Role(r[1]))
		}
	}
	slices.Sort(out)
	return out
}

// Covers reports whether holder's grants include every permission of want (used to keep token scopes within their
// creator's grants): for each wanted grant, holder must hold an equal or stronger role in the same domain or in "*".
func Covers(holder []Grant, want Grant) bool {
	for _, h := range holder {
		if (h.Domain == want.Domain || h.Domain == Global) && h.Role.atLeast(want.Role) {
			return true
		}
	}
	return false
}

// ValidDomain reports whether d is "*" or a space ID.
func ValidDomain(d string) bool {
	return d == Global || (strings.HasPrefix(d, "spc_") && len(d) == 30)
}

// adapter loads policies from code and grants from the store. Writes are not supported: grants change only
// through store transactions.
type adapter struct {
	ctx    context.Context //nolint:containedctx // casbin's adapter interface has no context parameter
	loader GrantLoader
}

var errReadOnly = errors.New("authz: the policy adapter is read-only; change grants through the store")

func (a *adapter) LoadPolicy(m model.Model) error {
	for _, p := range policyRows() {
		if err := persist.LoadPolicyArray(append([]string{"p"}, p...), m); err != nil {
			return err
		}
	}
	if a.loader == nil {
		return nil
	}
	rows, err := a.loader.ListRoleGrants(a.ctx)
	if err != nil {
		return fmt.Errorf("load grants: %w", err)
	}
	for _, r := range rows {
		if !Role(r.Role).Valid() || !ValidDomain(r.Domain) {
			continue // the schema CHECK and service validation make this unreachable; never widen on bad data
		}
		if err := persist.LoadPolicyArray([]string{"g", r.Subject, r.Role, r.Domain}, m); err != nil {
			return err
		}
	}
	return nil
}

func (a *adapter) SavePolicy(model.Model) error                              { return errReadOnly }
func (a *adapter) AddPolicy(string, string, []string) error                  { return errReadOnly }
func (a *adapter) RemovePolicy(string, string, []string) error               { return errReadOnly }
func (a *adapter) RemoveFilteredPolicy(string, string, int, ...string) error { return errReadOnly }
