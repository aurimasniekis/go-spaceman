package app_test

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// SM-SPACE-004, SM-AUTH-004 (security review SR-L02): purging a space deletes its role grants through the auth service,
// so the authorization engine forgets them as soon as the purge commits, without waiting for an unrelated reload.
// Grants in other spaces are kept.
func TestPurgeSpaceForgetsItsGrants(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	ctx := context.Background()
	e.enroll()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	mk := func(slug string) *fleet.Space {
		sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: slug, Name: slug, NodeID: nodes[0].ID})
		if err != nil {
			t.Fatal(err)
		}
		return sp
	}
	gone, kept := mk("purged"), mk("kept")
	u, err := e.c.Auth.Create(ctx, audit.System, auth.CreateInput{Email: "op@example.test", DisplayName: "Op",
		Grants: []auth.GrantInput{{Role: authz.RoleOperator, Domain: gone.ID}, {Role: authz.RoleOperator, Domain: kept.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	subj := auth.UserSubject(u.ID)
	if !e.c.Authz.Allowed(subj, gone.ID, authz.RunControl) || !e.c.Authz.Allowed(subj, kept.ID, authz.RunControl) {
		t.Fatal("grants not effective before the purge")
	}
	sp, _ := e.c.Fleet.GetSpace(ctx, gone.ID)
	if _, err := e.c.Fleet.SetSpaceState(ctx, admin, gone.ID, sp.Version, "archived", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Fleet.PurgeSpace(ctx, admin, gone.ID); err != nil {
		t.Fatal(err)
	}
	// The reload runs after commit, before PurgeSpace returns.
	if e.c.Authz.Allowed(subj, gone.ID, authz.RunControl) || e.c.Authz.Allowed(subj, gone.ID, authz.SpaceRead) {
		t.Fatal("the purged space's grant is still effective in the authorization engine")
	}
	for _, d := range e.c.Authz.Domains(subj) {
		if d == gone.ID {
			t.Fatalf("purged domain %s still listed for the user", d)
		}
	}
	if !e.c.Authz.Allowed(subj, kept.ID, authz.RunControl) {
		t.Fatal("purge removed a grant in another space")
	}
}
