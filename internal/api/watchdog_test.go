package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/control"
)

// SM-CTRL-008: the watchdog state is read-only and reports a status, counts and times, never content.
func TestWatchdogView(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	if v := watchdogView(control.WatchState{}, false); v.Observed || v.Status != "off" || v.CheckedAt != nil {
		t.Fatalf("unobserved %+v", v)
	}
	for _, tc := range []struct {
		name string
		st   control.WatchState
		want string
	}{
		{"off", control.WatchState{Guard: "off", CheckedAt: now}, "off"},
		{"paused", control.WatchState{Guard: "space_not_active", CheckedAt: now}, "off"},
		{"stopping control", control.WatchState{Guard: "no_live_control", Nudges: 2, CheckedAt: now}, "idle"},
		{"idle", control.WatchState{Guard: "no_backlog", CheckedAt: now}, "idle"},
		{"busy mid-episode", control.WatchState{Guard: "busy", Nudges: 2, CheckedAt: now}, "nudging"},
		{"nudging", control.WatchState{Nudges: 1, LastNudge: now, NextNudge: now.Add(5 * time.Minute), CheckedAt: now}, "nudging"},
		{"escalated", control.WatchState{Nudges: 3, Escalated: true, CheckedAt: now}, "escalated"},
	} {
		v := watchdogView(tc.st, true)
		if !v.Observed || v.Status != tc.want || v.CheckedAt == nil || !v.CheckedAt.Equal(now) {
			t.Errorf("%s: %+v, want status %s", tc.name, v, tc.want)
		}
	}
	if v := watchdogView(control.WatchState{Nudges: 1, CheckedAt: now}, true); v.LastNudge != nil || v.NextNudge != nil || v.LastActivity != nil {
		t.Fatalf("zero times must be omitted: %+v", v)
	}
}

// The endpoint answers with the view, and with an unobserved "off" view when there is no watchdog.
func TestGetSpaceWatchdogEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	RegisterWatchdog(nil)(api, &Deps{})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/spaces/spc_01234567890123456789012345/watchdog", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got SpaceWatchdog
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Observed || got.Status != "off" {
		t.Fatalf("body %s (%v)", rec.Body, err)
	}
	if r, ok := WatchdogRules["get-space-watchdog"]; !ok || r.Perm != authz.RunRead {
		t.Fatalf("rule %+v", r)
	}
}

// Each path answers with its own space's state: two spaces held by a real watchdog, told apart by when they were
// checked, and a third it has not seen.
func TestGetSpaceWatchdogPerSpace(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(t0)
	sv := control.NewSupervisor(nil, nil, nil, nil, nil, clk)
	sv.Limit = func(context.Context, string, string) int64 { return 0 } // the watchdog is off: a pass settles at once
	w := control.NewWatchdog(sv)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	defer func() { stop(); <-done }()
	const a, b, c = "spc_0000000000000000000000000a", "spc_0000000000000000000000000b", "spc_0000000000000000000000000c"
	for i, id := range []string{a, b} {
		if i > 0 {
			clk.Advance(time.Hour)
		}
		w.Kick(id)
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			if _, ok := w.State(id); ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s was not checked", id)
			}
		}
	}
	mux := http.NewServeMux()
	RegisterWatchdog(w)(humago.New(mux, huma.DefaultConfig("t", "1")), &Deps{})
	get := func(id string) SpaceWatchdog {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/spaces/"+id+"/watchdog", nil))
		var got SpaceWatchdog
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s (%v)", id, rec.Code, rec.Body, err)
		}
		return got
	}
	ga, gb, gc := get(a), get(b), get(c)
	if !ga.Observed || ga.Guard != "off" || ga.CheckedAt == nil || !ga.CheckedAt.Equal(t0) {
		t.Errorf("a: %+v", ga)
	}
	if !gb.Observed || gb.CheckedAt == nil || !gb.CheckedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("b: %+v", gb)
	}
	if gc.Observed || gc.CheckedAt != nil || gc.Status != "off" {
		t.Errorf("c: %+v", gc)
	}
}
