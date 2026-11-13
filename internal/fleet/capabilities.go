package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Bounds on a node.capabilities report: it is stored as is and shown in the node detail.
const (
	maxCapabilitiesJSON = 64 << 10
	maxCapHarnesses     = 16
	maxCapList          = 64 // features, a harness's capabilities
	maxCapCached        = 256
	maxCapString        = 512
	maxCapPath          = 4096
)

// checkCapabilities validates a node.capabilities report (ADR-0077): bounded sizes, and on a node-managed harness a
// channel, channel targets and cached versions of the shapes the node's release cache accepts.
func checkCapabilities(c proto.Capabilities) error {
	short := func(what, s string, limit int) error {
		if len(s) > limit {
			return fmt.Errorf("%s is longer than %d bytes", what, limit)
		}
		return nil
	}
	if len(c.Harnesses) > maxCapHarnesses || len(c.Features) > maxCapList {
		return fmt.Errorf("too many harnesses or features")
	}
	for _, s := range []string{c.OS, c.Arch, c.Runner.Kind, c.Runner.Version, c.Runner.CoreID, c.Runner.Error} {
		if err := short("a runner or platform field", s, maxCapString); err != nil {
			return err
		}
	}
	for _, f := range c.Features {
		if err := short("a feature", f, maxCapString); err != nil {
			return err
		}
	}
	for _, h := range c.Harnesses {
		if h.Kind == "" {
			return fmt.Errorf("a harness without a kind")
		}
		for _, s := range []string{h.Kind, h.Version, h.Error, h.Channel} {
			if err := short("harness "+h.Kind+" field", s, maxCapString); err != nil {
				return err
			}
		}
		if err := short("harness "+h.Kind+" path", h.Path, maxCapPath); err != nil {
			return err
		}
		if len(h.Capabilities) > maxCapList || len(h.Cached) > maxCapCached || h.CacheBytes < 0 {
			return fmt.Errorf("harness %s: too many capabilities or cached versions, or a negative cache size", h.Kind)
		}
		if !h.Managed {
			if h.Channel != "" || len(h.Resolved) > 0 || len(h.Cached) > 0 || h.CacheBytes != 0 {
				return fmt.Errorf("harness %s: managed details on an unmanaged harness", h.Kind)
			}
			continue
		}
		if h.Channel == "" || proto.CheckHarnessVersion(h.Channel) != nil {
			return fmt.Errorf("harness %s: invalid channel", h.Kind)
		}
		if h.Version != "" && !proto.IsHarnessVersion(h.Version) {
			return fmt.Errorf("harness %s: invalid version", h.Kind)
		}
		for ch, r := range h.Resolved {
			if (ch != proto.HarnessChannelLatest && ch != proto.HarnessChannelStable) || !proto.IsHarnessVersion(r.Version) {
				return fmt.Errorf("harness %s: invalid channel target", h.Kind)
			}
		}
		for _, v := range h.Cached {
			if !proto.IsHarnessVersion(v) {
				return fmt.Errorf("harness %s: invalid cached version", h.Kind)
			}
		}
	}
	return nil
}

// nodeCapabilities stores a node.capabilities report (minor ≥ 11) the way hello does, from the node's newest connection
// only: a report still in flight on a connection a newer one replaced must not overwrite the newer hello's. An invalid
// report is dropped with a warning; the connection stays up.
func (h *Hub) nodeCapabilities(ctx context.Context, s *session, env proto.Envelope) error {
	if s.minor < proto.MinorHarnessVersion {
		return apperr.New(apperr.CodeInvalid, "node.capabilities needs protocol minor %d", proto.MinorHarnessVersion)
	}
	var rep proto.NodeCapabilities
	if err := env.Decode(&rep); err != nil {
		return err
	}
	caps, err := json.Marshal(rep.Capabilities)
	if err == nil && len(caps) > maxCapabilitiesJSON {
		err = fmt.Errorf("larger than %d bytes", maxCapabilitiesJSON)
	}
	if err == nil {
		err = checkCapabilities(rep.Capabilities)
	}
	if err != nil {
		s.log.Warn("node capabilities report dropped", slog.String("reason", err.Error()))
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
		_, err := tx.Q.RecordNodeCapabilities(ctx, db.RecordNodeCapabilitiesParams{CapabilitiesJson: string(caps),
			Now: sql.NullInt64{Int64: now, Valid: true}, ID: s.nodeID})
		return err
	})
}
