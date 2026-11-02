package auth

import (
	"context"
	"slices"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// Role aliases mapping the inherited Hallpass test vocabulary onto SpaceMan's built-in roles.
const (
	RoleAdministrator = authz.RoleAdmin
	RoleApprover      = authz.RoleOperator
	RoleAuditor       = authz.RoleViewer
	RoleViewer        = authz.RoleViewer
)

func mustAuthz(t testing.TB, s *store.Store) *authz.Engine {
	t.Helper()
	e, err := authz.New(context.Background(), s.R())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// grants returns one global grant per role.
func grants(rs ...authz.Role) []GrantInput {
	out := make([]GrantInput, 0, len(rs))
	for _, r := range rs {
		out = append(out, GrantInput{Role: r, Domain: authz.Global})
	}
	return out
}

// roles returns the user's distinct roles, sorted.
func roles(u *User) []authz.Role {
	out := []authz.Role{}
	for _, g := range u.Grants {
		if !slices.Contains(out, g.Role) {
			out = append(out, g.Role)
		}
	}
	slices.Sort(out)
	return out
}

func hasRole(p *Principal, r authz.Role) bool {
	return p.User != nil && slices.Contains(roles(p.User), r)
}
