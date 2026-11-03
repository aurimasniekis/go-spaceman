package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
)

// SM-INV-016: root authority does not bypass execution-safety invariants. With the bootstrap root token over the HTTP
// API, a second control run is refused while one is live, an active space cannot be archived or purged, editing a
// space cannot change its pinned node, and a paused space admits no launch.
func TestRootDoesNotBypassExecutionSafety(t *testing.T) {
	e := newFleetEnv(t)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "rooted", Name: "Rooted", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	root, _, err := e.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			bs, _ := json.Marshal(body)
			rd = bytes.NewReader(bs)
		}
		req, _ := http.NewRequest(method, e.srv.URL+"/api/v1/spaces/"+sp.ID+path, rd)
		req.Header.Set("Authorization", "Bearer "+root)
		req.Header.Set("Content-Type", "application/json")
		if method == http.MethodPut || (method == http.MethodPost && path == "/state") {
			s, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
			req.Header.Set("If-Match", fmt.Sprintf(`"%d"`, s.Version))
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		return res.StatusCode, out
	}

	code, out := do(http.MethodPost, "/runs", map[string]any{"kind": "control", "role": "control", "harness": hns.KindClaude, "prompt": "hello"})
	if code != http.StatusCreated {
		t.Fatalf("root launch of control: %d %s", code, out)
	}
	var ctrl fleet.Run
	_ = json.Unmarshal(out, &ctrl)
	eventually(t, "control running", func() bool { return e.run1(ctrl.ID).Observed == "running" })

	// SM-INV-006: at most one authoritative control run, also for root.
	if code, out := do(http.MethodPost, "/runs", map[string]any{"kind": "control", "role": "control", "harness": hns.KindCodex}); code != http.StatusConflict {
		t.Fatalf("second control run for root: %d %s", code, out)
	}
	runs, err := e.c.Fleet.ListRuns(ctx, sp.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	controls := 0
	for _, r := range runs {
		if r.Kind == "control" {
			controls++
		}
	}
	if controls != 1 {
		t.Fatalf("%d control runs after root asked for a second one", controls)
	}
	// A space with active runs is neither archived nor purged.
	if code, out := do(http.MethodPost, "/state", map[string]any{"state": "archived"}); code != http.StatusConflict {
		t.Fatalf("root archived a space with an active run: %d %s", code, out)
	}
	if code, out := do(http.MethodDelete, "", nil); code != http.StatusConflict {
		t.Fatalf("root purged an active space: %d %s", code, out)
	}
	// SM-INV-002/003: the pinned node changes only through relocation, never by editing the space.
	if code, out := do(http.MethodPut, "", map[string]any{"name": "Rooted", "description": "", "goals": "", "standing_instructions": "",
		"disconnect_policy": "interrupt", "node_id": "nod_elsewhere"}); code < 400 || code >= 500 {
		t.Fatalf("root edited the pinned node: %d %s", code, out)
	}
	if s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); s.NodeID != nodes[0].ID {
		t.Fatalf("pinned node changed to %s", s.NodeID)
	}
	// A paused space admits no new work, whoever asks.
	if code, out := do(http.MethodPost, "/state", map[string]any{"state": "paused", "mode": "hold"}); code != http.StatusOK {
		t.Fatalf("pause: %d %s", code, out)
	}
	if code, out := do(http.MethodPost, "/runs", map[string]any{"kind": "worker", "role": "planner", "harness": hns.KindClaude}); code != http.StatusConflict {
		t.Fatalf("root launched in a paused space: %d %s", code, out)
	}
	if e.run1(ctrl.ID).Observed != "running" {
		t.Fatal("hold stopped the running control run")
	}
}
