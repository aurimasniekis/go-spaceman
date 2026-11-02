package dream

import (
	"context"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
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
