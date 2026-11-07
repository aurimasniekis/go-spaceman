package harborpilot

import (
	"context"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hptest"
)

// TestRealHarborPilotConnection validates connections and space settings against a real `harbor-pilot serve --demo`
// v0.1.3 (opt-in: SPACEMAN_HARBOR_PILOT=1, make test-harbor-pilot). Checked:
//   - a parent with tokens:mint connects, and the probe learns its client binding and leaves no live credential;
//   - a parent without tokens:mint is refused as "parent token lacks tokens:mint" (Harbor Pilot's real 403);
//   - a service-principal token reads budget status and projects; a gateway token in its place is refused;
//   - a space's project is checked: an allowed project id passes, a project name is refused with its id, an unknown
//     project is refused, and without a service-principal token Harbor Pilot's own mint check refuses a project the
//     binding doesn't allow.
func TestRealHarborPilotConnection(t *testing.T) {
	g := hptest.Start(t)
	svc, _ := newSvc(t)
	ctx := context.Background()

	_, noMint, err := g.IssueCredential(ctx, "spaceman-parent-no-mint", []string{ScopeInvoke, ScopeModels})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.PutConnection(ctx, admin, 0, ConnectionInput{BaseURL: g.URL, AllowInsecureHTTP: true, ParentToken: noMint})
	fieldMsg(t, err, "parent_token", "parent token lacks tokens:mint")
	_, err = svc.PutConnection(ctx, admin, 0, ConnectionInput{BaseURL: g.URL, AllowInsecureHTTP: true, ParentToken: g.ParentToken, ServiceToken: g.ParentToken})
	fieldMsg(t, err, "service_token", "is a gateway token")

	c, err := svc.PutConnection(ctx, admin, 0, ConnectionInput{BaseURL: g.URL, AllowInsecureHTTP: true, ParentToken: g.ParentToken, ServiceToken: g.ServiceToken})
	if err != nil {
		t.Fatal(err)
	}
	if c.BindingID != g.BindingID || !c.HasServiceToken || c.ParentExpiresAt != nil {
		t.Fatalf("connection %+v (binding want %s)", c, g.BindingID)
	}

	// The probe credentials were revoked: none of the parent's children is left active.
	assertNoChildren := func() {
		t.Helper()
		if live, err := g.ActiveChildren(ctx); err != nil || len(live) != 0 {
			t.Fatalf("active children after validation: %v %v", live, err)
		}
	}
	assertNoChildren()

	sp, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: g.AltProjectID})
	if err != nil || !sp.Enabled || sp.ProjectID != g.AltProjectID {
		t.Fatalf("space %+v %v", sp, err)
	}
	_, err = svc.PutSpace(ctx, admin, "spc_b", 0, SpaceInput{Enabled: true, ProjectID: "web-app"})
	fieldMsg(t, err, "project_id", "use its id "+g.ProjectID)
	_, err = svc.PutSpace(ctx, admin, "spc_b", 0, SpaceInput{Enabled: true, ProjectID: "prj_doesnotexist"})
	fieldMsg(t, err, "project_id", "has no project")

	// Without a service-principal token, the mint probe is the check: Harbor Pilot refuses a name (it allows ids only).
	c, err = svc.PutConnection(ctx, admin, c.Version, ConnectionInput{BaseURL: g.URL, AllowInsecureHTTP: true, RemoveServiceToken: true})
	if err != nil || c.HasServiceToken {
		t.Fatalf("remove service token %+v %v", c, err)
	}
	_, err = svc.PutSpace(ctx, admin, "spc_b", 0, SpaceInput{Enabled: true, ProjectID: "web-app"})
	fieldMsg(t, err, "project_id", "refused project")

	// Deleting the connection is refused while spc_a uses it.
	if err := svc.DeleteConnection(ctx, admin, c.Version); err == nil || !strings.Contains(err.Error(), "route runs through Harbor Pilot") {
		t.Fatalf("delete while enabled: %v", err)
	}
	code(t, svc.DeleteConnection(ctx, admin, c.Version), apperr.CodePrecondition)
	assertNoChildren()
}
