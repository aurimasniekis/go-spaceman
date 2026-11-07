package app_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// SM-API-002 (security review SR-L01): concurrent requests with the same Idempotency-Key create the resource once;
// the others get the stored result.
func TestConcurrentIdempotentCreatesOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, err := h.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	en, err := h.c.Fleet.CreateNode(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_test"}, fleet.CreateNodeInput{Name: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"slug": "once", "name": "Once", "node_id": en.Node.ID}
	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := h.client()
			c.bearer = rootSecret
			r := c.do("POST", "/spaces", body, "Idempotency-Key", "concurrent-create-1")
			var sp struct {
				ID string `json:"id"`
			}
			if r.status/100 == 2 {
				_ = json.Unmarshal(r.body, &sp)
			}
			ids[i] = sp.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("concurrent idempotent creates returned %v", ids)
		}
	}
	spaces, err := h.c.Fleet.ListSpaces(ctx, func(string) bool { return true })
	if err != nil || len(spaces) != 1 {
		t.Fatalf("%d spaces after %d concurrent creates with one key", len(spaces), n)
	}
}
