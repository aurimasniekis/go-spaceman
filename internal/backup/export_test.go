package backup

import (
	"context"
	"log/slog"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
)

// RunWatched runs fn under the same tracker and progress watchdog as a backup run, in stage "upload", and returns
// the watchdog's error when it stopped fn.
func RunWatched(ctx context.Context, stall, total time.Duration, fn func(ctx context.Context) error) error {
	tr := newTracker("bkp_test", clock.System{}, slog.New(slog.DiscardHandler))
	ctx, stop := watch(withTracker(ctx, tr), tr, stall, total)
	defer stop()
	tr.enter(StageUpload)
	err := fn(ctx)
	if se := stalled(ctx); se != nil && err != nil {
		return se
	}
	return err
}
