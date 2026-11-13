package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Bounds on a node's capabilities, from hello or node.capabilities: they are stored and shown in the node detail.
const (
	maxCapabilitiesJSON = 64 << 10
	maxCapHarnesses     = 16
	maxCapList          = 64 // features, a harness's capabilities
	maxCapCached        = 256
	maxCapString        = 512
	maxCapPath          = 4096
)

// cleanCapabilities bounds a node's capabilities, from hello or node.capabilities (ADR-0077), before central stores
// them: lists longer than their bound are cut, an over-long field is cleared, a harness without a kind is dropped and,
// on a node-managed harness, a channel, version, channel target or cached version of a shape the node's release cache
// would not accept is dropped from that entry. The rest of the report is kept; problems lists what was cleaned.
func cleanCapabilities(c proto.Capabilities) (out proto.Capabilities, problems []string) {
	fix := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	short := func(what string, s *string, limit int) {
		if len(*s) > limit {
			fix("%s is longer than %d bytes", what, limit)
			*s = ""
		}
	}
	out = c
	for _, f := range []*string{&out.OS, &out.Arch, &out.Runner.Kind, &out.Runner.Version, &out.Runner.CoreID, &out.Runner.Error} {
		short("a runner or platform field", f, maxCapString)
	}
	if len(c.Features) > maxCapList {
		fix("more than %d features", maxCapList)
	}
	out.Features = make([]string, 0, min(len(c.Features), maxCapList))
	for _, f := range c.Features[:min(len(c.Features), maxCapList)] {
		if len(f) > maxCapString {
			fix("a feature is longer than %d bytes", maxCapString)
			continue
		}
		out.Features = append(out.Features, f)
	}
	if len(c.Harnesses) > maxCapHarnesses {
		fix("more than %d harnesses", maxCapHarnesses)
	}
	out.Harnesses = make([]proto.HarnessInfo, 0, min(len(c.Harnesses), maxCapHarnesses))
	for _, h := range c.Harnesses[:min(len(c.Harnesses), maxCapHarnesses)] {
		if h.Kind == "" || len(h.Kind) > maxCapString {
			fix("a harness without a valid kind")
			continue
		}
		out.Harnesses = append(out.Harnesses, cleanHarness(h, func(format string, args ...any) { fix("harness "+h.Kind+": "+format, args...) }))
	}
	return out, problems
}

// cleanHarness is cleanCapabilities for one harness entry; it never changes the caller's slices or map.
func cleanHarness(h proto.HarnessInfo, fix func(format string, args ...any)) proto.HarnessInfo {
	short := func(what string, s *string, limit int) {
		if len(*s) > limit {
			fix("%s is longer than %d bytes", what, limit)
			*s = ""
		}
	}
	short("version", &h.Version, maxCapString)
	short("error", &h.Error, maxCapString)
	short("channel", &h.Channel, maxCapString)
	short("path", &h.Path, maxCapPath)
	if len(h.Capabilities) > maxCapList {
		fix("more than %d capabilities", maxCapList)
	}
	caps := make([]string, 0, min(len(h.Capabilities), maxCapList))
	for _, s := range h.Capabilities[:min(len(h.Capabilities), maxCapList)] {
		if len(s) > maxCapString {
			fix("a capability is longer than %d bytes", maxCapString)
			continue
		}
		caps = append(caps, s)
	}
	h.Capabilities = caps
	if !h.Managed {
		if h.Channel != "" || len(h.Resolved) > 0 || len(h.Cached) > 0 || h.CacheBytes != 0 {
			fix("managed details on an unmanaged harness")
		}
		h.Channel, h.Resolved, h.Cached, h.CacheBytes = "", nil, nil, 0
		return h
	}
	if h.Channel != "" && proto.CheckHarnessVersion(h.Channel) != nil {
		fix("invalid channel")
		h.Channel = ""
	}
	if h.Version != "" && !proto.IsHarnessVersion(h.Version) {
		fix("invalid version")
		h.Version = ""
	}
	var resolved map[string]proto.HarnessResolved
	for ch, r := range h.Resolved {
		if (ch != proto.HarnessChannelLatest && ch != proto.HarnessChannelStable) || !proto.IsHarnessVersion(r.Version) {
			fix("invalid channel target")
			continue
		}
		if resolved == nil {
			resolved = make(map[string]proto.HarnessResolved, 2)
		}
		resolved[ch] = r
	}
	h.Resolved = resolved
	if len(h.Cached) > maxCapCached {
		fix("more than %d cached versions", maxCapCached)
	}
	var cached []string
	for _, v := range h.Cached[:min(len(h.Cached), maxCapCached)] {
		if !proto.IsHarnessVersion(v) {
			fix("invalid cached version")
			continue
		}
		cached = append(cached, v)
	}
	h.Cached = cached
	if h.CacheBytes < 0 {
		fix("negative cache size")
		h.CacheBytes = 0
	}
	return h
}

