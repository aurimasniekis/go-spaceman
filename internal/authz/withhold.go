package authz

import (
	"fmt"
	"testing"
)

// WithholdForTest removes perms from every built-in role below admin, in memory only, so a test can exercise a
// principal that holds some permissions of a space but not others (no built-in role does: viewer holds every read
// permission). Admins keep everything. It panics outside a test binary (testing.Testing() is false), so production
// code can never reach it. A Reload restores the policy, so call it after the last grant change.
func (en *Engine) WithholdForTest(perms ...Permission) error {
	if !testing.Testing() {
		panic("authz: WithholdForTest is only for test binaries")
	}
	en.mu.Lock()
	defer en.mu.Unlock()
	for _, r := range Roles[:len(Roles)-1] {
		for _, p := range perms {
			if _, err := en.e.RemovePolicy(string(r), p.Object, p.Action); err != nil {
				return fmt.Errorf("withhold %s.%s from %s: %w", p.Object, p.Action, r, err)
			}
		}
	}
	return nil
}
