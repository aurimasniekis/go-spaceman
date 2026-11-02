package memory

import (
	"context"
	"errors"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// Tools registers the agents' memory tool. Agents read and write their memory as files in $SPACEMAN_MEMORY_DIR; the
// tool reports what central has preserved and what is still pending.
func Tools(svc *Service) func(r *agenttools.Registry) {
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
	}
}
