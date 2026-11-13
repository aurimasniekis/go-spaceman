package proto

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// A run report's usage limit survives an encode/decode round trip through the envelope, in both the open and the
// cleared form.
func TestRunLimitRoundTrip(t *testing.T) {
	since := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	reset := since.Add(5 * time.Hour)
	for name, l := range map[string]*RunLimit{
		"open":    {Kind: LimitFiveHour, ResetsAt: &reset, Since: since},
		"unknown": {Kind: LimitUnknown, Since: since},
		"cleared": {Since: since, Cleared: true},
	} {
		in := RunReport{RunID: "run_1", SpaceID: "spc_1", Generation: 3, State: RunRunning, Status: "idle", Limit: l, ObservedAt: since}
		b, err := Encode("msg_1", TypeRunReport, since, in)
		if err != nil {
			t.Fatal(err)
		}
		env, err := Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		var out RunReport
		if err := env.Decode(&out); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(out, in) {
			t.Errorf("%s: round trip\n got %+v (limit %+v)\nwant %+v (limit %+v)", name, out, out.Limit, in, in.Limit)
		}
	}
	// The wire names T3/T4 rely on.
	b, _ := json.Marshal(RunLimit{Kind: LimitSevenDay, ResetsAt: &since, Since: since, Cleared: true})
	for _, f := range []string{`"kind":"seven_day"`, `"resets_at":"2026-11-13T09:00:00Z"`, `"since":"2026-11-13T09:00:00Z"`, `"cleared":true`} {
		if !strings.Contains(string(b), f) {
			t.Errorf("%s missing from %s", f, b)
		}
	}
}

// The minor-12 field is additive and left out when empty, so a minor-11 peer sees the same messages as before.
func TestRunLimitOmitted(t *testing.T) {
	for _, v := range []any{RunReport{RunID: "run_1"}, RunLimit{}} {
		b, _ := json.Marshal(v)
		for _, f := range []string{"limit", "kind", "resets_at", "since", "cleared"} {
			if strings.Contains(string(b), `"`+f+`"`) {
				t.Errorf("%T carries %s when empty: %s", v, f, b)
			}
		}
	}
}

// A minor-11 node negotiates minor 11 with this central, and a report at that minor never carries a limit in either
// direction: the node strips it before sending and central ignores one it receives anyway.
func TestUsageLimitNegotiation(t *testing.T) {
	if MaxMinor < MinorUsageLimit || MinorUsageLimit != 12 {
		t.Fatalf("MaxMinor %d, MinorUsageLimit %d", MaxMinor, MinorUsageLimit)
	}
	m, err := Negotiate(0, MinorUsageLimit-1)
	if err != nil || m != 11 {
		t.Fatalf("minor-11 node: %d %v", m, err)
	}
	if m, err := Negotiate(0, MinorUsageLimit); err != nil || m != MinorUsageLimit {
		t.Fatalf("minor-12 node: %d %v", m, err)
	}
	r := RunReport{RunID: "run_1", Limit: &RunLimit{Kind: LimitFiveHour, Since: time.Now()}}
	old := r.ForMinor(m - 1)
	if old.Limit != nil {
		t.Fatalf("minor %d report keeps its limit: %+v", m-1, old.Limit)
	}
	b, _ := json.Marshal(r.ForMinor(11))
	if strings.Contains(string(b), `"limit"`) {
		t.Fatalf("minor-11 report on the wire carries a limit: %s", b)
	}
	if r.Limit == nil {
		t.Fatal("ForMinor changed the receiver")
	}
	if got := r.ForMinor(MinorUsageLimit); got.Limit != r.Limit {
		t.Fatal("minor-12 report lost its limit")
	}
}

func TestLimitKind(t *testing.T) {
	for _, k := range LimitKinds {
		if LimitKind(k) != k {
			t.Errorf("%q not on the allowlist", k)
		}
	}
	for _, k := range []string{"", "Five_Hour", "five_hour ", "daily", "seven_day_haiku"} {
		if LimitKind(k) != LimitUnknown {
			t.Errorf("%q kept", k)
		}
	}
	if slices.Contains(LimitKinds, "") {
		t.Error("empty kind on the allowlist")
	}
}

func TestRunLimitNormalized(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d).In(time.FixedZone("x", 3600)); return &v }
	for _, c := range []struct {
		name      string
		in        RunLimit
		kind      string
		wantReset bool
	}{
		{"in range", RunLimit{Kind: LimitFiveHour, ResetsAt: at(3 * time.Hour)}, LimitFiveHour, true},
		{"upper bound", RunLimit{Kind: LimitSevenDay, ResetsAt: at(MaxLimitReset)}, LimitSevenDay, true},
		{"too far", RunLimit{Kind: LimitSevenDay, ResetsAt: at(MaxLimitReset + time.Second)}, LimitSevenDay, false},
		{"now", RunLimit{Kind: LimitFiveHour, ResetsAt: at(0)}, LimitFiveHour, false},
		{"past", RunLimit{Kind: LimitFiveHour, ResetsAt: at(-time.Minute)}, LimitFiveHour, false},
		{"bad kind", RunLimit{Kind: "weekly", ResetsAt: at(time.Hour)}, LimitUnknown, true},
		{"cleared", RunLimit{Kind: LimitFiveHour, ResetsAt: at(time.Hour), Cleared: true}, "", false},
	} {
		got := c.in.Normalized(now)
		if got.Kind != c.kind || (got.ResetsAt != nil) != c.wantReset {
			t.Errorf("%s: %+v", c.name, got)
		}
		if got.ResetsAt != nil && (got.ResetsAt.Location() != time.UTC || !got.ResetsAt.Equal(*c.in.ResetsAt)) {
			t.Errorf("%s: reset %v, want %v in UTC", c.name, got.ResetsAt, c.in.ResetsAt)
		}
	}
}
