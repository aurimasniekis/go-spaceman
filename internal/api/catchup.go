package api

import (
	"context"
	"net/http"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
)

// acquireCatchUp takes one of central's slots for reading a stored recording (Deps.CatchUps), waiting up to
// CatchUpWait: each read decompresses a run's recording, so many viewers opening long terminals at once would
// otherwise all do it at the same time (ADR-0081). ok is false when no slot came free in time or ctx ended; release
// gives the slot back.
func (d *Deps) acquireCatchUp(ctx context.Context) (release func(), ok bool) {
	if d.CatchUps == nil {
		return func() {}, true
	}
	release = func() { <-d.CatchUps }
	select {
	case d.CatchUps <- struct{}{}:
		return release, true
	default:
	}
	if d.CatchUpWait <= 0 {
		return nil, false
	}
	t := time.NewTimer(d.CatchUpWait)
	defer t.Stop()
	select {
	case d.CatchUps <- struct{}{}:
		return release, true
	case <-t.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// catchUpBusy is the answer when no catch-up slot came free in time: the client retries.
func catchUpBusy() *Problem {
	return newProblem(http.StatusServiceUnavailable, apperr.CodeUnavailable, "too many terminal catch-ups at once; retry shortly", nil).
		withHeader("Retry-After", "2")
}
