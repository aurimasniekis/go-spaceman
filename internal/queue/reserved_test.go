package queue

import (
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
)

// ADR-0087: a task under a reserved dedup namespace (an accepted cross-space request, a schedule occurrence) is
// created only by its service; an API or agent caller can't plant one that the service would then reuse.
func TestReservedTaskDedupNamespaces(t *testing.T) {
	f := newFx(t)
	for _, p := range reservedDedupPrefixes {
		key := p + "x1"
		_, _, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Title: "Planted", DedupKey: key})
		if apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Fatalf("outside caller with %q: %v", key, err)
		}
		a, created, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Title: "Genuine", DedupKey: key, Internal: true})
		must(t, err)
		again, created2, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Title: "Genuine", DedupKey: key, Internal: true})
		must(t, err)
		if !created || created2 || again.ID != a.ID || a.Title != "Genuine" {
			t.Fatalf("internal %s: %v %v %+v", key, created, created2, again)
		}
		if _, _, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Title: "Planted", DedupKey: key}); apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Fatalf("outside caller handed back %q: %v", key, err)
		}
	}
	for _, key := range []string{"", "mine:1", "xspace", "my-schedule:1"} {
		if _, _, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Title: "T", DedupKey: key, Internal: true}); err == nil {
			t.Fatalf("internal task with unreserved key %q accepted", key)
		}
	}
	a, _, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Title: "Mine", DedupKey: "mine:2"})
	must(t, err)
	b, created, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Title: "Mine", DedupKey: "mine:2"})
	must(t, err)
	if created || a.ID != b.ID {
		t.Fatalf("unreserved dedup: %v %s %s", created, a.ID, b.ID)
	}
}
