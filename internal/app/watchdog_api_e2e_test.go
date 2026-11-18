package app_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// SM-CTRL-008: a space's watchdog state needs run:read in that space. A viewer of space A reads A's state and is
// refused B's; a caller whose only grant is in space C has no run:read in A or B and is refused both.
func TestWatchdogAPIAuthorization(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	mk := func(slug string) *fleet.Space {
		sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: slug, Name: slug, NodeID: nodes[0].ID})
		if err != nil {
			t.Fatal(err)
		}
		return sp
	}
	spA, spB, spC := mk("wda"), mk("wdb"), mk("wdc")
	mkToken := func(email string, grants []auth.GrantInput) string {
		t.Helper()
		u, err := e.c.Auth.Create(ctx, audit.System, auth.CreateInput{Email: email, DisplayName: email, Grants: grants})
		if err != nil {
			t.Fatal(err)
		}
		u, _ = e.c.Auth.Get(ctx, u.ID)
		_, tok, err := e.c.Auth.CreateAPIToken(ctx, &auth.Principal{User: u}, "wd", grants, 24*time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	viewerA := mkToken("wda@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: spA.ID}})
	// Holds run:read only in space C, so none in A or B.
	noRead := mkToken("wdn@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: spC.ID}})
	get := func(tok, spaceID string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+api.Prefix+"/spaces/"+spaceID+"/watchdog", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	if st := get(viewerA, spA.ID); st != http.StatusOK {
		t.Fatalf("viewer of A reading A: %d", st)
	}
	for _, tc := range []struct{ who, tok, space string }{{"viewer of A reading B", viewerA, spB.ID}, {"holder of C reading A", noRead, spA.ID}, {"holder of C reading B", noRead, spB.ID}} {
		if st := get(tc.tok, tc.space); st != http.StatusNotFound && st != http.StatusForbidden {
			t.Errorf("%s: %d", tc.who, st)
		}
	}
}
