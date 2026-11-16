package spacemanmcp_test

import (
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/spacemanmcp"
)

// ADR-0093 on the admin MCP: renaming a role moves its memory, open tasks and messages, so it is a dangerous change
// (its own tool gate), and listing the renames only reads. Pinned by name so a reclassification is a reviewed change.
func TestRoleRenameClasses(t *testing.T) {
	t.Parallel()
	_, d := buildSpec(t)
	for _, c := range []struct {
		op    string
		class spacemanmcp.Class
	}{
		{"rename-space-role", spacemanmcp.Dangerous},
		{"list-space-role-renames", spacemanmcp.Read},
	} {
		r, ok := d.RuleFor(c.op)
		if !ok {
			t.Fatalf("%s has no rule", c.op)
		}
		e, ok := spacemanmcp.Classify(c.op, r)
		if !ok || e.Class != c.class {
			t.Errorf("%s: class %q (classified %v), want %q", c.op, e.Class, ok, c.class)
		}
	}
}
