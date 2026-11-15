package dist

import (
	"context"
	"log/slog"
	"time"
)

// Run re-resolves channels every refresh interval and installs their targets in the background, so launches rarely
// wait for a download. It returns when ctx ends.
func (m *Manager) Run(ctx context.Context, channels ...string) {
	t := time.NewTicker(m.refresh)
	defer t.Stop()
	for {
		m.prefetchOnce(ctx, channels)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) prefetchOnce(ctx context.Context, channels []string) {
	for _, ch := range channels {
		if !ValidChannel(ch) {
			continue
		}
		v, err := m.refreshChannel(ctx, ch)
		if err != nil {
			m.log.Warn("claude code channel refresh failed", slog.String("channel", ch), slog.String("error", err.Error()))
			continue
		}
		m.Prefetch(v) //nolint:contextcheck // installs run on the manager's context, bounded by the download timeout
	}
}
