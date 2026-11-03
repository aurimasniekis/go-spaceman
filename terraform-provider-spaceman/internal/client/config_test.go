package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Limits: version 0 sends no If-Match, a stored version does; values is always an object.
func TestSetSpaceLimitsIfMatch(t *testing.T) {
	f := &fakeAPI{resp: []func(http.ResponseWriter){
		jsonResp(200, map[string]any{"version": 1}), jsonResp(200, map[string]any{"version": 2})}}
	c := newTest(t, f)
	ctx := context.Background()
	if _, err := c.SetSpaceLimits(ctx, "spc_1", 0, SpaceLimitsInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetSpaceLimits(ctx, "spc_1", 1, SpaceLimitsInput{Values: map[string]int64{"max_concurrent_runs": 4}}); err != nil {
		t.Fatal(err)
	}
	if f.reqs[0].ifMatch != "" || !strings.Contains(f.reqs[0].body, `"values":{}`) || f.reqs[0].method != http.MethodPut {
		t.Fatalf("first: %+v", f.reqs[0])
	}
	if f.reqs[1].ifMatch != `"1"` || !strings.Contains(f.reqs[1].body, `"max_concurrent_runs":4`) {
		t.Fatalf("second: %+v", f.reqs[1])
	}
}

func TestOverrides(t *testing.T) {
	four := int64(4)
	l := SpaceLimits{Limits: []Limit{{Key: "max_concurrent_runs", Value: 4, Override: &four}, {Key: "support_depth", Value: 1}}}
	if o := l.Overrides(); len(o) != 1 || o["max_concurrent_runs"] != 4 {
		t.Fatalf("overrides = %v", o)
	}
}

// Schedules are created without an Idempotency-Key and are never retried (a lost response must not create two).
func TestCreateScheduleIsNotRetried(t *testing.T) {
	f := &fakeAPI{resp: []func(http.ResponseWriter){problem(503, "unavailable", "busy")}}
	c := newTest(t, f)
	if _, err := c.CreateSchedule(context.Background(), "spc_1", ScheduleInput{Name: "n", Kind: "cron", Cron: "@daily", Target: "inbox", Title: "t"}); err == nil {
		t.Fatal("want an error")
	}
	if len(f.reqs) != 1 || f.reqs[0].idem != "" || f.reqs[0].path != "/api/v1/spaces/spc_1/schedules" {
		t.Fatalf("requests = %+v", f.reqs)
	}
	if strings.Contains(f.reqs[0].body, "anchor") || strings.Contains(f.reqs[0].body, "interval_seconds") {
		t.Fatalf("unset fields were sent: %s", f.reqs[0].body)
	}
}

func TestPeerLookupAndCreate(t *testing.T) {
	f := &fakeAPI{resp: []func(http.ResponseWriter){
		jsonResp(200, map[string]any{"items": []any{map[string]any{"space_id": "spc_a", "peer": map[string]any{"id": "spc_b"}, "version": 3}}}),
		jsonResp(200, map[string]any{"items": []any{}}),
		jsonResp(200, map[string]any{"space_id": "spc_a", "version": 1}),
	}}
	c := newTest(t, f)
	ctx := context.Background()
	p, err := c.GetXSpacePeer(ctx, "spc_a", "spc_b")
	if err != nil || p.Version != 3 {
		t.Fatalf("peer = %+v, %v", p, err)
	}
	if _, err := c.GetXSpacePeer(ctx, "spc_a", "spc_c"); !IsNotFound(err) {
		t.Fatalf("missing peer: %v", err)
	}
	if _, err := c.SetXSpacePeer(ctx, "spc_a", "spc_c", 0, XSpacePeerInput{}); err != nil {
		t.Fatal(err)
	}
	if r := f.reqs[2]; r.ifMatch != "" || !strings.Contains(r.body, `"kbs":[]`) || r.path != "/api/v1/spaces/spc_a/xspace/peers/spc_c" {
		t.Fatalf("create = %+v", r)
	}
}

func TestHumanPolicyPaths(t *testing.T) {
	f := &fakeAPI{resp: []func(http.ResponseWriter){
		jsonResp(200, map[string]any{"items": []any{map[string]any{"id": "hpl_1", "action_type": "todo", "version": 2}}}),
		jsonResp(200, map[string]any{"items": []any{}}),
		jsonResp(200, map[string]any{"id": "hpl_2", "action_type": "question", "version": 1}),
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) },
	}}
	c := newTest(t, f)
	ctx := context.Background()
	if p, err := c.GetHumanPolicy(ctx, "", "todo"); err != nil || p.ID != "hpl_1" {
		t.Fatalf("default = %+v, %v", p, err)
	}
	if _, err := c.GetHumanPolicy(ctx, "spc_1", "todo"); !IsNotFound(err) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := c.SetHumanPolicy(ctx, "spc_1", "question", 0, HumanPolicyFields{MaxReminders: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteHumanPolicy(ctx, "", "todo"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/api/v1/human-policies", "/api/v1/spaces/spc_1/human-policies", "/api/v1/spaces/spc_1/human-policies/question", "/api/v1/human-policies/todo"}
	for i, w := range want {
		if f.reqs[i].path != w {
			t.Errorf("request %d path = %s, want %s", i, f.reqs[i].path, w)
		}
	}
	// Every field is sent, zero values included (PUT replaces the whole policy).
	if !strings.Contains(f.reqs[2].body, `"remind_after_minutes":0`) || !strings.Contains(f.reqs[2].body, `"wake_idle":false`) {
		t.Fatalf("body = %s", f.reqs[2].body)
	}
}

func TestUsersAndGrants(t *testing.T) {
	f := &fakeAPI{resp: []func(http.ResponseWriter){
		jsonResp(201, map[string]any{"id": "usr_1", "version": 1, "enabled": true}),
		jsonResp(200, map[string]any{"id": "usr_1", "version": 1, "grants": []any{
			map[string]any{"role": "admin", "domain": "*", "source": "manual"}}}),
		jsonResp(200, map[string]any{"id": "usr_1", "version": 2, "enabled": false}),
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) },
	}}
	c := newTest(t, f)
	ctx := context.Background()
	if _, err := c.CreateUser(ctx, CreateUserInput{Email: "a@example.com", DisplayName: "A"}); err != nil {
		t.Fatal(err)
	}
	if r := f.reqs[0]; r.idem == "" || !strings.Contains(r.body, `"grants":[]`) || strings.Contains(r.body, "password") {
		t.Fatalf("create = %+v", r)
	}
	u, err := c.SetUserGrants(ctx, "usr_1", 1, nil)
	if err != nil || len(u.ManualGrants()) != 1 {
		t.Fatalf("grants = %+v, %v", u, err)
	}
	if r := f.reqs[1]; r.ifMatch != `"1"` || r.body != `{"grants":[]}` {
		t.Fatalf("set grants = %+v", r)
	}
	if _, err := c.SetUserEnabled(ctx, "usr_1", 1, false); err != nil {
		t.Fatal(err)
	}
	if r := f.reqs[2]; r.path != "/api/v1/users/usr_1/disable" || r.ifMatch != `"1"` || r.body != "" {
		t.Fatalf("disable = %+v", r)
	}
	if err := c.UnlinkUserIdentity(ctx, "usr_1", "https://idp.example.com", "u_1"); err != nil {
		t.Fatal(err)
	}
	if r := f.reqs[3]; r.method != http.MethodDelete || r.path != "/api/v1/users/usr_1/identities" {
		t.Fatalf("unlink = %+v", r)
	}
}

func TestManualGrantsSkipSSO(t *testing.T) {
	u := User{Grants: []Grant{{Role: "viewer", Domain: "*", Source: "manual"}, {Role: "admin", Domain: "*", Source: "sso"}, {Role: "operator", Domain: "spc_1"}}}
	if g := u.ManualGrants(); len(g) != 2 || g[0].Role != "viewer" || g[1].Role != "operator" {
		t.Fatalf("manual = %v", g)
	}
}
