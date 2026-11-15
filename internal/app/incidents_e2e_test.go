package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

// fleetCaller calls the API of a fleet environment with a bearer token.
type fleetCaller struct {
	t   *testing.T
	url string
}

func (f fleetCaller) do(tok, method, path string, body any, hdr ...string) (int, http.Header, []byte) {
	f.t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, f.url+api.Prefix+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, b
}

func (f fleetCaller) want(tok, method, path string, body any, status int, out any, hdr ...string) http.Header {
	f.t.Helper()
	code, h, b := f.do(tok, method, path, body, hdr...)
	if code != status {
		f.t.Fatalf("%s %s: status %d, want %d: %s", method, path, code, status, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			f.t.Fatalf("decode %s: %v", b, err)
		}
	}
	return h
}

// userToken creates a user with grants and returns an API token with the same grants.
func userToken(t *testing.T, a *auth.Service, email string, grants []auth.GrantInput) string {
	t.Helper()
	ctx := context.Background()
	u, err := a.Create(ctx, audit.System, auth.CreateInput{Email: email, DisplayName: email, Grants: grants})
	if err != nil {
		t.Fatal(err)
	}
	u, _ = a.Get(ctx, u.ID)
	_, secret, err := a.CreateAPIToken(ctx, &auth.Principal{User: u}, "test", grants, 24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

// SM-QUEUE-009 end to end over HTTP against a real central and node agent (in-memory runner): the policy's incident
// fields round-trip; an incident from a trusted operator preempts running work and pauses its worker on the node
// (the interrupt command reaches the runner); an untrusted source's incident is only a proposal that a viewer cannot
// confirm and an operator can (If-Match); when the incident ends, automatic resumption runs the preempted task again,
// and control is told each step through its durable notifications.
func TestIncidentPolicyEndToEnd(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "incidents", Name: "Incidents", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	api := fleetCaller{t: t, url: e.srv.URL}
	opTok := userToken(t, e.c.Auth, "oscar@example.com", []auth.GrantInput{{Role: authz.RoleOperator, Domain: sp.ID}})
	viewTok := userToken(t, e.c.Auth, "vera@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: sp.ID}})
	base := "/spaces/" + sp.ID

	// The policy: preempt, pause workers, resume automatically; triggers omitted → operator and control.
	var pol queue.Policy
	api.want(opTok, "PUT", base+"/queue/policy", map[string]any{"max_active": 1, "incident_insertion": "top", "incident_preempts": true,
		"escalation_reorders": false, "incident_workers": "pause", "incident_resume": "auto", "version": 0}, 200, &pol)
	if strings.Join(pol.IncidentTriggers, ",") != "control,operator" || pol.IncidentWorkers != "pause" || pol.IncidentResume != "auto" || pol.IncidentUntrusted != "propose" {
		t.Fatalf("policy %+v", pol)
	}
	api.want(opTok, "PUT", base+"/queue/policy", map[string]any{"max_active": 1, "incident_insertion": "top", "incident_preempts": false, "escalation_reorders": false, "incident_triggers": []string{"worker"}, "version": 0}, 422, nil)
	api.want(viewTok, "PUT", base+"/queue/policy", map[string]any{"max_active": 1, "incident_insertion": "top", "incident_preempts": false, "escalation_reorders": false, "version": 0}, 403, nil)

	// Running work with a live worker.
	wrk, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker running", func() bool { return e.run1(wrk.ID).Observed == "running" })
	work, _, _ := e.c.Queue.Create(ctx, admin, sp.ID, queue.CreateInput{Title: "Refactor refunds", Status: queue.Ready})
	work, _ = e.c.Queue.SetStatus(ctx, admin, sp.ID, work.ID, work.Version, queue.StatusInput{Status: queue.Running})
	work, _ = e.c.Queue.Update(ctx, admin, sp.ID, work.ID, work.Version, queue.UpdateInput{Title: work.Title, Priority: work.Priority, AssigneeRun: wrk.ID})

	// An operator raises an incident: the work is preempted and its worker paused on the node.
	var inc queue.Task
	api.want(opTok, "POST", base+"/tasks", map[string]any{"title": "Payments down", "kind": "incident", "status": "ready"}, 201, &inc)
	if inc.Kind != "incident" || inc.IncidentTrigger != "operator" || inc.Position != 1 {
		t.Fatalf("incident %+v", inc)
	}
	var w queue.Task
	api.want(viewTok, "GET", base+"/tasks/"+work.ID, nil, 200, &w)
	if w.Status != "ready" || w.PreemptedBy != inc.ID || len(w.PreemptionLog) != 2 {
		t.Fatalf("preempted %+v", w)
	}
	// The in-memory runner exits on an interrupt (a real harness stops its turn); either way the node got the command.
	eventually(t, "worker interrupted", func() bool { r := e.run1(wrk.ID); return r.ExitCode != nil && *r.ExitCode == 130 })
	eventually(t, "control told", func() bool { return toControlMsg(e, sp.ID, "Incident preempted work: Payments down") })

	// An untrusted source's incident is a proposal; viewers cannot decide, operators can.
	prop, _, err := e.c.Queue.Create(ctx, admin, sp.ID, queue.CreateInput{Title: "Webhook says outage", Kind: "incident", Status: queue.Ready,
		Triggers: []string{"subscription:sub_00000000000000000000000000"}})
	if err != nil || prop.Kind != "ordinary" || prop.IncidentProposedBy == "" {
		t.Fatalf("proposal %+v %v", prop, err)
	}
	api.want(viewTok, "POST", base+"/tasks/"+prop.ID+"/incident", map[string]any{"decision": "confirm"}, 403, nil, "If-Match", `"1"`)
	api.want(opTok, "POST", base+"/tasks/"+prop.ID+"/incident", map[string]any{"decision": "confirm"}, 428, nil)
	var dec queue.Task
	api.want(opTok, "POST", base+"/tasks/"+prop.ID+"/incident", map[string]any{"decision": "dismiss"}, 200, &dec, "If-Match", `"1"`)
	if dec.Kind != "ordinary" || dec.IncidentProposedBy != "" {
		t.Fatalf("dismissed %+v", dec)
	}

	// The incident runs and ends; the preempted task has no live worker left (the fake exited it), so it waits for
	// control to launch a new one. Resume refuses a task that is no longer preempted.
	inc2 := inc
	api.want(opTok, "POST", base+"/tasks/"+inc.ID+"/status", map[string]any{"status": "running"}, 200, &inc2, "If-Match", `"1"`)
	api.want(opTok, "POST", base+"/tasks/"+inc.ID+"/status", map[string]any{"status": "done"}, 200, nil, "If-Match", jsonVersion(inc2.Version))
	var ended queue.Task // a fresh value: an empty preempted_by is omitted from the JSON
	api.want(viewTok, "GET", base+"/tasks/"+work.ID, nil, 200, &ended)
	if ended.Status != "ready" || ended.PreemptedBy != "" || !strings.HasPrefix(ended.PreemptionLog[0].Outcome, "relaunch") {
		t.Fatalf("after the incident %+v", ended)
	}
	eventually(t, "control told of the end", func() bool { return toControlMsg(e, sp.ID, "Incident ended: Payments down") })
	api.want(opTok, "POST", base+"/tasks/"+work.ID+"/resume", nil, 409, nil)
	api.want(viewTok, "POST", base+"/tasks/"+work.ID+"/resume", nil, 403, nil)
}

func jsonVersion(v int64) string { return `"` + strconv.FormatInt(v, 10) + `"` }

func toControlMsg(e *fleetEnv, spaceID, subject string) bool {
	l, _ := e.c.Messages.List(context.Background(), spaceID, 100)
	for _, m := range l {
		if m.ToKind == "control" && m.FromKind == "system" && strings.Contains(m.Subject, subject) {
			return true
		}
	}
	return false
}

// SM-CTRL-004: an operator stops control and launches it again over the API (the supervisor does not restart an
// operator's stop, and a second control is refused while one runs); diagnostic launches need run:diagnose, which
// operators and admins hold and viewers do not, and the launch is audited with its kind — also when the kind comes
// from the space role rather than the request.
func TestOperatorRestartsControlAndDiagnostics(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "restart", Name: "Restart", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	api := fleetCaller{t: t, url: e.srv.URL}
	opTok := userToken(t, e.c.Auth, "oscar@example.com", []auth.GrantInput{{Role: authz.RoleOperator, Domain: sp.ID}})
	viewTok := userToken(t, e.c.Auth, "vera@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: sp.ID}})
	base := "/spaces/" + sp.ID
	control := map[string]any{"kind": "control", "role": "control", "harness": hns.KindClaude}

	var first fleet.Run
	api.want(opTok, "POST", base+"/runs", control, 201, &first)
	eventually(t, "control running", func() bool { return e.run1(first.ID).Observed == "running" })
	api.want(opTok, "POST", base+"/runs", control, 409, nil) // one authoritative control
	api.want(viewTok, "POST", base+"/runs/"+first.ID+"/stop", map[string]any{"mode": "interrupt"}, 403, nil)
	api.want(opTok, "POST", base+"/runs/"+first.ID+"/stop", map[string]any{"mode": "interrupt", "reason": "restart control"}, 200, nil)
	eventually(t, "control stopped", func() bool { o := e.run1(first.ID).Observed; return o == "exited" || o == "failed" })
	var again fleet.Run
	api.want(opTok, "POST", base+"/runs", control, 201, &again)
	eventually(t, "control relaunched", func() bool { return e.run1(again.ID).Observed == "running" })
	time.Sleep(300 * time.Millisecond)
	runs, _ := e.c.Fleet.ListRuns(ctx, sp.ID, 50)
	n := 0
	for _, r := range runs {
		if r.Kind == "control" && r.Desired == "running" {
			n++
			if r.ID != again.ID || !strings.HasPrefix(r.CreatedBy, "user:") {
				t.Fatalf("unexpected control run %+v", r)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d wanted control runs after the restart, want 1", n)
	}

	// Diagnostics: viewers are refused, operators may launch them (run:diagnose), and the audit names the kind.
	diag := map[string]any{"kind": "diagnostic", "role": "doctor", "harness": hns.KindClaude}
	api.want(viewTok, "POST", base+"/runs", diag, 403, nil)
	var d fleet.Run
	api.want(opTok, "POST", base+"/runs", diag, 201, &d)
	if d.Kind != "diagnostic" {
		t.Fatalf("diagnostic %+v", d)
	}
	var kinds []string
	l, _ := e.c.Fleet.ListRuns(ctx, sp.ID, 50)
	for _, r := range l {
		kinds = append(kinds, r.Kind)
	}
	if !strings.Contains(strings.Join(kinds, ","), "diagnostic") {
		t.Fatalf("no diagnostic run: %v", kinds)
	}
	// The check sees the kind the launch resolves to: a refusal there stops the launch before anything is written.
	before, _ := e.c.Fleet.ListRuns(ctx, sp.ID, 100)
	seen := ""
	_, err = e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "diagnostic", Role: "doctor", Harness: hns.KindClaude,
		AuthorizeKind: func(kind string) error { seen = kind; return errDenied }})
	after, _ := e.c.Fleet.ListRuns(ctx, sp.ID, 100)
	if !errors.Is(err, errDenied) || seen != "diagnostic" || len(after) != len(before) {
		t.Fatalf("refused launch: %v seen=%q runs %d→%d", err, seen, len(before), len(after))
	}
	eventually(t, "diagnostic running", func() bool { return e.run1(d.ID).Observed == "running" })
}

var errDenied = authzDenied{}

type authzDenied struct{}

func (authzDenied) Error() string { return "denied" }
