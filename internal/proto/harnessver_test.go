package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckHarnessVersion(t *testing.T) {
	if MaxMinor < MinorHarnessVersion {
		t.Fatal("this build doesn't speak minor 11")
	}
	for _, ok := range []string{"", "latest", "stable", "0.0.0", "2.1.322", "999999.0.1"} {
		if err := CheckHarnessVersion(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"Latest", "rc", "v2.1.3", "2.1", "2.1.3.4", "02.1.3", "2.1.3-rc.1", "2.1.3+b", " 2.1.3", "1000000.0.0", "../2.1.3"} {
		if CheckHarnessVersion(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The minor-11 fields are additive and left out when empty, so an older peer sees the same messages as before.
func TestHarnessVersionFieldsOmitted(t *testing.T) {
	for _, v := range []any{LaunchPayload{Harness: "claude-code"}, RunReport{RunID: "run_1"}, HarnessInfo{Kind: "claude-code"}} {
		b, _ := json.Marshal(v)
		for _, f := range []string{"harness_version", "harness_requested", "harness_fallback", "managed", "channel", "resolved", "cached", "cache_bytes"} {
			if strings.Contains(string(b), `"`+f+`"`) {
				t.Errorf("%T carries %s when empty: %s", v, f, b)
			}
		}
	}
}
