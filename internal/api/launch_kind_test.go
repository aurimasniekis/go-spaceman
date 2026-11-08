package api

import (
	"context"
	"errors"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/gateway"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

type staticGrants []db.RoleGrant

func (g staticGrants) ListRoleGrants(context.Context) ([]db.RoleGrant, error) { return g, nil }

// A diagnostic launch, with its kind as resolved (a space role may set it), needs run:diagnose; an internal principal
// (an agent's SpaceMan MCP call, ADR-0063) also needs the gateway to have approved this call as a dangerous launch-run.
func TestAuthorizeLaunchKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ip := internalPrincipal()
	az, err := authz.New(ctx, staticGrants{
		{Subject: auth.UserSubject("usr_owner"), Role: string(authz.RoleAdmin), Domain: authz.Global},
		{Subject: auth.TokenSubject(ip.TokenID), Role: string(authz.RoleAdmin), Domain: authz.Global},
		{Subject: auth.UserSubject("usr_op"), Role: string(authz.RoleViewer), Domain: authz.Global},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &Deps{Authz: az}
	with := func(p *auth.Principal, approved string) context.Context {
		c := context.WithValue(ctx, principalKey{}, p)
		if approved != "" {
			c = gateway.WithApprovedCall(c, approved)
		}
		return c
	}
	admin := &auth.Principal{User: &auth.User{ID: "usr_owner"}, SessionID: "ses_1", Method: auth.MethodPassword}
	viewer := &auth.Principal{User: &auth.User{ID: "usr_op"}, SessionID: "ses_2", Method: auth.MethodPassword}
	for _, tc := range []struct {
		name     string
		p        *auth.Principal
		approved string
		kind     string
		ok       bool
	}{
		{"worker by an internal principal", ip, "", "worker", true},
		{"diagnostic by a person", admin, "", "diagnostic", true},
		{"diagnostic without run:diagnose", viewer, "", "diagnostic", false},
		{"diagnostic by an internal principal, not approved", ip, "", "diagnostic", false},
		{"diagnostic by an internal principal, another call approved", ip, "delete-schedule", "diagnostic", false},
		{"diagnostic by an internal principal, approved", ip, "launch-run", "diagnostic", true},
	} {
		err := d.authorizeLaunchKind(with(tc.p, tc.approved), "spc_1", tc.kind)
		if tc.ok != (err == nil) || (err != nil && !errors.Is(err, apperr.ErrForbidden)) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}
