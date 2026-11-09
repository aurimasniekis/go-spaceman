package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// SM-AUTH-007, SM-UI-004: the live terminal ends at once when the viewer's access does: "forbidden" when the viewer
// loses terminal:watch in the space (a grant removed), "unauthenticated" when the token, SSO session or root token that
// opened it is revoked. Revoking a viewer never stops the run itself, and a stopped run's credential fails the next
// tool call.
func TestTerminalStreamEndsOnRevocation(t *testing.T) {
	// Not parallel: central restarts must finish within the node lease and the fleet tests' wall-clock bounds (such as
	// the 5 s restart check), which parallel tests can starve.
	e := newFleetEnv(t)
	e.cfg.Auth.StreamRevalidate = config.Duration(time.Minute) // only the event-driven path meets revocationBound
	e.restartCentral()
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "revoke", Name: "Revoke", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindCodex})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "running", func() bool { return e.run1(r.ID).Observed == "running" })
	eventually(t, "recording shipped", func() bool {
		res, err := e.c.Fleet.Recordings().Read(context.Background(), r.ID, 0)
		return err == nil && len(res.Records) > 0
	})
	termURL := e.srv.URL + api.Prefix + "/spaces/" + sp.ID + "/runs/" + r.ID + "/terminal"
	sys := audit.System
	if _, err := e.c.Auth.Create(ctx, sys, auth.CreateInput{Email: "adm@example.com", DisplayName: "Admin", Grants: []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}}); err != nil {
		t.Fatal(err)
	}
	operator := []auth.GrantInput{{Role: authz.RoleOperator, Domain: sp.ID}}
	vic, err := e.c.Auth.Create(ctx, sys, auth.CreateInput{Email: "vic@example.com", DisplayName: "Vic", Grants: operator})
	if err != nil {
		t.Fatal(err)
	}
	token := func() string {
		t.Helper()
		u, err := e.c.Auth.Get(ctx, vic.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, secret, err := e.c.Auth.CreateAPIToken(ctx, &auth.Principal{User: u}, "term", operator, 24*time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		return secret
	}
	ends := func(t *testing.T, ch <-chan sseEvent, want string) {
		t.Helper()
		if got := waitEnd(t, ch, revocationBound); got != want {
			t.Fatalf("end reason %q, want %q", got, want)
		}
	}

	t.Run("grant removed", func(t *testing.T) {
		ch := openSSE(t, http.DefaultClient, termURL, "Authorization", "Bearer "+token())
		if _, err := e.c.Auth.SetGrants(ctx, sys, vic.ID, []auth.GrantInput{{Role: authz.RoleViewer, Domain: sp.ID}}); err != nil {
			t.Fatal(err)
		}
		ends(t, ch, api.EndForbidden)
		if _, err := e.c.Auth.SetGrants(ctx, sys, vic.ID, operator); err != nil {
			t.Fatal(err)
		}
	})

	// The stream is scoped by the run and the caller's grant in the run's own space: a grant in another space opens
	// neither this run under that space's path nor under its own (both are "not found", never another run's output).
	t.Run("another space's grant", func(t *testing.T) {
		other, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "elsewhere", Name: "Elsewhere", NodeID: nodes[0].ID})
		if err != nil {
			t.Fatal(err)
		}
		elsewhere := []auth.GrantInput{{Role: authz.RoleOperator, Domain: other.ID}}
		eve, err := e.c.Auth.Create(ctx, sys, auth.CreateInput{Email: "eve@example.com", DisplayName: "Eve", Grants: elsewhere})
		if err != nil {
			t.Fatal(err)
		}
		_, secret, err := e.c.Auth.CreateAPIToken(ctx, &auth.Principal{User: eve}, "term", elsewhere, 24*time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range []string{
			e.srv.URL + api.Prefix + "/spaces/" + other.ID + "/runs/" + r.ID + "/terminal",
			termURL,
		} {
			req, _ := http.NewRequest(http.MethodGet, u, nil)
			req.Header.Set("Authorization", "Bearer "+secret)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != http.StatusNotFound || res.Header.Get("Content-Type") == "text/event-stream" {
				t.Fatalf("%s: %d %s", u, res.StatusCode, res.Header.Get("Content-Type"))
			}
		}
	})

	t.Run("API token revoked", func(t *testing.T) {
		secret := token()
		ch := openSSE(t, http.DefaultClient, termURL, "Authorization", "Bearer "+secret)
		p, err := e.c.Auth.AuthenticateAPIToken(ctx, secret, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.c.Auth.RevokeAPIToken(ctx, sys, p.TokenID); err != nil {
			t.Fatal(err)
		}
		ends(t, ch, api.EndUnauthenticated)
	})

	t.Run("SSO session revoked", func(t *testing.T) {
		var tok string
		if err := e.c.Store.W().Tx(ctx, func(tx *store.Tx) error {
			var err error
			tok, err = e.c.Auth.CreateSessionTx(ctx, tx, vic.ID, auth.MethodOIDC, auth.SessionMeta{Provider: "porch"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		ch := openSSE(t, http.DefaultClient, termURL, "Cookie", api.SessionCookieName(false)+"="+tok)
		if err := e.c.Auth.RevokeAllSessions(ctx, vic.ID, "test"); err != nil {
			t.Fatal(err)
		}
		ends(t, ch, api.EndUnauthenticated)
	})

	t.Run("root token rotated", func(t *testing.T) {
		secret, _, err := e.c.Auth.IssueRootToken(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ch := openSSE(t, http.DefaultClient, termURL, "Authorization", "Bearer "+secret)
		if _, _, err := e.c.Auth.RotateRootToken(ctx, sys); err != nil {
			t.Fatal(err)
		}
		ends(t, ch, api.EndUnauthenticated)
	})

	// Revoking the people watching did not touch the run: revocation never undoes or stops completed actions.
	if o := e.run1(r.ID).Observed; o != "running" {
		t.Fatalf("run %s after viewer revocations", o)
	}
	// A run's credential is revoked when the run is stopped; the next tool call with it fails.
	runTok := e.runToken(r.ID)
	if res := e.bridgeCall(r.ID, runTok, proto.ToolCatalog, nil); !res.OK {
		t.Fatalf("catalog before stop: %+v", res)
	}
	if _, err := e.c.Fleet.StopRun(ctx, admin, r.ID, "kill", "test"); err != nil {
		t.Fatal(err)
	}
	if res := e.bridgeCall(r.ID, runTok, proto.ToolCatalog, nil); res.OK {
		t.Fatalf("tool call with a revoked run credential: %+v", res)
	}
}

// reenroll issues a new enrollment for a node and exchanges it for a new credential (the agent must be stopped).
func (e *fleetEnv) reenroll(nodeID string) {
	e.t.Helper()
	en, err := e.c.Fleet.ReissueEnrollment(context.Background(), admin, nodeID)
	if err != nil {
		e.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": en.Token})
	res, err := http.Post(e.srv.URL+fleet.EnrollPath, "application/json", bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var er fleet.EnrollResult
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&er) != nil {
		e.t.Fatalf("re-enroll: %d", res.StatusCode)
	}
	e.cred = er.Credential
}

// SM-AUTH-007, SM-NODE-001: a node connection whose credential expired (or was revoked outside central) is closed by
// the periodic re-check, and the node cannot reconnect with it; disabling the node closes it at once.
func TestNodeConnectionEndsWithItsCredential(t *testing.T) {
	// Not parallel: central restarts must finish within the node lease and the fleet tests' wall-clock bounds (such as
	// the 5 s restart check), which parallel tests can starve.
	e := newFleetEnv(t)
	e.cfg.Auth.StreamRevalidate = config.Duration(time.Second)
	e.restartCentral()
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	id := nodes[0].ID
	connected := func() bool { n, _ := e.c.Fleet.GetNode(ctx, id); return n.Connected }
	eventually(t, "connected", connected)
	if err := e.c.Store.W().Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE node_credentials SET expires_at = ? WHERE node_id = ?`, time.Now().Add(-time.Second).UnixMicro(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	eventually(t, "disconnected after the credential expired", func() bool { return !connected() })
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("expired credential kept its connection for %s (re-check every 1 s)", d)
	}
	time.Sleep(1500 * time.Millisecond) // the agent keeps retrying with the expired credential
	if connected() {
		t.Fatal("node reconnected with an expired credential")
	}

	// Disabling closes a live connection immediately, without waiting for the re-check.
	e.stopAgent()
	e.reenroll(id)
	e.startAgent()
	eventually(t, "reconnected after re-enrollment", connected)
	start = time.Now()
	if _, err := e.c.Fleet.SetNodeState(ctx, admin, id, "disabled"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "disconnected after disable", func() bool { return !connected() })
	if d := time.Since(start); d > 900*time.Millisecond {
		t.Fatalf("disable took %s to close the connection", d)
	}
}
