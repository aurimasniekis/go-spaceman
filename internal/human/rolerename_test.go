package human

import (
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
)

// ADR-0093: an action records the requester's role as it was. When the role is renamed before the response arrives,
// a notification held for the requester's next launch is labelled with the role's current name; a name a role has is
// never redirected.
func TestRequesterRoleFollowsRename(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.policy("", TypeQuestion, PolicyFields{MaxReminders: 1, RequesterEnded: EndedNextLaunch, HoldMinutes: 30, RatePerRunHour: 30, RatePerSpaceHour: 300})
	inst := ids.New(ids.Instance)
	f.run("run_k1", "worker", "coder", inst, "running", "running")
	req := Requester{Kind: "agent", ID: inst, Role: "coder", Run: "run_k1"}
	a := f.create(CreateInput{Type: TypeQuestion, Title: "a", Requester: req})
	b := f.create(CreateInput{Type: TypeQuestion, Title: "b", Requester: Requester{Kind: "agent", ID: ids.New(ids.Instance), Role: "tester", Run: "run_k1"}})
	f.end("run_k1")
	f.exec(`INSERT INTO role_templates (id, slug, name, kind, created_at, created_by, updated_at) VALUES ('rtp_w', 'worker', 'W', 'worker', 0, 't', 0)`,
		`INSERT INTO space_roles (space_id, name, template_id, created_at, created_by, updated_at) VALUES ('spc_a', 'implementor', 'rtp_w', 0, 't', 0),
		 ('spc_a', 'tester', 'rtp_w', 0, 't', 0)`,
		`INSERT INTO space_role_renames (id, space_id, from_name, to_name, renamed_at, renamed_by, node_state) VALUES
		 ('rrn_1', 'spc_a', 'coder', 'implementor', 1, 't', 'done'), ('rrn_2', 'spc_a', 'tester', 'implementor', 0, 't', 'done')`)
	if n := f.answer(a).Notifications[0]; n.TargetKind != TargetInstance || n.TargetRole != "implementor" {
		t.Fatalf("held for the instance %+v, want target role implementor", n)
	}
	if n := f.answer(b).Notifications[0]; n.TargetRole != "tester" {
		t.Fatalf("a live role's name was redirected: %+v", n)
	}
}

// The operators route (no agent can take the response) labels the notification with the requester's current role name.
func TestRequesterRoleLabelOnOperatorsRoute(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.policy("", TypeQuestion, PolicyFields{MaxReminders: 1, RequesterEnded: EndedReroute, HoldMinutes: 30, RatePerRunHour: 30, RatePerSpaceHour: 300})
	inst := ids.New(ids.Instance)
	f.run("run_k1", "worker", "coder", inst, "running", "running")
	a := f.create(CreateInput{Type: TypeQuestion, Title: "a", Requester: Requester{Kind: "agent", ID: inst, Role: "coder", Run: "run_k1"}})
	f.end("run_k1")
	f.exec(`INSERT INTO role_templates (id, slug, name, kind, created_at, created_by, updated_at) VALUES ('rtp_w', 'worker', 'W', 'worker', 0, 't', 0)`,
		`INSERT INTO space_roles (space_id, name, template_id, created_at, created_by, updated_at) VALUES ('spc_a', 'implementor', 'rtp_w', 0, 't', 0)`,
		`INSERT INTO space_role_renames (id, space_id, from_name, to_name, renamed_at, renamed_by, node_state) VALUES
		 ('rrn_1', 'spc_a', 'coder', 'implementor', 1, 't', 'done')`)
	if n := f.answer(a).Notifications[0]; n.TargetKind != TargetOperators || n.TargetRole != "implementor" {
		t.Fatalf("notification %+v, want the operators route labelled implementor", n)
	}
}
