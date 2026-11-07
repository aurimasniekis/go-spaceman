package app

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// The approver re-check (SM-APPR-003) never counts a SpaceMan MCP call as an approver (ADR-0063): not by its "via",
// and not when an internal token is named as an API token, even though its owner and grants could decide.
func TestApproverRecheckRefusesSpacemanMCP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)
	kr := keyringtest.Load(t, s)
	e, err := authz.New(ctx, s.R())
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.New(s, kr, audit.NewWriter(kr), eventbus.New(), e, auth.Options{})
	sys := audit.Actor{Type: audit.ActorSystem, ID: "test"}
	u, err := svc.Create(ctx, sys, auth.CreateInput{Email: "admin@example.com", DisplayName: "Admin",
		Grants: []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}})
	if err != nil {
		t.Fatal(err)
	}
	owner := &auth.Principal{User: u}
	bearer, _, err := svc.CreateAPIToken(ctx, owner, "ci", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}, 30*24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	internal, err := svc.CreateInternalToken(ctx, owner, auth.InternalTokenInput{Purpose: auth.TokenPurposeSpacemanMCP, Name: "SpaceMan",
		Grants: []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}, NeverExpires: true})
	if err != nil {
		t.Fatal(err)
	}
	c := &Central{Store: s, Authz: e}
	const space = "spc_0000000000000000000000000x"
	for name, tc := range map[string]struct {
		by   audit.Actor
		want bool
	}{
		"user":                    {audit.Actor{Type: audit.ActorUser, ID: u.ID}, true},
		"bearer token":            {audit.Actor{Type: audit.ActorUser, ID: u.ID, Via: "api_token:" + bearer.ID}, true},
		"spaceman mcp":            {audit.Actor{Type: audit.ActorUser, ID: u.ID, Via: auth.ViaSpacemanMCP + internal.ID}, false},
		"spaceman mcp, run only":  {audit.Actor{Type: audit.ActorUser, ID: u.ID, Run: "run_x"}, false},
		"internal token as token": {audit.Actor{Type: audit.ActorUser, ID: u.ID, Via: "api_token:" + internal.ID}, false},
	} {
		if got := c.approverStillAuthorized(ctx, tc.by, space); got != tc.want {
			t.Errorf("%s: authorized %v, want %v", name, got, tc.want)
		}
	}
}
