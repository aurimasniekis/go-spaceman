package human

import (
	"encoding/json"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
)

// SM-HUI-008 (ADR-0050): type permissions are stored per space and type (built-in or custom, by ID or slug), created
// once and then updated with the version, validated, audited and removable; agents of roles a permission leaves out
// are refused when they ask.
func TestTypePermissions(t *testing.T) {
	f := newFx(t)
	ht, err := f.svc.CreateType(f.ctx, op, "deploy-window", "Deploy window", "", TypeDraft{})
	must(t, err)
	if p, err := f.svc.TypePermission(f.ctx, "spc_a", "approval"); err != nil || p.Stored || p.ViewRole != RoleViewer || p.Agents != AgentsAny {
		t.Fatalf("defaults %+v %v", p, err)
	}
	p, err := f.svc.SetTypePermission(f.ctx, op, "spc_a", "approval", 0, TypePermissionInput{ViewRole: RoleOperator, RespondRole: RoleAdmin,
		Agents: AgentsRoles, AgentRoles: []string{"reviewer", "deployer", "reviewer"}})
	must(t, err)
	if !p.Stored || p.Version != 1 || p.CreateRole != RoleOperator || len(p.AgentRoles) != 2 || p.AgentRoles[0] != "deployer" {
		t.Fatalf("stored %+v", p)
	}
	_, err = f.svc.SetTypePermission(f.ctx, op, "spc_a", "approval", 0, TypePermissionInput{})
	code(t, err, apperr.CodeAlreadyExists)
	_, err = f.svc.SetTypePermission(f.ctx, op, "spc_a", "approval", 7, TypePermissionInput{})
	code(t, err, apperr.CodePrecondition)
	for _, bad := range []TypePermissionInput{{ViewRole: "root"}, {RespondRole: RoleViewer}, {Agents: AgentsRoles}, {AgentRoles: []string{"x"}},
		{Agents: AgentsRoles, AgentRoles: []string{"Bad Role"}}} {
		_, err = f.svc.SetTypePermission(f.ctx, op, "spc_a", "todo", 0, bad)
		code(t, err, apperr.CodeInvalid)
	}
	_, err = f.svc.SetTypePermission(f.ctx, op, "spc_a", "nope", 0, TypePermissionInput{})
	code(t, err, apperr.CodeNotFound)
	// custom types by slug or ID name the same permission
	c, err := f.svc.SetTypePermission(f.ctx, op, "spc_a", "deploy-window", 0, TypePermissionInput{Agents: AgentsNone})
	must(t, err)
	if c.ActionType != ht.ID || c.TypeSlug != "deploy-window" {
		t.Fatalf("custom %+v", c)
	}
	if g, err := f.svc.TypePermission(f.ctx, "spc_a", ht.ID); err != nil || !g.Stored || g.Agents != AgentsNone {
		t.Fatalf("by ID %+v %v", g, err)
	}
	// agents: a listed role may ask, others are refused; another space is unaffected
	ask := func(space, role string) error {
		_, _, err := f.svc.Create(f.ctx, op, space, CreateInput{Type: TypeApproval, Title: "Deploy?", Request: json.RawMessage(`{"action":"deploy"}`),
			Requester: Requester{Kind: "agent", ID: "ins_1", Role: role, Run: "run_1", Label: role}})
		return err
	}
	code(t, ask("spc_a", "worker"), apperr.CodeForbidden)
	must(t, ask("spc_a", "deployer"))
	must(t, ask("spc_b", "worker"))
	// operators and the system are not agents: their requests pass the service (the API checks people's roles)
	_, _, err = f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeApproval, Title: "Op asks", Request: json.RawMessage(`{"action":"x"}`),
		Requester: Requester{Kind: "operator", ID: "usr_op", Label: "Op"}})
	must(t, err)
	p, err = f.svc.SetTypePermission(f.ctx, op, "spc_a", "approval", p.Version, TypePermissionInput{ViewRole: RoleAdmin})
	must(t, err)
	if p.Version != 2 || p.Agents != AgentsAny {
		t.Fatalf("updated %+v", p)
	}
	must(t, ask("spc_a", "worker"))
	l, err := f.svc.TypePermissions(f.ctx, "spc_a")
	if err != nil || len(l) != 2 {
		t.Fatalf("list %+v %v", l, err)
	}
	ps, err := f.svc.Permissions(f.ctx, "spc_a")
	must(t, err)
	if ps.For(&Action{SpaceID: "spc_a", Type: TypeCustom, TypeSlug: "deploy-window"}).Agents != AgentsNone || ps.For(&Action{Type: TypeTodo}).Stored {
		t.Fatalf("permission set %+v", ps)
	}
	must(t, f.svc.DeleteTypePermission(f.ctx, op, "spc_a", "approval"))
	code(t, f.svc.DeleteTypePermission(f.ctx, op, "spc_a", "approval"), apperr.CodeNotFound)
	var n int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action LIKE 'human_type.permission_%'`).Scan(&n))
	if n != 4 {
		t.Fatalf("%d permission audit records, want 4", n)
	}
}
