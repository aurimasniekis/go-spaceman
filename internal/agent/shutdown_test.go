package agent

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// A launch still finishing while Run's shutdown drains its background work sees its program live and observes it,
// which starts the run's recorder. Recordings were already stopped by then, so no recorder may start: nothing would
// cancel it, and it would keep writing runs/<id>/ after Run returned.
func TestLaunchFinishingDuringDrainStartsNoRecorder(t *testing.T) {
	a, f, row := sizeAgent(t)
	ctx := context.Background()
	release, observed := make(chan struct{}), make(chan struct{})
	// The accepted launch, tracked like command's: it reaches observe only once shutdown has begun.
	a.goTrack(func() {
		defer close(observed)
		<-release
		p, err := f.Get(ctx, row.TerminalID)
		if err != nil || p.State != runner.StateLive {
			t.Errorf("launch's program: %v, %+v", err, p)
			return
		}
		a.observe(ctx, row, *p, false)
	})
	stopped := make(chan struct{})
	go func() { a.shutdown(time.Minute); close(stopped) }()
	// shutdown stops recordings before it drains: wait for that, then let the launch finish inside the drain.
	waitFor(t, "recordings stopped", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.recordersStopped
	})
	close(release)
	<-observed
	<-stopped
	a.mu.Lock()
	n := len(a.recorders)
	a.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d recorder(s) running after shutdown", n)
	}
}
