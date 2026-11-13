package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// A launch with Claude Code settings is never dispatched to a node older than minor 8, which would run without them;
// the run fails as node_too_old instead of being retried (ADR-0059).
func TestClaudeSettingsNeedMinor8(t *testing.T) {
	payload, _ := json.Marshal(proto.LaunchPayload{Harness: "claude-code", Role: "implementor", Kind: "worker",
		ClaudeSettings: proto.ClaudeSettings{"permissions": map[string]any{"deny": []string{"Bash(rm:*)"}}}})
	c := db.NodeCommand{ID: "cmd_1", Kind: proto.CmdLaunch, SpaceID: "spc_a", PayloadJson: string(payload)}
	_, err := (&Hub{}).materialize(context.Background(), c, "nod_1", proto.MinorClaudeSettings-1)
	if !errors.Is(err, ErrNodeTooOld) || undispatchable(err) != "node_too_old" {
		t.Fatalf("old node: %v", err)
	}
	if undispatchable(ErrCredentialsUnavailable) != "account_unavailable" || undispatchable(errors.New("io")) != "" {
		t.Fatal("end reasons")
	}
}

// A run's command whose stored payload doesn't decode fails the run as invalid_command instead of being retried every
// dispatch pass: the payload never changes.
func TestUndecodablePayloadIsUndispatchable(t *testing.T) {
	for _, kind := range []string{proto.CmdLaunch, proto.CmdReposSync, proto.CmdRelocatePrepare} {
		c := db.NodeCommand{ID: "cmd_1", Kind: kind, SpaceID: "spc_a", PayloadJson: "{"}
		h := &Hub{RepoKey: func(context.Context, string, string) (string, error) { return "", nil }}
		_, err := h.materialize(context.Background(), c, "nod_1", proto.MinorRepoKeys)
		if !errors.Is(err, errBadPayload) || undispatchable(err) != "invalid_command" {
			t.Fatalf("%s: %v", kind, err)
		}
	}
}

// A pinned launch is never dispatched to a node older than minor 11, which would ignore the pin and run whatever version
// it has; the run fails as node_too_old with a clear error (ADR-0077).
func TestHarnessVersionPinNeedsMinor11(t *testing.T) {
	pinned, _ := json.Marshal(proto.LaunchPayload{Harness: "claude-code", Role: "implementor", Kind: "worker", HarnessVersion: "2.1.317"})
	c := db.NodeCommand{ID: "cmd_1", Kind: proto.CmdLaunch, SpaceID: "spc_a", PayloadJson: string(pinned)}
	_, err := (&Hub{}).materialize(context.Background(), c, "nod_1", proto.MinorHarnessVersion-1)
	if !errors.Is(err, ErrNodeTooOld) || undispatchable(err) != "node_too_old" {
		t.Fatalf("old node: %v", err)
	}
	if !strings.Contains(err.Error(), "node nod_1 doesn't support pinned Claude Code versions; upgrade it") || !strings.Contains(err.Error(), "2.1.317") {
		t.Fatalf("error text: %v", err)
	}
}
