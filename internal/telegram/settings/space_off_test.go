package settings

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/store"
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

// The first Load is silent even when a space is already visible at startup; after a terminal_watch toggle-off, the
// second Load reports exactly that space, and not the one that stayed on.
func TestOnSpaceOffIsSilentOnTheFirstLoad(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t, nil)
	ctx := context.Background()
	exec := func(q string) {
		t.Helper()
		if err := s.W().TxNamed(ctx, "test.space_off", func(tx *store.Tx) error {
			_, err := tx.SQL.ExecContext(ctx, q)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// spc_a is visible with terminal watching, spc_b is visible without it: both are on before the first Load.
	exec(`INSERT INTO space_telegram_settings (space_id, enabled, terminal_watch, created_at, created_by, updated_at, updated_by)
		VALUES ('spc_a', 1, 1, 0, 't', 0, 't'), ('spc_b', 1, 0, 0, 't', 0, 't')`)
	var mu sync.Mutex
	var got []string
	svc.OnSpaceOff(func(id string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, id)
	})
	calls := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}

	if err := svc.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if c := calls(); len(c) != 0 {
		t.Fatalf("the first Load reported %v, want nothing", c)
	}
	if !svc.Space("spc_a").Visible() {
		t.Fatal("spc_a is not visible after the first Load")
	}

	exec(`UPDATE space_telegram_settings SET terminal_watch = 0, version = version + 1 WHERE space_id = 'spc_a'`)
	if err := svc.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if c := calls(); !slices.Equal(c, []string{"spc_a"}) {
		t.Fatalf("the second Load reported %v, want [spc_a]", c)
	}
}
