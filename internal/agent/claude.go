package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/harness/dist"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// ClaudeOptions configure node-managed Claude Code (harness.claude, ADR-0077).
type ClaudeOptions struct {
	// Dist is the verified release cache (harness.claude.manage). Nil: Claude Code is not managed, and runs use
	// HarnessBinaries[claude-code] through the core's PATH as before.
	Dist *dist.Manager
	// Channel is the node's version: latest (default), stable or an exact X.Y.Z.
	Channel string
	// DownloadTimeout bounds how long a launch waits in starting for a version when none it may run is cached.
	// Default dist.DefaultDownloadTimeout.
	DownloadTimeout time.Duration
	// SelfUpdate lets unmanaged runs update themselves: without it every Claude Code run gets the updater-off
	// variables. Managed runs always get them (nothing would launch a self-updated copy).
	SelfUpdate bool
}

// managedClaude reports whether this node manages Claude Code.
func (a *Agent) managedClaude() bool { return a.o.Claude.Dist != nil }

// claudePinned returns the versions the node itself pins: its channel when that is an exact version. (Profile pins
// arrive with protocol minor 11, CC-4.)
func (a *Agent) claudePinned() []string {
	if dist.ValidVersion(a.o.Claude.Channel) {
		return []string{a.o.Claude.Channel}
	}
	return nil
}

// acquireClaude resolves the version a Claude Code run gets and holds it against GC until release is called, which the
// caller does once the run's journal row records it. When nothing usable is cached the call waits for a download,
// bounded by the download timeout.
func (a *Agent) acquireClaude(ctx context.Context) (dist.Resolution, func(), error) {
	limit := a.o.Claude.DownloadTimeout
	if limit <= 0 {
		limit = dist.DefaultDownloadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	res, release, err := a.o.Claude.Dist.Acquire(ctx, a.o.Claude.Channel)
	if errors.Is(err, context.DeadlineExceeded) {
		return res, nil, fmt.Errorf("no Claude Code version %s was available within the download timeout (%s): %w", channelName(a.o.Claude.Channel), limit, err)
	}
	return res, release, err
}

func channelName(c string) string {
	if c == "" {
		return dist.ChannelLatest
	}
	return c
}

// claudeVersionsInUse is GC's inUse: the versions of the journal's live and adoptable Claude Code runs.
func (a *Agent) claudeVersionsInUse(ctx context.Context) ([]string, error) {
	return a.j.harnessVersionsInUse(ctx, harness.KindClaude)
}

// startClaude prepares the managed cache at startup (removes interrupted installs, re-hashes the versions the node will
// use, collects garbage) and starts the background work: the channel prefetch loop and periodic GC. wg tracks the
// background goroutines, which end with ctx.
func (a *Agent) startClaude(ctx context.Context, wg *sync.WaitGroup) {
	m := a.o.Claude.Dist
	if m == nil {
		return
	}
	log := a.log.With(slog.String("harness", harness.KindClaude))
	if err := m.Clean(); err != nil {
		log.Warn("claude code cache: clean up interrupted installs", obs.Err(err))
	}
	used, err := a.claudeVersionsInUse(ctx)
	if err != nil {
		log.Warn("claude code cache: versions in use", obs.Err(err))
	}
	verify := map[string]bool{}
	for _, v := range append(used, a.claudePinned()...) {
		verify[v] = true
	}
	for _, v := range m.Resolved() {
		verify[v] = true
	}
	list := make([]string, 0, len(verify))
	for v := range verify {
		list = append(list, v)
	}
	sort.Strings(list)
	for v, err := range m.Verify(ctx, list...) {
		log.Warn("claude code cache: a version failed verification and was removed", slog.String("version", v), obs.Err(err))
	}
	a.claudeGC(ctx)
	if dist.ValidVersion(a.o.Claude.Channel) {
		m.Prefetch(a.o.Claude.Channel) //nolint:contextcheck // installs run on the manager's context
	} else {
		wg.Go(func() { m.Run(ctx, channelName(a.o.Claude.Channel)) })
	}
	wg.Go(func() {
		t := time.NewTicker(m.RefreshInterval())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.claudeGC(ctx)
			}
		}
	})
}

// claudeGC removes cached versions nothing needs any more.
func (a *Agent) claudeGC(ctx context.Context) {
	removed, err := a.o.Claude.Dist.GC(ctx, a.claudePinned(), a.claudeVersionsInUse)
	if err != nil && ctx.Err() == nil {
		a.log.Warn("claude code cache: garbage collection", obs.Err(err))
	}
	if len(removed) > 0 {
		a.log.Info("claude code cache: removed unused versions", slog.Any("versions", removed))
	}
}

// claudeInfo is the capability inventory's Claude Code entry on a managed node: the cached version a new run would get
// without waiting (the channel's memoised target, else the newest cached version). It detects nothing on PATH.
func (a *Agent) claudeInfo(ctx context.Context) proto.HarnessInfo {
	m := a.o.Claude.Dist
	h, _ := harness.Get(harness.KindClaude)
	want := a.o.Claude.Channel
	if !dist.ValidVersion(want) {
		want = m.Resolved()[channelName(want)]
	}
	installed := m.Installed()
	for _, inst := range installed {
		if inst.Version == want {
			return h.Detect(ctx, inst.Path)
		}
	}
	if len(installed) > 0 && !dist.ValidVersion(a.o.Claude.Channel) {
		return h.Detect(ctx, installed[0].Path)
	}
	return proto.HarnessInfo{Kind: harness.KindClaude, Error: "managed: no version cached yet", Capabilities: []string{}}
}

// warnClaudeUpdateSettings logs the profile's Claude Code settings that would steer its updater: the node decides the
// version, so they have no effect on what runs (ADR-0077).
func warnClaudeUpdateSettings(log *slog.Logger, cs proto.ClaudeSettings) {
	var keys []string
	for _, k := range []string{"autoUpdatesChannel", "minimumVersion"} {
		if _, ok := cs[k]; ok {
			keys = append(keys, k)
		}
	}
	for _, k := range harness.ClaudeUpdaterOffEnv {
		set := false
		switch env := cs["env"].(type) {
		case map[string]any:
			_, set = env[k]
		case map[string]string:
			_, set = env[k]
		}
		if set {
			keys = append(keys, "env."+k)
		}
	}
	if len(keys) > 0 {
		log.Warn("the profile's Claude Code updater settings have no effect: the node decides the version", slog.Any("keys", keys))
	}
}
