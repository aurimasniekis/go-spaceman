package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/dialog"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// ADR-0102 wiring: a run.dialog a node sends over its connection reaches the notice through central's OnRunDialog hook
// (so turning that hook into a no-op fails here), once per occurrence however often the node repeats it.
func TestRunDialogOverTheNodeConnection(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	nodeID := nodes[0].ID
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "wired", Name: "Wired", NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.c.Store.W().TxNamed(ctx, "test.dialog_run", func(tx *store.Tx) error {
		now := time.Now().UnixMicro()
		return tx.Q.InsertRun(ctx, db.InsertRunParams{ID: "run_wired", SpaceID: sp.ID, NodeID: nodeID, Generation: 1, Kind: "worker",
			Role: "implementor", Harness: hns.KindClaude, InstanceID: "ins_wired", SessionID: "ses_wired", LaunchJson: "{}", ManifestJson: "{}",
			CreatedAt: now, CreatedBy: "usr_test", UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}

	n := dialRawNode(t, e, proto.MinorDialog)
	n.next(t, proto.TypeWelcome)
	eventually(t, "node connected", func() bool { x, _ := e.c.Fleet.GetNode(ctx, nodeID); return x.Connected })
	d := proto.RunDialog{ID: ids.New(ids.Dialog), RunID: "run_wired", SpaceID: sp.ID, Generation: 1, Outcome: proto.DialogBlocked,
		Dialog: "unknown", Signal: dialog.SignalPrompt, Excerpt: "Pick a thing\n❯ 1. Yes", DetectedAt: time.Now()}
	for range 3 {
		n.send(t, proto.TypeRunDialog, d)
	}
	count := func(to string) (c int) {
		_ = e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE space_id = ? AND from_kind = 'system' AND to_kind = ?
			AND body LIKE '%blocked_on_dialog%' AND body LIKE '%SpaceMan does not know this dialog%' AND body LIKE '%❯ 1. Yes%'`, sp.ID, to).Scan(&c)
		return c
	}
	eventually(t, "notice to the operators and control", func() bool { return count("operators") >= 1 && count("control") >= 1 })
	time.Sleep(300 * time.Millisecond) // the repeats are processed in order behind the first
	if o, c := count("operators"), count("control"); o != 1 || c != 1 {
		t.Fatalf("%d notices to operators, %d to control (want 1 each)", o, c)
	}
}
