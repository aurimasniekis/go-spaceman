package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/human"
)

// Human-action type permissions (SM-HUI-008, ADR-0050): per space and type, the weakest role that sees, creates and
// responds to actions of the type. The route rules check the role permissions first; these checks only narrow them.

// HumanTypePermissionRules are the authorization rules of the type permission operations. Changing who may see or
// answer a type is authorization, so it needs human_type_permission:write (admins), not space_config:write.
var HumanTypePermissionRules = map[string]Rule{
	"list-human-type-permissions":  InSpace(authz.SpaceConfigRead),
	"get-human-type-permission":    InSpace(authz.SpaceConfigRead),
	"set-human-type-permission":    InSpace(authz.HumanTypePermissionWrite),
	"delete-human-type-permission": InSpace(authz.HumanTypePermissionWrite),
}

// Type permission actions.
const (
	humanView    = "view"
	humanCreate  = "create"
	humanRespond = "respond"
)

// humanAccess answers type-permission questions for one request's principal, loading each space's set once.
type humanAccess struct {
	d    *Deps
	svc  *human.Service
	subj []string
	sets map[string]human.PermissionSet
}

func (d *Deps) humanAccess(ctx context.Context, svc *human.Service) *humanAccess {
	return &humanAccess{d: d, svc: svc, subj: PrincipalFrom(ctx).Subjects(), sets: map[string]human.PermissionSet{}}
}

// may reports whether the principal may act on actions like a (its space, type and slug). Errors deny.
func (h *humanAccess) may(ctx context.Context, a *human.Action, what string) bool {
	ps, ok := h.sets[a.SpaceID]
	if !ok {
		var err error
		if ps, err = h.svc.Permissions(ctx, a.SpaceID); err != nil {
			return false
		}
		h.sets[a.SpaceID] = ps
	}
	p := ps.For(a)
	role := p.ViewRole
	switch what {
	case humanCreate:
		role = p.CreateRole
	case humanRespond:
		role = p.RespondRole
	}
	if role == human.RoleViewer {
		return true // the route's human_action:read already holds
	}
	return h.d.Authz != nil && h.d.Authz.HoldsRole(h.subj, a.SpaceID, authz.Role(role))
}

func (h *humanAccess) visible(ctx context.Context) func(a *human.Action) bool {
	return func(a *human.Action) bool { return h.may(ctx, a, humanView) }
}

// action returns a human action the principal may see and, for respond, act on: a hidden one is not found (as a
// space the caller cannot read is), a visible one the caller may not answer is forbidden.
func (h *humanAccess) action(ctx context.Context, spaceID, id, what string) (*human.Action, error) {
	a, err := h.svc.Get(ctx, spaceID, id)
	if err != nil {
		return nil, err
	}
	if !h.may(ctx, a, humanView) {
		return nil, apperr.New(apperr.CodeNotFound, "human action not found")
	}
	if what == humanRespond && !h.may(ctx, a, humanRespond) {
		p := h.sets[spaceID].For(a)
		return nil, apperr.New(apperr.CodeForbidden, "acting on %s actions in this space needs the %s role", typeName(a), p.RespondRole)
	}
	return a, nil
}

func typeName(a *human.Action) string {
	if a.Type == human.TypeCustom {
		return a.TypeSlug
	}
	return a.Type
}

// HumanTypePermissions lists a space's stored type permissions.
type HumanTypePermissions struct {
	Items []human.TypePermission `json:"items" doc:"Stored permissions; types without one use the defaults (viewers see, operators create and respond, any agent asks)"`
}

// HumanTypePermissionPath names a type: a built-in type, or a custom type's ID or slug.
type HumanTypePermissionPath struct {
	SpaceID    string `path:"space_id" pattern:"^spc_[0-9a-z]{26}$"`
	ActionType string `path:"action_type" maxLength:"64" doc:"question, approval, todo, message, or a custom type's ID or slug"`
}

type humanPermOut struct {
	ETag string `header:"ETag"`
	Body human.TypePermission
}

func registerHumanTypePermissions(api huma.API, d *Deps, svc *human.Service) {
	tags := []string{"human-action-types"}
	huma.Register(api, huma.Operation{OperationID: "list-human-type-permissions", Method: http.MethodGet, Path: "/spaces/{space_id}/human-type-permissions",
		Summary: "Who may see, create and respond to each human-action type in the space", Tags: tags},
		func(ctx context.Context, in *SpacePath) (*struct{ Body HumanTypePermissions }, error) {
			l, err := svc.TypePermissions(ctx, in.SpaceID)
			if err != nil {
				return nil, d.fail(ctx, "human_perms.list", err)
			}
			return &struct{ Body HumanTypePermissions }{Body: HumanTypePermissions{Items: l}}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "get-human-type-permission", Method: http.MethodGet,
		Path: "/spaces/{space_id}/human-type-permissions/{action_type}", Summary: "The permission in force for a type (stored: false means the defaults)",
		Tags: tags},
		func(ctx context.Context, in *HumanTypePermissionPath) (*humanPermOut, error) {
			p, err := svc.TypePermission(ctx, in.SpaceID, in.ActionType)
			if err != nil {
				return nil, d.fail(ctx, "human_perms.get", err)
			}
			return &humanPermOut{ETag: ETag(p.Version), Body: p}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "set-human-type-permission", Method: http.MethodPut,
		Path: "/spaces/{space_id}/human-type-permissions/{action_type}", Summary: "Set who may see, create and respond to a type in the space",
		Description: "Without If-Match the permission is created and an existing one is refused; with If-Match it is updated at that version. " +
			"Permissions narrow the roles' own permissions and are audited.", Tags: tags},
		func(ctx context.Context, in *struct {
			HumanTypePermissionPath
			IfMatch
			Body human.TypePermissionInput
		}) (*humanPermOut, error) {
			v, err := optionalVersion(in.IfMatch)
			if err != nil {
				return nil, d.fail(ctx, "human_perms.set", err)
			}
			p, err := svc.SetTypePermission(ctx, Actor(ctx), in.SpaceID, in.ActionType, v, in.Body)
			if err != nil {
				return nil, d.fail(ctx, "human_perms.set", err)
			}
			return &humanPermOut{ETag: ETag(p.Version), Body: p}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "delete-human-type-permission", Method: http.MethodDelete,
		Path: "/spaces/{space_id}/human-type-permissions/{action_type}", Summary: "Remove a type's permission (the defaults apply again)", Tags: tags,
		DefaultStatus: http.StatusNoContent},
		func(ctx context.Context, in *HumanTypePermissionPath) (*struct{}, error) {
			if err := svc.DeleteTypePermission(ctx, Actor(ctx), in.SpaceID, in.ActionType); err != nil {
				return nil, d.fail(ctx, "human_perms.delete", err)
			}
			return &struct{}{}, nil
		})
}
