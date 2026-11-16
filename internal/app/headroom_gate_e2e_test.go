package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// The launch-time headroom gate is wired into central with the real usage ledger (fleet.Options.AccountHeadroom): with
// the gate on and the only account's window at 95 %, a launch is refused by the gate, and a far reset is capped at an hour.
func TestHeadroomGateRefusesLaunchFromLedger(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "hrg", Name: "HRG", NodeID: nodes[0].ID, StarterRoles: true})
	if err != nil {
		t.Fatal(err)
	}
	acc := bindStubAccounts(t, e.c, sp.ID)["claude_oauth_token"]
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.UsageLimitMinHeadroomPercent: 20}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := e.c.Store.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO account_windows (account_id, kind, used_pct, resets_at, observed_at, run_id, updated_at) VALUES (?, 'seven_day', 95, ?, ?, '', ?)`,
			acc, now.Add(7*24*time.Hour).UnixMicro(), now.UnixMicro(), now.UnixMicro())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err = e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Role: "implementor", Harness: hns.KindClaude})
	h, ok := fleet.AsHold(err)
	if !ok || !h.Gate {
		t.Fatalf("launch not refused by the headroom gate: %v", err)
	}
	if !h.Known || h.Until.After(time.Now().Add(time.Hour+time.Minute)) {
		t.Fatalf("wait not capped: until %v known %v", h.Until, h.Known)
	}
}
