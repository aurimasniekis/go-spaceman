package app_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
)

// SM-NODE-003: a launch whose delivery failed once for a transient reason (here the account's secrets; before
// d8fa263 also a reader connection's SQLITE_BUSY) is delivered again on its own. The dispatch loop used to drop it
// until something else kicked the node, so the run stayed pending ("timed out waiting for runs running").
func TestLaunchDeliveredAfterATransientDispatchError(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	secrets := e.c.Fleet.Hub().Secrets
	var failed atomic.Int32
	e.c.Fleet.Hub().Secrets = func(ctx context.Context, run *fleet.Run) (map[string]string, error) {
		if failed.CompareAndSwap(0, 1) {
			return nil, errors.New("transient: database is locked")
		}
		return secrets(ctx, run)
	}
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "retried", Name: "Retried", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "run running after the failed delivery", func() bool { return e.run1(r.ID).Observed == "running" })
	if failed.Load() != 1 {
		t.Fatal("the delivery never failed: the test proves nothing")
	}
}
