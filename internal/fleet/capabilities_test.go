package fleet

import (
	"encoding/json"
	"log/slog"
	"slices"
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
// arrived, ignores one from a connection a newer one replaced and refuses it below minor 11. A bad entry is cleaned and
// the rest stored; only a report too large even then is dropped.
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

	// A bad entry is cleaned, not the whole report refused: the rest is stored and the connection stays.
	bad := managedCaps("not-a-version", "2.1.902", "../../etc", "2.1.900")
	bad.Harnesses[0].Resolved = map[string]proto.HarnessResolved{"latest": {Version: "2.1.902"}, "stable": {Version: "2.1"}, "nightly": {Version: "2.1.902"}}
	bad.Harnesses = append(bad.Harnesses, proto.HarnessInfo{Kind: "codex", Version: "0.50.0", Path: strings.Repeat("p", 5000), Channel: "latest"},
		proto.HarnessInfo{Version: "1.0.0"})
	if err := h.handle(e.ctx, s1, capsEnvelope(t, bad)); err != nil {
		t.Fatalf("a bad entry closed the connection: %v", err)
	}
	c, _ = stored()
	if len(c.Harnesses) != 2 {
		t.Fatalf("harnesses %+v", c.Harnesses)
	}
	if cc := c.Harnesses[0]; cc.Version != "" || !slices.Equal(cc.Cached, []string{"2.1.902", "2.1.900"}) || len(cc.Resolved) != 1 || cc.Resolved["latest"].Version != "2.1.902" || cc.Channel != "latest" || !cc.Managed {
		t.Fatalf("claude-code entry %+v", cc)
	}
	if cx := c.Harnesses[1]; cx.Kind != "codex" || cx.Version != "0.50.0" || cx.Path != "" || cx.Channel != "" {
		t.Fatalf("codex entry %+v", cx)
	}
	// A report too large even when cleaned is dropped: nothing changes.
	huge := managedCaps("2.1.903", "2.1.903")
	huge.Extra = json.RawMessage(`"` + strings.Repeat("x", maxCapabilitiesJSON) + `"`)
	if err := h.handle(e.ctx, s1, capsEnvelope(t, huge)); err != nil {
		t.Fatal(err)
	}
	if got, _ := stored(); len(got.Harnesses) != 2 || got.Harnesses[1].Kind != "codex" {
		t.Fatalf("an oversized report was stored: %+v", got.Harnesses)
	}
	if err := h.handle(e.ctx, s1, capsEnvelope(t, managedCaps("2.1.901", "2.1.901", "2.1.900"))); err != nil {
		t.Fatal(err)
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

// SM-HARN-008: hello capabilities pass the report's bounds too. A valid inventory (an old node's unmanaged entries
// included) is stored unchanged; a bad entry is cleaned and the rest kept; lists are cut to their bounds; and a hello
// too large even when cleaned keeps its platform without harnesses, so the hello is still accepted.
func TestHelloCapabilitiesCleaned(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	decode := func(s string) proto.Capabilities {
		t.Helper()
		var c proto.Capabilities
		if err := json.Unmarshal([]byte(s), &c); err != nil {
			t.Fatal(err)
		}
		return c
	}

	valid := managedCaps("2.1.901", "2.1.901", "2.1.900")
	valid.Harnesses = append(valid.Harnesses, proto.HarnessInfo{Kind: "codex", Version: "codex-cli 0.50.0", Path: "/usr/bin/codex", Available: true, Capabilities: []string{"resume"}})
	want, _ := json.Marshal(valid)
	if got := helloCapabilities(log, valid); got != string(want) {
		t.Fatalf("a valid hello changed:\n%s\n%s", got, want)
	}
	if _, problems := cleanCapabilities(valid); len(problems) != 0 {
		t.Fatalf("problems on a valid hello: %v", problems)
	}

	bad := managedCaps("2.1.901-beta", "2.1.901", "2.1.9x")
	bad.Harnesses[0].Channel = "nightly"
	bad.Harnesses[0].CacheBytes = -1
	bad.Harnesses = append(bad.Harnesses, proto.HarnessInfo{Kind: "codex", Version: "0.50.0", Cached: []string{"0.50.0"}, CacheBytes: 5,
		Capabilities: make([]string, maxCapList+3)})
	bad.Features = make([]string, maxCapList+1)
	bad.Runner.Error = strings.Repeat("e", maxCapString+1)
	c := decode(helloCapabilities(log, bad))
	cc, cx := c.Harnesses[0], c.Harnesses[1]
	if cc.Version != "" || cc.Channel != "" || !slices.Equal(cc.Cached, []string{"2.1.901"}) || cc.CacheBytes != 0 || !cc.Managed || cc.Path == "" {
		t.Fatalf("claude-code entry %+v", cc)
	}
	if cx.Version != "0.50.0" || cx.Cached != nil || cx.CacheBytes != 0 || len(cx.Capabilities) != maxCapList {
		t.Fatalf("codex entry %+v", cx)
	}
	if len(c.Features) != maxCapList || c.Runner.Error != "" || c.OS != "linux" {
		t.Fatalf("platform %+v", c)
	}
	if bad.Harnesses[0].Cached[1] != "2.1.9x" || len(bad.Harnesses[1].Cached) != 1 {
		t.Fatal("cleaning changed the caller's capabilities")
	}

	huge := managedCaps("2.1.901", "2.1.901")
	huge.Extra = json.RawMessage(`"` + strings.Repeat("x", maxCapabilitiesJSON) + `"`)
	if c := decode(helloCapabilities(log, huge)); len(c.Harnesses) != 0 || c.Extra != nil || c.OS != "linux" {
		t.Fatalf("an oversized hello %+v", c)
	}
}
