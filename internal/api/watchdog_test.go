package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
