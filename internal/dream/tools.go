package dream

import (
	"context"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// Tools registers the dreamer's report tool. Everything else a dream publishes goes through the ordinary tools:
// messages to control, human actions, KB questions (SM-DREAM-004).
func Tools(svc *Service) func(r *agenttools.Registry) {
	return func(r *agenttools.Registry) {
		agenttools.Add(r, "spaceman_dream_report", "Finish this dream: a summary, the memory files you changed, and findings. Report no_change when "+
			"nothing needed changing (a successful dream). SpaceMan then checkpoints your memory changes and advances the evidence you were given.",
			[]string{agenttools.KindDreamer}, false, func(ctx context.Context, p *fleet.RunPrincipal, in ReportInput) (any, error) {
				return svc.Report(ctx, p, in)
			})
	}
}

// MemoryRolesOfRun returns the memory roles of the dream configuration a dreamer run belongs to ("*": every role).
func (svc *Service) MemoryRolesOfRun(ctx context.Context, runID string) ([]string, error) {
	dr, err := svc.s.R().GetDreamRunByRun(ctx, runID)
	if err != nil {
		return nil, apperr.New(apperr.CodeForbidden, "this run is not a dream")
	}
	c, err := svc.s.R().GetDreamConfig(ctx, dr.ConfigID)
	if err != nil {
		return nil, errNotFound
	}
	return parseList(c.MemoryRolesJson), nil
}
