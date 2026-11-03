package audit

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/store"
)

func prune(t *testing.T, s *store.Store, w *Writer, cutoff int64) (int64, int64) {
	t.Helper()
	var n, through int64
	if err := s.W().Tx(context.Background(), func(tx *store.Tx) error {
		var err error
		n, through, err = w.Prune(context.Background(), tx, cutoff, System)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n, through
}

// Retention (ADR-0045): pruning deletes the oldest events in batches, keeps the head, records the pruned prefix and an
// audit.pruned event, and verification continues from the recorded hash; a gap after the prefix is still detected.
func TestPruneKeepsChainVerifiable(t *testing.T) {
	ctx := context.Background()
	s, w := setup(t)
	if n, _ := prune(t, s, w, time.Now().Add(time.Hour).UnixMicro()); n != 0 {
		t.Fatal("pruned an empty log")
	}
	if pr, err := PrunedPrefix(ctx, s.R()); err != nil || pr != nil {
		t.Fatalf("prefix of a fresh log %+v %v", pr, err)
	}
	appendN(t, s, w, 10)
	future := time.Now().Add(time.Hour).UnixMicro()
	n, through := prune(t, s, w, future)
	if n != 9 || through != 9 {
		t.Fatalf("pruned %d through %d (the head must stay)", n, through)
	}
	pr, err := PrunedPrefix(ctx, s.R())
	if err != nil || pr == nil || pr.Through != 9 || pr.Count != 9 || len(pr.Hash) != 32 {
		t.Fatalf("prefix %+v %v", pr, err)
	}
	res, err := Verify(ctx, s.R())
	if err != nil || !res.OK || res.PrunedThrough != 9 || res.Events != 2 || res.HeadSeq != 11 {
		t.Fatalf("verify %+v %v", res, err)
	}
	// Nothing older than an early cutoff: no change.
	if n, _ := prune(t, s, w, 1); n != 0 {
		t.Fatal("pruned events newer than the cutoff")
	}
	// A second prune continues from the prefix and adds to the count.
	appendN(t, s, w, 3)
	n, through = prune(t, s, w, future)
	if n != 4 || through != 13 {
		t.Fatalf("second prune %d through %d", n, through)
	}
	if pr, _ := PrunedPrefix(ctx, s.R()); pr.Count != 13 {
		t.Fatalf("count %d", pr.Count)
	}
	if res, _ := Verify(ctx, s.R()); !res.OK {
		t.Fatalf("after second prune %+v", res)
	}
	// Deleting a retained event right after the prefix is a sequence gap.
	if err := s.W().Exec(ctx, "DELETE FROM audit_events WHERE seq = 14"); err != nil {
		t.Fatal(err)
	}
	if res, _ := Verify(ctx, s.R()); res.OK || res.BrokenAt != 15 {
		t.Fatalf("gap after the prefix not detected: %+v", res)
	}
}
