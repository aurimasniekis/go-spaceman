package fleet

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

func capsEnvelope(t *testing.T, c proto.Capabilities) proto.Envelope {
	t.Helper()
	b, err := proto.Encode("msg_caps", proto.TypeNodeCapabilities, time.Now(), proto.NodeCapabilities{Capabilities: c})
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func managedCaps(version string, cached ...string) proto.Capabilities {
	return proto.Capabilities{OS: "linux", Arch: "amd64", Features: []string{}, Harnesses: []proto.HarnessInfo{{
		Kind: "claude-code", Version: version, Path: "/data/harness/claude-code/" + version + "/claude", Available: true, Capabilities: []string{},
		Managed: true, Channel: "latest", Resolved: map[string]proto.HarnessResolved{"latest": {Version: version, ResolvedAt: time.Date(2026, 11, 13, 6, 0, 0, 0, time.UTC)}},
		Cached: cached, CacheBytes: 1 << 20,
	}}}
}

// SM-HARN-008: central stores a node.capabilities report from the node's current connection (minor ≥ 11) with when it
// arrived, ignores one from a connection a newer one replaced, refuses it below minor 11 and drops an invalid one.
func TestNodeCapabilitiesReport(t *testing.T) {
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at, capabilities_json) VALUES ('nod_1', 'n1', 'active', 0, 't', 0, '{"os":"hello"}')`)
	h := e.svc.hub
	s1 := &session{nodeID: "nod_1", seq: 1, minor: proto.MinorHarnessVersion, log: slog.New(slog.DiscardHandler)}
	h.mu.Lock()
	h.seq, h.newest["nod_1"], h.sessions["nod_1"] = 1, 1, s1
	h.mu.Unlock()
	stored := func() (proto.Capabilities, *time.Time) {
		t.Helper()
		n, err := e.s.R().GetNode(e.ctx, "nod_1")
		if err != nil {
			t.Fatal(err)
		}
		var c proto.Capabilities
		if err := json.Unmarshal([]byte(n.CapabilitiesJson), &c); err != nil {
			t.Fatal(err)
		}
		return c, e.svc.nodeFromRow(n).CapabilitiesUpdatedAt
	}

	if err := h.handle(e.ctx, s1, capsEnvelope(t, managedCaps("2.1.901", "2.1.901", "2.1.900"))); err != nil {
		t.Fatal(err)
	}
	c, at := stored()
	if len(c.Harnesses) != 1 || c.Harnesses[0].Version != "2.1.901" || len(c.Harnesses[0].Cached) != 2 || at == nil || !at.Equal(e.t0) {
		t.Fatalf("stored %+v at %v", c, at)
	}

	// Invalid reports are dropped and the connection stays: nothing changes.
	bad := []proto.Capabilities{
		managedCaps("2.1.902", "2.1.902", "../../etc"),
		managedCaps("not-a-version"),
		{Harnesses: []proto.HarnessInfo{{Kind: "claude-code", Channel: "latest"}}}, // managed details on an unmanaged entry
		{Harnesses: []proto.HarnessInfo{{Kind: "claude-code", Path: strings.Repeat("p", 5000)}}},
		{Harnesses: []proto.HarnessInfo{{Version: "1.0.0"}}},
		{Harnesses: make([]proto.HarnessInfo, 17)},
	}
	resolvedBad := managedCaps("2.1.902", "2.1.902")
	resolvedBad.Harnesses[0].Resolved["nightly"] = proto.HarnessResolved{Version: "2.1.902"}
	bad = append(bad, resolvedBad)
	for i, c := range bad {
		if err := h.handle(e.ctx, s1, capsEnvelope(t, c)); err != nil {
			t.Fatalf("bad report %d closed the connection: %v", i, err)
		}
		if got, _ := stored(); got.Harnesses[0].Version != "2.1.901" {
			t.Fatalf("bad report %d stored: %+v", i, got)
		}
	}

	// A newer connection replaced s1 (its hello is stored): a report still in flight on s1 changes nothing.
	s2 := &session{nodeID: "nod_1", seq: 2, minor: proto.MinorHarnessVersion, log: slog.New(slog.DiscardHandler)}
	h.mu.Lock()
	h.seq, h.newest["nod_1"], h.sessions["nod_1"] = 2, 2, s2
	h.mu.Unlock()
	e.clk.Advance(time.Minute)
	if err := h.handle(e.ctx, s1, capsEnvelope(t, managedCaps("2.1.800", "2.1.800"))); err != nil {
		t.Fatal(err)
	}
	if got, at := stored(); got.Harnesses[0].Version != "2.1.901" || !at.Equal(e.t0) {
		t.Fatalf("the replaced connection's report was stored: %+v at %v", got, at)
	}
	if err := h.handle(e.ctx, s2, capsEnvelope(t, managedCaps("2.1.902", "2.1.902"))); err != nil {
		t.Fatal(err)
	}
	if got, at := stored(); got.Harnesses[0].Version != "2.1.902" || !at.Equal(e.t0.Add(time.Minute)) {
		t.Fatalf("the current connection's report: %+v at %v", got, at)
	}

	// Below minor 11 the message is a protocol error (an old node never sends it).
	old := &session{nodeID: "nod_1", seq: 2, minor: proto.MinorHarnessVersion - 1, log: slog.New(slog.DiscardHandler)}
	if err := h.handle(e.ctx, old, capsEnvelope(t, managedCaps("2.1.903"))); err == nil {
		t.Fatal("node.capabilities accepted at minor 10")
	}
}
