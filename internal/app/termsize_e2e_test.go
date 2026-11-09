package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// ADR-0075: resizing a run's terminal over HTTP against a real central and node agent (in-memory runner; the held
// interactive attach and its resize are verified against herdr-core by internal/runner/hc TestRealTerminalSizeHold).
// Only terminal:takeover in the run's space resizes (an operator, a viewer and an admin of another space cannot), the
// size is bounded, audited, shown on the run and held by the node through one attach that types nothing; while the
// terminal is taken over only the holder resizes; clearing it and the run's end close the attach; and the space's
// terminal_cols and terminal_rows limits size new runs from their launch.
func TestTerminalSize(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	mkSpace := func(slug string) *fleet.Space {
		sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: slug, Name: slug, NodeID: nodes[0].ID})
		if err != nil {
			t.Fatal(err)
		}
		bindStubAccounts(t, e.c, sp.ID)
		eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
		return sp
	}
	sp, other := mkSpace("termsize"), mkSpace("termsize-other")
	launch := func(spaceID string) (*fleet.Run, string) {
		t.Helper()
		r, err := e.c.Fleet.LaunchRun(ctx, admin, spaceID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "running", func() bool { return e.run1(r.ID).Observed == "running" })
		return r, e.run1(r.ID).TerminalID
	}
	r, term := launch(sp.ID)
	if got := e.run1(r.ID); got.TerminalSize != nil || !got.TerminalSizing {
		t.Fatalf("new run: size %+v sizing %v", got.TerminalSize, got.TerminalSizing)
	}

	mkToken := func(email, name string, grants []auth.GrantInput) (string, string) {
		t.Helper()
		u, err := e.c.Auth.Create(ctx, audit.System, auth.CreateInput{Email: email, DisplayName: name, Grants: grants})
		if err != nil {
			t.Fatal(err)
		}
		u, _ = e.c.Auth.Get(ctx, u.ID)
		_, secret, err := e.c.Auth.CreateAPIToken(ctx, &auth.Principal{User: u}, "size", grants, 24*time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		return secret, u.ID
	}
	adminTok, adminID := mkToken("ada@example.com", "Ada", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: sp.ID}})
	otherTok, _ := mkToken("bob@example.com", "Bob", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: sp.ID}})
	opTok, _ := mkToken("oscar@example.com", "Oscar", []auth.GrantInput{{Role: authz.RoleOperator, Domain: sp.ID}})
	viewTok, _ := mkToken("vera@example.com", "Vera", []auth.GrantInput{{Role: authz.RoleViewer, Domain: sp.ID}})
	elsewhereTok, _ := mkToken("eve@example.com", "Eve", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: other.ID}})
	call := func(tok, method, spaceID, path string, body any, hdr ...string) (int, []byte) {
		t.Helper()
		var rd io.Reader = http.NoBody
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		if !strings.HasPrefix(path, "/limits") {
			path = "/runs/" + r.ID + path
		}
		req, _ := http.NewRequest(method, e.srv.URL+api.Prefix+"/spaces/"+spaceID+path, rd)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	size := func(c, rows int) map[string]int { return map[string]int{"cols": c, "rows": rows} }
	held := func(c, rows int) func() bool {
		return func() bool {
			gc, gr, _ := e.run.Size(term)
			return gc == c && gr == rows && e.run.TakeoverAttached(term) == 1
		}
	}

	// Who may resize: terminal:takeover in the run's space only.
	for name, tok := range map[string]string{"operator": opTok, "viewer": viewTok} {
		if code, b := call(tok, http.MethodPut, sp.ID, "/terminal-size", size(120, 40)); code != http.StatusForbidden {
			t.Fatalf("%s resized: %d %s", name, code, b)
		}
		if code, b := call(tok, http.MethodDelete, sp.ID, "/terminal-size", nil); code != http.StatusForbidden {
			t.Fatalf("%s cleared: %d %s", name, code, b)
		}
	}
	// An admin of another space does not see this space at all (404, like every space outside one's grants).
	if code, b := call(elsewhereTok, http.MethodPut, sp.ID, "/terminal-size", size(120, 40)); code != http.StatusNotFound {
		t.Fatalf("an admin of another space resized: %d %s", code, b)
	}
	if code, b := call(elsewhereTok, http.MethodPut, other.ID, "/terminal-size", size(120, 40)); code != http.StatusNotFound {
		t.Fatalf("a run addressed through another space: %d %s", code, b)
	}
	// Bounds: 40-400 columns, 10-200 rows.
	for _, bad := range []map[string]int{size(39, 24), size(401, 24), size(80, 9), size(80, 201)} {
		if code, b := call(adminTok, http.MethodPut, sp.ID, "/terminal-size", bad); code != http.StatusUnprocessableEntity {
			t.Fatalf("size %v: %d %s", bad, code, b)
		}
	}
	if e.run.TakeoverAttached(term) != 0 {
		t.Fatal("a refused resize attached")
	}

	// An admin resizes: shown on the run, held by one attach, never typed into.
	code, b := call(adminTok, http.MethodPut, sp.ID, "/terminal-size", size(120, 40))
	if code != http.StatusOK || !strings.Contains(string(b), `"terminal_size":{"cols":120,"rows":40}`) {
		t.Fatalf("resize: %d %s", code, b)
	}
	eventually(t, "node holds 120x40", held(120, 40))
	code, b = call(adminTok, http.MethodPut, sp.ID, "/terminal-size", size(80, 24))
	if code != http.StatusOK {
		t.Fatalf("resize again: %d %s", code, b)
	}
	eventually(t, "node holds 80x24", held(80, 24))

	// The recording notes the sizes, so a replay redraws at them.
	eventually(t, "size records shipped", func() bool {
		res, err := e.c.Fleet.Recordings().Read(ctx, r.ID, 0)
		if err != nil {
			return false
		}
		var got []string
		for _, rec := range res.Records {
			if rec.Kind == "size" {
				got = append(got, fmt.Sprintf("%dx%d", rec.Cols, rec.Rows))
			}
		}
		return strings.Join(got, ",") == "120x40,80x24"
	})

	// While the terminal is taken over only the holder resizes.
	code, b = call(adminTok, http.MethodPost, sp.ID, "/takeover", nil)
	if code != http.StatusCreated {
		t.Fatalf("takeover: %d %s", code, b)
	}
	var tk fleet.Takeover
	_ = json.Unmarshal(b, &tk)
	eventually(t, "takeover attached", func() bool { return e.run.TakeoverAttached(term) == 2 })
	if code, b := call(otherTok, http.MethodPut, sp.ID, "/terminal-size", size(100, 30)); code != http.StatusForbidden || !strings.Contains(string(b), "Ada") {
		t.Fatalf("non-holder resized during a takeover: %d %s", code, b)
	}
	if code, b := call(adminTok, http.MethodPut, sp.ID, "/terminal-size", size(100, 30)); code != http.StatusOK {
		t.Fatalf("holder resize: %d %s", code, b)
	}
	eventually(t, "holder's size applied", func() bool { c, rows, _ := e.run.Size(term); return c == 100 && rows == 30 })
	if code, b := call(adminTok, http.MethodPost, sp.ID, "/takeover/"+tk.ID+"/end", nil); code != http.StatusOK {
		t.Fatalf("release: %d %s", code, b)
	}
	eventually(t, "takeover detached, hold stays", held(100, 30))

	// Clearing closes the hold: the terminal goes back to its start size.
	if code, b := call(otherTok, http.MethodDelete, sp.ID, "/terminal-size", nil); code != http.StatusOK || strings.Contains(string(b), `"terminal_size"`) {
		t.Fatalf("clear: %d %s", code, b)
	}
	eventually(t, "hold closed", func() bool {
		c, rows, _ := e.run.Size(term)
		return c == 200 && rows == 120 && e.run.TakeoverAttached(term) == 0
	})
	if typed := e.run.Typed(term); typed != "" {
		t.Fatalf("%d bytes typed into the terminal", len(typed))
	}

	// Audited: who, which size, the previous one; a repeat of the same size is not a change.
	if code, _ := call(otherTok, http.MethodDelete, sp.ID, "/terminal-size", nil); code != http.StatusOK {
		t.Fatalf("clear again: %d", code)
	}
	evs, err := e.c.Store.R().AuditEventsFrom(ctx, db.AuditEventsFromParams{Seq: 0, Limit: 100000})
	if err != nil {
		t.Fatal(err)
	}
	var resized []db.AuditEvent
	for _, ev := range evs {
		if ev.Action == "run.terminal_resized" && ev.TargetID.String == r.ID {
			resized = append(resized, ev)
		}
	}
	if len(resized) != 4 || resized[0].ActorID.String != adminID || !strings.Contains(resized[0].DetailsJson, `"cols":120`) ||
		!strings.Contains(resized[1].DetailsJson, `"previous":{"cols":120,"rows":40}`) || !strings.Contains(resized[3].DetailsJson, `"cleared":true`) {
		for _, ev := range resized {
			t.Log(ev.DetailsJson)
		}
		t.Fatalf("%d resize audit events", len(resized))
	}

	// The space's limits size new runs from their launch; the run's end closes the hold.
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.TerminalCols: 39, limits.TerminalRows: 30}}); err == nil {
		t.Fatal("a 39-column default was accepted")
	}
	// Changing them needs terminal:takeover, like resizing a live run: an operator holds space_config:write but not
	// takeover, so the operator is refused and the admin succeeds.
	sized := map[string]any{"values": map[string]int64{limits.TerminalCols: 132, limits.TerminalRows: 43}}
	if code, b := call(opTok, http.MethodPut, sp.ID, "/limits", sized); code != http.StatusForbidden || !strings.Contains(string(b), "terminal:takeover") {
		t.Fatalf("an operator set the new-run size: %d %s", code, b)
	}
	if code, b := call(adminTok, http.MethodPut, sp.ID, "/limits", sized); code != http.StatusOK {
		t.Fatalf("an admin set the new-run size: %d %s", code, b)
	}
	// Other limits stay operator-writable as long as the size is left as it is.
	sized["values"].(map[string]int64)[limits.ControlIdleStop] = 45
	if code, b := call(opTok, http.MethodPut, sp.ID, "/limits", sized, "If-Match", `"1"`); code != http.StatusOK {
		t.Fatalf("an operator changed another limit: %d %s", code, b)
	}
	r2, term2 := launch(sp.ID)
	if got := e.run1(r2.ID).TerminalSize; got == nil || *got != (fleet.TerminalSize{Cols: 132, Rows: 43}) {
		t.Fatalf("launched with size %+v", got)
	}
	eventually(t, "new run held at the space default", func() bool {
		c, rows, _ := e.run.Size(term2)
		return c == 132 && rows == 43 && e.run.TakeoverAttached(term2) == 1
	})
	if _, err := e.c.Fleet.StopRun(ctx, admin, r2.ID, "kill", "test"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "ended run's hold closed", func() bool { return e.run.TakeoverAttached(term2) == 0 })
	r = r2
	if code, b := call(adminTok, http.MethodPut, sp.ID, "/terminal-size", size(120, 40)); code != http.StatusConflict || !strings.Contains(string(b), "precondition_failed") {
		t.Fatalf("resize of an ended run: %d %s", code, b)
	}
}

