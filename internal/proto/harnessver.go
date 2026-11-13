package proto

import (
	"fmt"
	"regexp"
	"time"
)

// MinorHarnessVersion is the first minor version that carries harness versions (ADR-0077): launches may carry the
// profile's pin (LaunchPayload.HarnessVersion), run reports carry the version a run actually launched, and HarnessInfo
// describes a node-managed harness. Central never dispatches a pinned launch to an older node: it would ignore the pin.
const MinorHarnessVersion = 11

// Harness version requests: a profile pin or a node channel.
const (
	HarnessChannelLatest = "latest"
	HarnessChannelStable = "stable"
)

// reHarnessVersion is a strict X.Y.Z (no leading zeros, no pre-release or build suffix), the same shape the node's
// release cache accepts (dist.ValidVersion).
var reHarnessVersion = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})$`)

// IsHarnessVersion reports whether v is an exact X.Y.Z version.
func IsHarnessVersion(v string) bool { return reHarnessVersion.MatchString(v) }

// CheckHarnessVersion validates a harness version request: "" (follow the node), latest, stable or an exact X.Y.Z.
func CheckHarnessVersion(v string) error {
	switch {
	case v == "", v == HarnessChannelLatest, v == HarnessChannelStable, IsHarnessVersion(v):
		return nil
	}
	return fmt.Errorf("harness version %q: want empty, latest, stable or an exact X.Y.Z", v)
}

// HarnessResolved is a node-managed harness's memoised channel target (HarnessInfo.Resolved, minor ≥ 11).
type HarnessResolved struct {
	Version    string    `json:"version"`
	ResolvedAt time.Time `json:"resolved_at,omitzero"`
}
