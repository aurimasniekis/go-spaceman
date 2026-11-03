package human

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Type permissions (SM-HUI-008, ADR-0050): per space and action type, the weakest built-in role that may see, create
// and respond to actions of the type, and which agents may ask for one. They narrow, never widen, the role
// permissions (human_action:read and :respond, approval:decide). A type without a stored permission uses the defaults.

// Roles a type permission names (the built-in roles, weakest first).
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"

	AgentsAny   = "any"
	AgentsRoles = "roles"
	AgentsNone  = "none"
)

// TypePermission says who may see, create and respond to one action type in a space.
type TypePermission struct {
	SpaceID     string    `json:"space_id"`
	ActionType  string    `json:"action_type" doc:"question, approval, todo, message, or a custom type's ID"`
	TypeSlug    string    `json:"type_slug,omitempty" doc:"A custom type's slug"`
	ViewRole    string    `json:"view_role" enum:"viewer,operator,admin" doc:"Weakest role that sees actions of this type (default viewer)"`
	CreateRole  string    `json:"create_role" enum:"operator,admin" doc:"Weakest role that may ask people with this type (default operator)"`
	RespondRole string    `json:"respond_role" enum:"operator,admin" doc:"Weakest role that may respond, assign, comment, clarify, move or cancel (default operator)"`
	Agents      string    `json:"agents" enum:"any,roles,none" doc:"Which agents may ask with this type: any (default), only agent_roles, or none"`
	AgentRoles  []string  `json:"agent_roles" doc:"Space roles whose agents may ask when agents is roles"`
	Stored      bool      `json:"stored" doc:"false: no permission is stored and the defaults apply"`
	Version     int64     `json:"version"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
}

// TypePermissionInput is the editable part of a type permission. Empty fields take the defaults.
type TypePermissionInput struct {
	ViewRole    string   `json:"view_role,omitempty" enum:"viewer,operator,admin"`
	CreateRole  string   `json:"create_role,omitempty" enum:"operator,admin"`
	RespondRole string   `json:"respond_role,omitempty" enum:"operator,admin"`
	Agents      string   `json:"agents,omitempty" enum:"any,roles,none"`
	AgentRoles  []string `json:"agent_roles,omitempty" maxItems:"50"`
}

var reAgentRole = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

// BuiltinTypes are the built-in action types.
var BuiltinTypes = []string{TypeQuestion, TypeApproval, TypeTodo, TypeMessage}

// DefaultTypePermission is the permission of a type that has none stored.
func DefaultTypePermission(spaceID, actionType, slug string) TypePermission {
	return TypePermission{SpaceID: spaceID, ActionType: actionType, TypeSlug: slug, ViewRole: RoleViewer, CreateRole: RoleOperator,
		RespondRole: RoleOperator, Agents: AgentsAny, AgentRoles: []string{}}
}

func (in *TypePermissionInput) normalize() error {
	v := &apperr.Validation{}
	if in.ViewRole == "" {
		in.ViewRole = RoleViewer
	}
	if !slices.Contains([]string{RoleViewer, RoleOperator, RoleAdmin}, in.ViewRole) {
		v.Add("view_role", "must be viewer, operator or admin")
	}
	if in.CreateRole == "" {
		in.CreateRole = RoleOperator
	}
	if in.CreateRole != RoleOperator && in.CreateRole != RoleAdmin {
		v.Add("create_role", "must be operator or admin")
	}
	if in.RespondRole == "" {
		in.RespondRole = RoleOperator
	}
	if in.RespondRole != RoleOperator && in.RespondRole != RoleAdmin {
		v.Add("respond_role", "must be operator or admin")
	}
	if in.Agents == "" {
		in.Agents = AgentsAny
	}
	if !slices.Contains([]string{AgentsAny, AgentsRoles, AgentsNone}, in.Agents) {
		v.Add("agents", "must be any, roles or none")
	}
	roles := []string{}
	for _, r := range in.AgentRoles {
		if !reAgentRole.MatchString(r) {
			v.Add("agent_roles", "%q is not a role name", r)
		} else if !slices.Contains(roles, r) {
			roles = append(roles, r)
		}
	}
	if len(roles) > 50 {
		v.Add("agent_roles", "at most 50 roles")
	}
	if in.Agents != AgentsRoles && len(roles) > 0 {
		v.Add("agent_roles", "only with agents = roles")
	}
	if in.Agents == AgentsRoles && len(roles) == 0 {
		v.Add("agent_roles", "agents = roles needs at least one role (or use none)")
	}
	slices.Sort(roles)
	in.AgentRoles = roles
	return v.Err()
}

// resolveActionType returns the stored key and slug of a type reference: a built-in type, or a custom type's ID or slug.
func resolveActionType(ctx context.Context, q *db.Queries, ref string) (string, string, error) {
	if slices.Contains(BuiltinTypes, ref) {
		return ref, "", nil
	}
	if ref == "" || ref == TypeCustom {
		return "", "", apperr.New(apperr.CodeInvalid, "name a built-in type (question, approval, todo, message) or a custom type's ID or slug")
	}
	var t db.HumanType
	var err error
	if strings.HasPrefix(ref, ids.HumanType+"_") {
		t, err = q.GetHumanType(ctx, ref)
	} else {
		t, err = q.GetHumanTypeBySlug(ctx, ref)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", apperr.New(apperr.CodeNotFound, "human-action type %q not found", ref)
	} else if err != nil {
		return "", "", err
	}
	return t.ID, t.Slug, nil
}

func permFromRow(r db.SpaceHumanTypePermission, slug string) TypePermission {
	p := TypePermission{SpaceID: r.SpaceID, ActionType: r.ActionType, TypeSlug: slug, ViewRole: r.ViewRole, CreateRole: r.CreateRole,
		RespondRole: r.RespondRole, Agents: r.Agents, AgentRoles: []string{}, Stored: true, Version: r.Version,
		UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), UpdatedBy: r.UpdatedBy}
	_ = json.Unmarshal([]byte(r.AgentRolesJson), &p.AgentRoles)
	return p
}

// TypePermissions returns a space's stored type permissions.
func (svc *Service) TypePermissions(ctx context.Context, spaceID string) ([]TypePermission, error) {
	return typePermissions(ctx, svc.s.R(), spaceID)
}

func typePermissions(ctx context.Context, q *db.Queries, spaceID string) ([]TypePermission, error) {
	rows, err := q.ListSpaceHumanTypePermissions(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]TypePermission, 0, len(rows))
	for _, r := range rows {
		slug := ""
		if !slices.Contains(BuiltinTypes, r.ActionType) {
			if t, err := q.GetHumanType(ctx, r.ActionType); err == nil {
				slug = t.Slug
			}
		}
		out = append(out, permFromRow(r, slug))
	}
	return out, nil
}

// TypePermission returns the permission in force for a type reference in a space (stored, or the defaults).
func (svc *Service) TypePermission(ctx context.Context, spaceID, ref string) (TypePermission, error) {
	q := svc.s.R()
	key, slug, err := resolveActionType(ctx, q, ref)
	if err != nil {
		return TypePermission{}, err
	}
	r, err := q.GetSpaceHumanTypePermission(ctx, db.GetSpaceHumanTypePermissionParams{SpaceID: spaceID, ActionType: key})
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultTypePermission(spaceID, key, slug), nil
	} else if err != nil {
		return TypePermission{}, err
	}
	return permFromRow(r, slug), nil
}

// SetTypePermission stores a type's permission. Version 0 creates it and is refused when one exists (import it
// instead); otherwise the version must match.
func (svc *Service) SetTypePermission(ctx context.Context, actor audit.Actor, spaceID, ref string, version int64, in TypePermissionInput) (TypePermission, error) {
	if err := in.normalize(); err != nil {
		return TypePermission{}, err
	}
	roles, _ := json.Marshal(in.AgentRoles)
	var key, slug string
	err := svc.s.W().TxNamed(ctx, "human.type_permission_set", func(tx *store.Tx) error {
		if _, err := tx.Q.GetSpace(ctx, spaceID); errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "space not found")
		} else if err != nil {
			return err
		}
		var err error
		if key, slug, err = resolveActionType(ctx, tx.Q, ref); err != nil {
			return err
		}
		cur, err := tx.Q.GetSpaceHumanTypePermission(ctx, db.GetSpaceHumanTypePermissionParams{SpaceID: spaceID, ActionType: key})
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		details := map[string]any{"space_id": spaceID, "action_type": key, "view_role": in.ViewRole, "create_role": in.CreateRole,
			"respond_role": in.RespondRole, "agents": in.Agents, "agent_roles": in.AgentRoles}
		switch {
		case version == 0 && exists:
			return apperr.New(apperr.CodeAlreadyExists, "a permission for %s exists in this space (version %d); update it with If-Match", ref, cur.Version)
		case version == 0:
			if err := tx.Q.InsertSpaceHumanTypePermission(ctx, db.InsertSpaceHumanTypePermissionParams{SpaceID: spaceID, ActionType: key, ViewRole: in.ViewRole,
				CreateRole: in.CreateRole, RespondRole: in.RespondRole, Agents: in.Agents, AgentRolesJson: string(roles), UpdatedAt: tx.NowMicros(),
				UpdatedBy: actor.ID}); err != nil {
				return err
			}
		case !exists:
			return apperr.New(apperr.CodeNotFound, "no permission for %s is stored in this space", ref)
		default:
			n, err := tx.Q.UpdateSpaceHumanTypePermission(ctx, db.UpdateSpaceHumanTypePermissionParams{ViewRole: in.ViewRole, CreateRole: in.CreateRole,
				RespondRole: in.RespondRole, Agents: in.Agents, AgentRolesJson: string(roles), Now: tx.NowMicros(), By: actor.ID, SpaceID: spaceID,
				ActionType: key, Version: version})
			if err != nil {
				return err
			}
			if n == 0 {
				return apperr.New(apperr.CodePrecondition, "the permission changed since version %d (now %d)", version, cur.Version)
			}
			details["before"] = map[string]any{"view_role": cur.ViewRole, "create_role": cur.CreateRole, "respond_role": cur.RespondRole,
				"agents": cur.Agents, "agent_roles": json.RawMessage(cur.AgentRolesJson)}
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space", spaceID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "human_type.permission_set", Target: audit.Target{Type: "space", ID: spaceID},
			Details: details})
		return err
	})
	if err != nil {
		return TypePermission{}, err
	}
	r, err := svc.s.R().GetSpaceHumanTypePermission(ctx, db.GetSpaceHumanTypePermissionParams{SpaceID: spaceID, ActionType: key})
	if err != nil {
		return TypePermission{}, err
	}
	return permFromRow(r, slug), nil
}

// DeleteTypePermission removes a type's stored permission: the defaults apply again.
func (svc *Service) DeleteTypePermission(ctx context.Context, actor audit.Actor, spaceID, ref string) error {
	return svc.s.W().TxNamed(ctx, "human.type_permission_delete", func(tx *store.Tx) error {
		key, _, err := resolveActionType(ctx, tx.Q, ref)
		if err != nil {
			return err
		}
		n, err := tx.Q.DeleteSpaceHumanTypePermission(ctx, db.DeleteSpaceHumanTypePermissionParams{SpaceID: spaceID, ActionType: key})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeNotFound, "no permission for %s is stored in this space", ref)
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space", spaceID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "human_type.permission_deleted", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"space_id": spaceID, "action_type": key}})
		return err
	})
}

// PermissionSet is a space's type permissions, keyed for actions: built-in type names and custom type slugs.
type PermissionSet map[string]TypePermission

// For returns the permission that applies to an action.
func (ps PermissionSet) For(a *Action) TypePermission {
	key := a.Type
	if a.Type == TypeCustom {
		key = "custom:" + a.TypeSlug
	}
	if p, ok := ps[key]; ok {
		return p
	}
	return DefaultTypePermission(a.SpaceID, a.Type, a.TypeSlug)
}

// Permissions returns a space's permission set for checking actions.
func (svc *Service) Permissions(ctx context.Context, spaceID string) (PermissionSet, error) {
	return permissionSet(ctx, svc.s.R(), spaceID)
}

func permissionSet(ctx context.Context, q *db.Queries, spaceID string) (PermissionSet, error) {
	l, err := typePermissions(ctx, q, spaceID)
	if err != nil {
		return nil, err
	}
	ps := PermissionSet{}
	for _, p := range l {
		if p.TypeSlug != "" {
			ps["custom:"+p.TypeSlug] = p
		} else {
			ps[p.ActionType] = p
		}
	}
	return ps, nil
}

// agentMayAsk reports whether an agent of role may create an action of the type.
func (p TypePermission) agentMayAsk(role string) bool {
	switch p.Agents {
	case AgentsNone:
		return false
	case AgentsRoles:
		return slices.Contains(p.AgentRoles, role)
	default:
		return true
	}
}

// checkAgentRequester refuses an agent's request of a type the space does not let its role ask with.
func checkAgentRequester(ctx context.Context, q *db.Queries, spaceID string, in *CreateInput) error {
	if in.Requester.Kind != "agent" {
		return nil
	}
	ps, err := permissionSet(ctx, q, spaceID)
	if err != nil {
		return err
	}
	a := &Action{SpaceID: spaceID, Type: in.Type, TypeSlug: in.TypeSlug}
	if p := ps.For(a); !p.agentMayAsk(in.Requester.Role) {
		name := in.Type
		if in.Type == TypeCustom {
			name = in.TypeSlug
		}
		return apperr.New(apperr.CodeForbidden, "agents of role %q may not ask with type %s in this space", in.Requester.Role, name)
	}
	return nil
}
