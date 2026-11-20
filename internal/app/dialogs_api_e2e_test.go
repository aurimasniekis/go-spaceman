package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// GET /spaces/{space}/runs/{run}/dialogs lists a run's recognised dialogs newest first, without their screen text, to
// whoever may read the space's runs and nobody else.
func TestListRunDialogsAPI(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "dlgs", Name: "Dialogs", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "dlgs2", Name: "Dialogs 2", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	api := fleetCaller{t: t, url: e.srv.URL}
	viewTok := userToken(t, e.c.Auth, "vera@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: sp.ID}})
	elseTok := userToken(t, e.c.Auth, "erin@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: other.ID}})
	var r fleet.Run
	api.want(userToken(t, e.c.Auth, "oscar@example.com", []auth.GrantInput{{Role: authz.RoleOperator, Domain: sp.ID}}), "POST", "/spaces/"+sp.ID+"/runs",
		map[string]any{"kind": "control", "role": "control", "harness": hns.KindClaude}, 201, &r)
	now := time.Now().UnixMicro()
	if err := e.c.Store.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		for i, row := range []struct{ id, outcome, excerpt string }{{"dlg_old", "blocked", "SECRET SCREEN"}, {"dlg_new", "answered", ""}} {
			if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO run_dialogs (id, run_id, space_id, node_id, dialog, signal, outcome, answer, excerpt, harness_version, detected_at, created_at)
				VALUES (?, ?, ?, ?, 'unknown', 'screen_prompt', ?, '', ?, '2.1', ?, ?)`, row.id, r.ID, sp.ID, nodes[0].ID, row.outcome, row.excerpt, now+int64(i), now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Items []fleet.RunDialog `json:"items"`
	}
	path := "/spaces/" + sp.ID + "/runs/" + r.ID + "/dialogs"
	api.want(viewTok, "GET", path, nil, 200, &got)
	if len(got.Items) != 2 || got.Items[0].ID != "dlg_new" || got.Items[1].ID != "dlg_old" || got.Items[1].Excerpt != "" {
		t.Fatalf("dialogs %+v", got.Items)
	}
	api.want(viewTok, "GET", path+"?limit=1", nil, 200, &got)
	if len(got.Items) != 1 || got.Items[0].ID != "dlg_new" {
		t.Fatalf("limit 1: %+v", got.Items)
	}
	api.want(elseTok, "GET", path, nil, 404, nil) // a space the caller cannot read is not found
	api.want(viewTok, "GET", "/spaces/"+sp.ID+"/runs/run_00000000000000000000000000/dialogs", nil, 404, nil)
}