// capabilitiesJSON cleans c (cleanCapabilities), logs what was cleaned and returns what central stores; ok is false
// when even the cleaned capabilities are larger than maxCapabilitiesJSON.
func capabilitiesJSON(log *slog.Logger, from string, c proto.Capabilities) (string, bool) {
	c, problems := cleanCapabilities(c)
	if len(problems) > 0 {
		log.Warn("node capabilities cleaned", slog.String("from", from), slog.String("reason", strings.Join(problems, "; ")))
	}
	b, err := json.Marshal(c)
	if err != nil || len(b) > maxCapabilitiesJSON {
		return "", false
	}
	return string(b), true
}

// helloCapabilities is what central stores from a hello's capabilities: cleaned like a report (capabilitiesJSON), and
// when even that is too large, the platform alone without its harnesses, git and extra. A hello is never refused for
// its capabilities.
func helloCapabilities(log *slog.Logger, c proto.Capabilities) string {
	if caps, ok := capabilitiesJSON(log, "hello", c); ok {
		return caps
	}
	log.Warn("node capabilities cleaned", slog.String("from", "hello"),
		slog.String("reason", fmt.Sprintf("larger than %d bytes: harnesses, git and extra dropped", maxCapabilitiesJSON)))
	c.Harnesses, c.Git, c.Extra = []proto.HarnessInfo{}, nil, nil
	caps, _ := capabilitiesJSON(log, "hello", c)
	return caps
}

// nodeCapabilities stores a node.capabilities report (minor ≥ 11) the way hello does, from the node's newest connection
// only: a report still in flight on a connection a newer one replaced must not overwrite the newer hello's. Invalid
// entries are cleaned with a warning (cleanCapabilities) and a report too large even then is dropped; the connection
// stays up.
func (h *Hub) nodeCapabilities(ctx context.Context, s *session, env proto.Envelope) error {
	if s.minor < proto.MinorHarnessVersion {
		return apperr.New(apperr.CodeInvalid, "node.capabilities needs protocol minor %d", proto.MinorHarnessVersion)
	}
	var rep proto.NodeCapabilities
	if err := env.Decode(&rep); err != nil {
		return err
	}
	caps, ok := capabilitiesJSON(s.log, "report", rep.Capabilities)
	if !ok {
		s.log.Warn("node capabilities report dropped", slog.String("reason", fmt.Sprintf("larger than %d bytes", maxCapabilitiesJSON)))
		return nil
	}
	return h.svc.s.W().TxNamed(ctx, "fleet.node_capabilities", func(tx *store.Tx) error {
		// Write transactions are serialized with the connect's, which sets newest: the same check onDisconnect makes.
		h.mu.Lock()
		newest := h.newest[s.nodeID] == s.seq && h.sessions[s.nodeID] == s
		h.mu.Unlock()
		if !newest {
			return nil
		}
		now := tx.NowMicros()
		_, err := tx.Q.RecordNodeCapabilities(ctx, db.RecordNodeCapabilitiesParams{CapabilitiesJson: caps,
			Now: sql.NullInt64{Int64: now, Valid: true}, ID: s.nodeID})
		return err
	})
}
