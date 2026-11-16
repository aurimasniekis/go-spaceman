package fleet

import (
	"errors"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
)

// ADR-0093: while a rename's node move is pending, launches of its new and old names and dream launches in its space are held
// (role_rename_pending) and write nothing; other roles, other spaces and, once the move is reported, the renamed role
// launch.
func TestLaunchHeldWhileRoleRenamePending(t *testing.T) {
	e := newHoldEnv(t)
	e.exec(`INSERT INTO space_role_renames (id, space_id, from_name, to_name, renamed_at, renamed_by, node_id, node_state)
		VALUES ('rrn_1', 'spc_a', 'coder', 'implementor', 1, 't', 'nod_1', 'pending')`)
	held := func(in LaunchInput, dream bool) {
		t.Helper()
		before := e.writes()
		_, err := e.svc.LaunchRun(e.ctx, holdActor, "spc_a", in)
		if apperr.CodeOf(err) != apperr.CodePrecondition {
			t.Fatalf("launch %s/%s: %v, want a precondition hold", in.Kind, in.Role, err)
		}
		var pe *RenamePendingError
		if !errors.As(err, &pe) || pe.RenameID != "rrn_1" || pe.NodeID != "nod_1" || pe.Dream != dream {
			t.Fatalf("cause %#v", pe)
		}
		if !strings.Contains(err.Error(), ReasonRoleRenamePending) || !strings.Contains(err.Error(), "nod_1") {
			t.Errorf("message %q names neither the reason nor the node", err)
		}
		if e.writes() != before {
			t.Error("a held launch wrote rows")
		}
	}
	held(LaunchInput{Kind: "worker", Role: "implementor", Harness: "claude-code", Prompt: "go"}, false)
	held(LaunchInput{Kind: "dreamer", Role: "dreamer", Harness: "claude-code", Prompt: "go"}, true)
	// The old name too: the node's directory of that name is the one still to move.
	held(LaunchInput{Kind: "worker", Role: "coder", Harness: "claude-code", Prompt: "go"}, false)

	if _, err := e.svc.LaunchRun(e.ctx, holdActor, "spc_a", LaunchInput{Kind: "worker", Role: "reviewer", Harness: "claude-code", Prompt: "go"}); err != nil {
		t.Fatalf("another role: %v", err)
	}
	if _, err := e.launch("spc_b", ""); err != nil {
		t.Fatalf("another space: %v", err)
	}
	e.exec(`UPDATE space_role_renames SET node_state = 'done' WHERE id = 'rrn_1'`)
	if _, err := e.launch("spc_a", ""); err != nil {
		t.Fatalf("after the node move: %v", err)
	}
}
