package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
)

// syncGate makes one Sync of central's recording segments, once armed, wait until released and then fail (EIO), so
// a test can have the node send its next chunk while central is still storing the failing one.
type syncGate struct {
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

type gatedSegment struct {
	recording.File
	g *syncGate
}

func (f gatedSegment) Sync() error {
	if f.g.armed.CompareAndSwap(true, false) {
		close(f.g.entered)
		<-f.g.release
		return syscall.EIO
	}
	return f.File.Sync()
}

// SM-REC-003 (ADR-0010 addendum), real central and agent: central fails to store one chunk while the node's next live
// chunk is already on the connection behind it. Central never stores that chunk past the hole; it closes the
// connection, and the node's catch-up on the next one re-ships from its acknowledgement, so central's copy ends up
// whole and contiguous.
func TestRecordingStoreFailureIsReshippedWithoutAHole(t *testing.T) {
	t.Parallel()
	g := &syncGate{entered: make(chan struct{}), release: make(chan struct{})}
	e := newFleetEnv(t)
	e.recCreate = func(path string) (recording.File, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // test path
		if err != nil {
			return nil, err
		}
		return gatedSegment{File: f, g: g}, nil
	}
	e.restartCentral() // central now writes its copies through the gate
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "rec-fail", Name: "Rec", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor-backend", Harness: hns.KindCodex})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "running", func() bool { return e.run1(r.ID).Observed == "running" })
	term := e.run.LiveByLabel("spaceman.run", r.ID)[0].ID
	central := func() (string, recording.ReadResult) {
		res, err := e.c.Fleet.Recordings().Read(context.Background(), r.ID, 0)
		if err != nil {
			return "", res
		}
		var out strings.Builder
		for _, rec := range res.Records {
			out.Write(rec.Data)
		}
		return out.String(), res
	}
	e.run.Output(term, "[mark-1]\r\n")
	eventually(t, "first output at central", func() bool { out, _ := central(); return strings.Contains(out, "[mark-1]") })

	g.armed.Store(true)
	e.run.Output(term, "[mark-2]\r\n")
	select {
	case <-g.entered:
	case <-time.After(time.Minute):
		t.Fatal("central did not store the second output")
	}
	// The next live chunk: the node records and ships it while central is still on the failing one.
	e.run.Output(term, "[mark-3]\r\n")
	local := filepath.Join(e.agentDir, "runs", r.ID, "recording")
	var markSeq uint64
	eventually(t, "next output recorded on the node", func() bool {
		res, _ := recording.Read(local, 0)
		for _, rec := range res.Records {
			if strings.Contains(string(rec.Data), "[mark-3]") {
				markSeq = rec.Seq
				return true
			}
		}
		return false
	})
	// Wait until the node has handed that record to the connection (central is blocked in its sync, so it is queued
	// behind the failing chunk), instead of sleeping.
	eventually(t, "next output shipped by the node", func() bool { return e.agent.SentRecordingSeq(r.ID) >= markSeq })
	close(g.release)

	eventually(t, "central holds the whole recording", func() bool {
		out, _ := central()
		if strings.Contains(out, "[mark-3]") && !strings.Contains(out, "[mark-2]") {
			t.Fatalf("central stored the next chunk past the hole: %q", out[strings.Index(out, "[mark-1]"):])
		}
		return strings.Contains(out, "[mark-1]\r\n[mark-2]\r\n[mark-3]")
	})
	_, res := central()
	for i := 1; i < len(res.Records); i++ {
		if res.Records[i].Seq != res.Records[i-1].Seq+1 {
			t.Fatalf("central's recording is not contiguous at %d: %d after %d", i, res.Records[i].Seq, res.Records[i-1].Seq)
		}
	}
	if res.Gaps != 0 || res.Truncated {
		t.Fatalf("central's recording has %d gaps, truncated %v", res.Gaps, res.Truncated)
	}
}
