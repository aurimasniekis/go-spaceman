package storetest

import (
	"testing"
	"time"
)

func TestTxMaxFrom(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   string
		scale int
		want  time.Duration
	}{
		{"default without race", "", 1, 100 * time.Millisecond},
		{"default scales under race", "", 5, 500 * time.Millisecond},
		{"env wins, not scaled", "250ms", 5, 250 * time.Millisecond},
		{"env wins without race", "1s", 1, time.Second},
		{"invalid env falls back to scaled default", "bogus", 5, 500 * time.Millisecond},
		{"non-positive env falls back", "0s", 1, 100 * time.Millisecond},
	} {
		if got := txMaxFrom(tc.env, tc.scale); got != tc.want {
			t.Errorf("%s: txMaxFrom(%q, %d) = %s, want %s", tc.name, tc.env, tc.scale, got, tc.want)
		}
	}
}

// The build tag picks the scale: 5 under -race, 1 otherwise; MaxTxDuration is that scale applied to the environment.
func TestMaxTxDurationUsesBuildScale(t *testing.T) {
	if raceScale != 1 && raceScale != 5 {
		t.Fatalf("raceScale = %d, want 1 or 5", raceScale)
	}
	if want := txMaxFrom(getenvTxMax(), raceScale); MaxTxDuration != want {
		t.Fatalf("MaxTxDuration = %s, want %s", MaxTxDuration, want)
	}
}
