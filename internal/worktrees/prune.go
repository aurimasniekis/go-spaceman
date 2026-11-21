package worktrees

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
)

// PruneAnswerWait is how long PruneSpace waits, in total, for the node's answers to one space's removals; an answer
// that has not arrived is counted as pending and its record stays in the audit log (ADR-0107 decision 4: the daily pass
// does not block on a slow node).
const PruneAnswerWait = 30 * time.Second

// PruneSummary is what PruneSpace did in one space.
type PruneSummary struct {
	Candidates int    `json:"candidates"` // eligible worktrees the rule found (before the batch cap)
	Requested  int    `json:"requested"`  // removals asked of the node
	Removed    int    `json:"removed"`
	Refused    int    `json:"refused"`           // the node (or central: a live run) refused; the worktree stays
	Pending    int    `json:"pending"`           // not answered within PruneAnswerWait
	Failed     int    `json:"failed"`            // the node could not tell, or the request failed
	Skipped    string `json:"skipped,omitempty"` // why nothing was tried (old node, not connected, relocating)
}

// PruneSpace applies the retention rule of ADR-0107 decision 4 to one space: it asks the node to remove up to limit
// worktrees (oldest finished first) whose owning task is done or cancelled and whose owning run ended at least days ago.
// Every removal is a Request, so a live run is refused here and the node checks again; nothing is forced. A space whose
// node cannot be asked (protocol below 16, not connected, relocating) is skipped without an error. days or limit of 0
// or less does nothing.
func (svc *Service) PruneSpace(ctx context.Context, actor audit.Actor, spaceID string, days int64, limit int) (PruneSummary, error) {
	var sum PruneSummary
	if days <= 0 || limit <= 0 {
		return sum, nil
	}
	cands, err := svc.Candidates(ctx, spaceID, "", int(days), 0)
	if err != nil {
		return sum, err
	}
	sum.Candidates = len(cands)
	if len(cands) > limit {
		cands = cands[:limit]
	}
	var pending []*Outcome
	var errs []error
	for _, e := range cands {
		o, err := svc.Request(ctx, actor, spaceID, e.Repo, e.Name)
		if err != nil {
			if apperr.CodeOf(err) == apperr.CodePrecondition && len(pending) == 0 && sum.Failed == 0 {
				sum.Skipped = err.Error()
				return sum, nil // the node cannot be asked: every other candidate would be refused the same way
			}
			sum.Failed++
			if apperr.CodeOf(err) != apperr.CodeNotFound {
				errs = append(errs, err)
			}
			continue
		}
		sum.Requested++
		pending = append(pending, o)
	}
	wait := PruneAnswerWait
	if svc.PruneWait > 0 {
		wait = svc.PruneWait
	}
	deadline := svc.clk.Now().Add(wait)
	for _, o := range pending {
		if o.Status == StatusPending {
			if got, err := svc.Await(ctx, spaceID, o, max(deadline.Sub(svc.clk.Now()), time.Millisecond)); err == nil {
				o = got
			}
		}
		switch o.Status {
		case StatusRemoved:
			sum.Removed++
		case StatusRefused:
			sum.Refused++
		case StatusError:
			sum.Failed++
		default:
			sum.Pending++
		}
	}
	svc.log.Info("worktree retention", slog.String(obs.FieldSpaceID, spaceID), slog.Int("candidates", sum.Candidates), slog.Int("requested", sum.Requested),
		slog.Int("removed", sum.Removed), slog.Int("refused", sum.Refused), slog.Int("pending", sum.Pending), slog.Int("failed", sum.Failed))
	return sum, errors.Join(errs...)
}
