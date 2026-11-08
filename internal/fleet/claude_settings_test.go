package fleet

import (
	"context"
	"encoding/json"
	"errors"
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