// ADR-0075: a node agent that predates node protocol 1.10 cannot hold a size, so resizing its run is refused with 409
// and the run says it cannot be resized; central never sends it terminal.sizes.
func TestTerminalSizeOldNode(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	old := dialRawNode(t, e, proto.MinorTerminalSize-1)
	old.next(t, proto.TypeWelcome)
	eventually(t, "old node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "oldnode", Name: "Old node", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	// The space's new-run size is not stored for a run the node cannot hold it for: the run would show a size it
	// doesn't have.
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.TerminalCols: 132, limits.TerminalRows: 43}}); err != nil {
		t.Fatal(err)
	}
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.run1(r.ID).TerminalSize; got != nil {
		t.Fatalf("launched on a minor-9 node with size %+v", got)
	}
	if e.run1(r.ID).TerminalSizing {
		t.Fatal("a minor-9 node's run claims it can be resized")
	}
	rootSecret, _, err := e.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]int{"cols": 120, "rows": 40})
	req, _ := http.NewRequest(http.MethodPut, e.srv.URL+api.Prefix+"/spaces/"+sp.ID+"/runs/"+r.ID+"/terminal-size", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rootSecret)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusConflict || !strings.Contains(string(b), "1.10") {
		t.Fatalf("resize on an old node: %d %s", res.StatusCode, b)
	}
	if got := e.run1(r.ID).TerminalSize; got != nil {
		t.Fatalf("size stored: %+v", got)
	}
	for {
		select {
		case env, ok := <-old.ch:
			if !ok {
				return
			}
			if env.Type == proto.TypeTerminalSizes {
				t.Fatal("terminal.sizes sent to a minor-9 node")
			}
		case <-time.After(300 * time.Millisecond):
			return
		}
	}
}
