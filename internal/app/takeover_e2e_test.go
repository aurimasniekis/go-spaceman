package app_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// SM-UI-004, SM-AUTH-004: terminal takeover over HTTP against a real central and node agent (in-memory runner; the
// interactive attach itself is verified against herdr-core by internal/runner/hc TestRealTakeoverTypesAndDetaches).
// Only terminal:takeover may take over; only the holder types and renews; every automated input is refused while it
// lasts (Message the agent centrally, terminal notices for messages, and run.input already queued on the node), and
// held notices go out after the release; start and end are audited without the keystrokes; it ends on release,
// expiry and run end, and the node detaches each time.
func TestTerminalTakeover(t *testing.T) {
	prev := fleet.TakeoverTTL
	fleet.TakeoverTTL = 2 * time.Second
	t.Cleanup(func() { fleet.TakeoverTTL = prev })
	e := newFleetEnv(t)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "takeover", Name: "Takeover", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "running", func() bool { return e.run1(r.ID).Observed == "running" })
	term := e.run1(r.ID).TerminalID
	e.run.SetStatus(term, "idle")

	sys := audit.System
	mkToken := func(email, name string, grants []auth.GrantInput) (string, string) {
		t.Helper()
		u, err := e.c.Auth.Create(ctx, sys, auth.CreateInput{Email: email, DisplayName: name, Grants: grants})
		if err != nil {
			t.Fatal(err)
		}
		u, _ = e.c.Auth.Get(ctx, u.ID)
		_, secret, err := e.c.Auth.CreateAPIToken(ctx, &auth.Principal{User: u}, "takeover", grants, 24*time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		return secret, u.ID
	}
	adminTok, adminID := mkToken("ada@example.com", "Ada", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: sp.ID}})
	otherTok, _ := mkToken("bob@example.com", "Bob", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: sp.ID}})
	opTok, _ := mkToken("oscar@example.com", "Oscar", []auth.GrantInput{{Role: authz.RoleOperator, Domain: sp.ID}})
	base := e.srv.URL + api.Prefix + "/spaces/" + sp.ID + "/runs/" + r.ID
	call := func(tok, path string, body any) (int, []byte) {
		t.Helper()
		var rd io.Reader = http.NoBody
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(http.MethodPost, base+path, rd)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	audits := func(action string) []db.AuditEvent {
		evs, err := e.c.Store.R().AuditEventsFrom(ctx, db.AuditEventsFromParams{Seq: 0, Limit: 100000})
		if err != nil {
			t.Fatal(err)
		}
		var out []db.AuditEvent
		for _, ev := range evs {
			if ev.Action == action && ev.TargetID.String == r.ID {
				out = append(out, ev)
			}
		}
		return out
	}

	// Operators watch and message but do not take over (terminal:takeover is an admin permission by default).
	if code, b := call(opTok, "/takeover", nil); code != http.StatusForbidden {
		t.Fatalf("operator takeover: %d %s", code, b)
	}
	code, b := call(adminTok, "/takeover", nil)
	if code != http.StatusCreated {
		t.Fatalf("start: %d %s", code, b)
	}
	var tk fleet.Takeover
	_ = json.Unmarshal(b, &tk)
	if tk.Holder != "user:"+adminID || tk.HolderLabel != "Ada" || !strings.HasPrefix(tk.ID, "tko_") {
		t.Fatalf("takeover %+v", tk)
	}
	if got := e.run1(r.ID).Takeover; got == nil || got.ID != tk.ID {
		t.Fatalf("run shows takeover %+v", got)
	}
	eventually(t, "node attached interactively", func() bool { return e.run.TakeoverAttached(term) == 1 })
	if code, _ := call(otherTok, "/takeover", nil); code != http.StatusConflict {
		t.Fatalf("second takeover: %d", code)
	}

	// Only the holder types; the bytes reach the program as they are.
	keys := "ls -la $HOME\r\x1b[A"
	if code, b := call(otherTok, "/takeover/"+tk.ID+"/input", map[string]string{"data": "x"}); code != http.StatusForbidden {
		t.Fatalf("non-holder typed: %d %s", code, b)
	}
	if code, b := call(opTok, "/takeover/"+tk.ID+"/input", map[string]string{"data": "x"}); code != http.StatusForbidden {
		t.Fatalf("operator typed: %d %s", code, b)
	}
	if code, b := call(adminTok, "/takeover/"+tk.ID+"/input", map[string]string{"data": keys}); code != http.StatusAccepted {
		t.Fatalf("type: %d %s", code, b)
	}
	eventually(t, "keystrokes typed", func() bool { return e.run.Typed(term) == keys })

	// Automated input is refused: Message the agent (any operator) and terminal notices for messages.
	if code, b := call(opTok, "/input", map[string]any{"text": "hello"}); code != http.StatusConflict {
		t.Fatalf("send-input during takeover: %d %s", code, b)
	}
	if _, _, err := e.c.Messages.Send(ctx, admin, sp.ID, messages.From{Kind: "operator", ID: "usr_test", Label: "Test"},
		messages.SendInput{To: "run:" + r.ID, Kind: "message", Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Notifier.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	// A run.input already queued for the node is refused there.
	var cmdID string
	if err := e.c.Store.W().Tx(ctx, func(tx *store.Tx) error {
		cmdID = ids.New(ids.Command)
		pl, _ := json.Marshal(proto.InputPayload{Text: "queued before", Force: true})
		return tx.Q.InsertNodeCommand(ctx, db.InsertNodeCommandParams{ID: cmdID, NodeID: nodes[0].ID, SpaceID: sp.ID,
			RunID: sql.NullString{String: r.ID, Valid: true}, Generation: e.run1(r.ID).Generation, Kind: proto.CmdInput,
			IdempotencyKey: "input:" + cmdID, PayloadJson: string(pl), CreatedAt: tx.NowMicros()})
	}); err != nil {
		t.Fatal(err)
	}
	e.c.Fleet.KickRun(ctx, r.ID)
	eventually(t, "queued input refused on the node", func() bool {
		c, err := e.c.Store.R().GetNodeCommand(ctx, cmdID)
		return err == nil && c.State == proto.AckRejected && strings.Contains(c.Reason, "taken over")
	})
	time.Sleep(200 * time.Millisecond)
	if in := e.run.Inputs(term); len(in) != 0 {
		t.Fatalf("automated input during takeover: %q", in)
	}

	// Renewal keeps it; the holder releases it; the node detaches and the held notice goes out.
	if code, b := call(otherTok, "/takeover/"+tk.ID+"/renew", nil); code != http.StatusForbidden {
		t.Fatalf("non-holder renewed: %d %s", code, b)
	}
	if code, b := call(adminTok, "/takeover/"+tk.ID+"/renew", nil); code != http.StatusOK {
		t.Fatalf("renew: %d %s", code, b)
	}
	if code, b := call(adminTok, "/takeover/"+tk.ID+"/end", nil); code != http.StatusOK || !strings.Contains(string(b), `"end_reason":"released"`) {
		t.Fatalf("release: %d %s", code, b)
	}
	eventually(t, "node detached", func() bool { return e.run.TakeoverAttached(term) == 0 })
	if e.run1(r.ID).Takeover != nil {
		t.Fatal("run still shows the takeover")
	}
	if err := e.c.Notifier.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "held notice delivered after release", func() bool {
		in := e.run.Inputs(term)
		return len(in) == 1 && strings.Contains(in[0], "1 new message")
	})
	if code, _ := call(adminTok, "/takeover/"+tk.ID+"/input", map[string]string{"data": "x"}); code != http.StatusConflict {
		t.Fatalf("typed after release: %d", code)
	}
	started, endedEv := audits("run.takeover_started"), audits("run.takeover_ended")
	if len(started) != 1 || len(endedEv) != 1 || started[0].ActorID.String != adminID {
		t.Fatalf("audit started=%d ended=%d", len(started), len(endedEv))
	}
	if d := endedEv[0].DetailsJson; !strings.Contains(d, `"reason":"released"`) || !strings.Contains(d, `"bytes_typed":`) || strings.Contains(d, "HOME") {
		t.Fatalf("end audit %s", d)
	}
	for _, ev := range append(started, endedEv...) {
		if strings.Contains(ev.DetailsJson, "ls -la") {
			t.Fatal("keystrokes in the audit log")
		}
	}

	// Without renewals it expires (a closed page, a revoked session or token): automated input resumes.
	code, b = call(adminTok, "/takeover", nil)
	if code != http.StatusCreated {
		t.Fatalf("start again: %d %s", code, b)
	}
	_ = json.Unmarshal(b, &tk)
	eventually(t, "attached again", func() bool { return e.run.TakeoverAttached(term) == 1 })
	time.Sleep(fleet.TakeoverTTL + 100*time.Millisecond)
	if code, _ := call(opTok, "/input", map[string]any{"text": "after expiry"}); code != http.StatusAccepted {
		t.Fatalf("input after expiry: %d", code)
	}
	if n, err := e.c.Fleet.ExpireTakeovers(ctx); err != nil || n != 1 {
		t.Fatalf("expire %d %v", n, err)
	}
	eventually(t, "detached after expiry", func() bool { return e.run.TakeoverAttached(term) == 0 })
	if ev := audits("run.takeover_ended"); len(ev) != 2 || !strings.Contains(ev[1].DetailsJson, `"reason":"expired"`) {
		t.Fatalf("expiry audit %+v", ev)
	}

	// Another admin may end someone's takeover; and a run's end ends it.
	code, b = call(adminTok, "/takeover", nil)
	if code != http.StatusCreated {
		t.Fatalf("start a third time: %d %s", code, b)
	}
	_ = json.Unmarshal(b, &tk)
	if code, b := call(otherTok, "/takeover/"+tk.ID+"/end", nil); code != http.StatusOK || !strings.Contains(string(b), `"end_reason":"ended"`) {
		t.Fatalf("ended by another admin: %d %s", code, b)
	}
	code, b = call(adminTok, "/takeover", nil)
	if code != http.StatusCreated {
		t.Fatalf("start a fourth time: %d %s", code, b)
	}
	_ = json.Unmarshal(b, &tk)
	eventually(t, "attached before stop", func() bool { return e.run.TakeoverAttached(term) == 1 })
	if _, err := e.c.Fleet.StopRun(ctx, admin, r.ID, "kill", "test"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "run ended", func() bool { o := e.run1(r.ID).Observed; return o == "exited" || o == "failed" })
	ev := audits("run.takeover_ended")
	if len(ev) != 4 || !strings.Contains(ev[3].DetailsJson, `"reason":"run_ended"`) {
		t.Fatalf("run-end audit %d", len(ev))
	}
	if code, _ := call(adminTok, "/takeover", nil); code != http.StatusConflict {
		t.Fatalf("takeover of an ended run: %d", code)
	}
}
