package memory

import (
	"context"
	"errors"
	"slices"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// Tools registers the agents' memory tool. Agents read and write their memory as files in $SPACEMAN_MEMORY_DIR; the
// tool reports what central has preserved and what is still pending.
//
// Control and the dreamer also get spaceman_memory_conflict_list and spaceman_memory_conflict_resolve. Control reaches
// every role of its space; a dreamer only the roles its dream configuration covers (dreamRoles answers that for a run;
// nil allows none). Other runs never see them.
func Tools(svc *Service, dreamRoles func(ctx context.Context, runID string) ([]string, error)) func(r *agenttools.Registry) {
	all := []string{agenttools.KindControl, agenttools.KindWorker, agenttools.KindKB, agenttools.KindDreamer, agenttools.KindDiagnostic}
	return func(r *agenttools.Registry) {
		agenttools.Add(r, "spaceman_memory_status", "Your role memory's sync state: the history central has preserved, what the node has not synced "+
			"yet, and conflicts that need reconciling. Your memory files themselves are in $SPACEMAN_MEMORY_DIR.", all, true,
			func(ctx context.Context, p *fleet.RunPrincipal, _ struct{}) (any, error) {
				c, err := svc.Get(ctx, p.SpaceID, p.Role)
				var ae *apperr.Error
				if errors.As(err, &ae) && ae.Code == apperr.CodeNotFound {
					return map[string]any{"role": p.Role, "preserved": false, "note": "nothing has been synced to central yet"}, nil
				}
				if err != nil {
					return nil, err
				}
				conflicts, _ := svc.Conflicts(ctx, p.SpaceID, p.Role)
				open := []Conflict{}
				for _, x := range conflicts {
					if x.ResolvedAt == nil {
						open = append(open, x)
					}
				}
				return map[string]any{"collection": c, "open_conflicts": open}, nil
			})

		agenttools.Add(r, "spaceman_memory_conflict_list", "List the open memory conflicts of the roles you may reconcile in this space (control: every role; "+
			"a dreamer: the roles its dream covers), or of one role. Close one you reconciled with spaceman_memory_conflict_resolve.", reconcilers, true,
			func(ctx context.Context, p *fleet.RunPrincipal, in conflictListIn) (any, error) {
				roles, err := reachable(ctx, svc, p, dreamRoles, in.Role)
				if err != nil {
					return nil, err
				}
				out := []conflictRow{}
				for _, role := range roles {
					cs, err := svc.Conflicts(ctx, p.SpaceID, role)
					if err != nil {
						continue // no memory for the role yet
					}
					for _, c := range cs {
						if c.ResolvedAt == nil {
							out = append(out, conflictRow{Role: role, Conflict: c})
						}
					}
				}
				return map[string]any{"open_conflicts": out}, nil
			})
		agenttools.Add(r, "spaceman_memory_conflict_resolve", "Mark one memory conflict reconciled once you merged what still matters into the shared "+
			"files and deleted its conflicts/ folder. The preserved history stays; this only closes the record, and it is audited as you. Control may "+
			"close any role's conflict in this space; a dreamer only the roles its dream covers.", reconcilers, false,
			func(ctx context.Context, p *fleet.RunPrincipal, in conflictResolveIn) (any, error) {
				if in.Role == "" || in.ConflictID == "" {
					return nil, apperr.New(apperr.CodeInvalid, "role and conflict_id are required")
				}
				if _, err := reachable(ctx, svc, p, dreamRoles, in.Role); err != nil {
					return nil, err
				}
				if err := svc.ResolveConflict(ctx, audit.Actor{Type: audit.ActorAgent, ID: p.RunID}, p.SpaceID, in.Role, in.ConflictID); err != nil {
					return nil, err
				}
				return map[string]any{"role": in.Role, "conflict": in.ConflictID, "resolved": true}, nil
			})
	}
}

var reconcilers = []string{agenttools.KindControl, agenttools.KindDreamer}

type conflictListIn struct {
	Role string `json:"role,omitempty" jsonschema:"only this role's conflicts"`
}

type conflictResolveIn struct {
	Role       string `json:"role" jsonschema:"the role whose memory the conflict belongs to"`
	ConflictID string `json:"conflict_id" jsonschema:"the conflict's id from spaceman_memory_conflict_list"`
}

type conflictRow struct {
	Role string `json:"role"`
	Conflict
}

// reachable returns the roles the run may reconcile (just role when one is named and allowed).
func reachable(ctx context.Context, svc *Service, p *fleet.RunPrincipal, dreamRoles func(context.Context, string) ([]string, error), role string) ([]string, error) {
	var covered []string // nil: every role of the space
	switch p.Kind {
	case agenttools.KindControl:
	case agenttools.KindDreamer:
		if dreamRoles == nil {
			return nil, apperr.New(apperr.CodeForbidden, "this run has no dream configuration")
		}
		rs, err := dreamRoles(ctx, p.RunID)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(rs, "*") {
			covered = rs
			if covered == nil {
				covered = []string{}
			}
		}
	default:
		return nil, apperr.New(apperr.CodeForbidden, "%s runs may not reconcile memory conflicts", p.Kind)
	}
	if role != "" {
		if covered != nil && !slices.Contains(covered, role) {
			return nil, apperr.New(apperr.CodeForbidden, "the dream does not cover the memory of role %q", role)
		}
		return []string{role}, nil
	}
	if covered != nil {
		return covered, nil
	}
	cols, err := svc.List(ctx, p.SpaceID)
	if err != nil {
		return nil, err
	}
	roles := make([]string, 0, len(cols))
	for _, c := range cols {
		roles = append(roles, c.Role)
	}
	return roles, nil
}
