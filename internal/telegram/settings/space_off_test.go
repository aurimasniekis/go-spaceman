package settings

import (
	"slices"
	"testing"
)

func TestSpacesOff(t *testing.T) {
	t.Parallel()
	on := func(id string, tw bool) Space {
		s := DefaultSpace(id)
		s.Enabled, s.TerminalWatch = true, tw
		return s
	}
	prev := map[string]Space{"a": on("a", true), "b": on("b", false), "c": on("c", true), "d": on("d", true)}
	next := map[string]Space{"a": on("a", false), "b": on("b", false), "d": on("d", true), "e": on("e", true)}
	next["d"] = func() Space { s := on("d", true); s.Enabled = false; return s }()
	// a: terminal_watch off; c: removed; d: hidden; b unchanged; e new.
	if got := spacesOff(prev, next); !slices.Equal(got, []string{"a", "c", "d"}) {
		t.Fatalf("got %v", got)
	}
}
