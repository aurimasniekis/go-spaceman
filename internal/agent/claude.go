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

// claudePinWindow is how long a profile's exact pin stays protected from GC after the node last launched a run with it:
// the node learns pins only from the launches it sees (ADR-0077).
const claudePinWindow = 30 * 24 * time.Hour

// claudePinned returns the exact versions GC must keep as pinned: the node's channel when that is an exact version, and
// every exact version a profile pinned on a launch this node saw (in this process, or in the journal within
// claudePinWindow).
func (a *Agent) claudePinned(ctx context.Context) []string {
	set := map[string]bool{}
	if dist.ValidVersion(a.o.Claude.Channel) {
		set[a.o.Claude.Channel] = true
	}
	a.mu.Lock()
	for v := range a.claudePins {
		set[v] = true
	}
	a.mu.Unlock()
	seen, err := a.j.harnessRequestsSince(ctx, harness.KindClaude, a.now().Add(-claudePinWindow))
	if err != nil && ctx.Err() == nil {
		a.log.Warn("claude code cache: pinned versions from the journal", obs.Err(err))
	}
	for _, v := range seen {
		if dist.ValidVersion(v) {
			set[v] = true
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// notePin remembers a profile's exact pin for GC (claudePinned).
func (a *Agent) notePin(v string) {
	if !dist.ValidVersion(v) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.claudePins == nil {
		a.claudePins = map[string]bool{}
	}
	a.claudePins[v] = true
}

// claudeRequest is what a Claude Code launch asks for: the profile's pin (minor ≥ 11, ADR-0077), which overrides the
// node's channel, else the channel ("" means latest).
func (a *Agent) claudeRequest(p proto.LaunchPayload) string {
	if p.HarnessVersion != "" {
		return p.HarnessVersion
	}
	return a.o.Claude.Channel
}

// claudeHoldAttempts bounds how often a managed launch waits for a version again because the one it waited for was no
// longer cached when it took it under the space's guard lock (the channel moved on, or GC removed a fallback).
const claudeHoldAttempts = 3

// claudeWait bounds a launch's waits for a Claude Code version, together, by the download timeout.
func (a *Agent) claudeWait(ctx context.Context) (context.Context, context.CancelFunc, time.Duration) {
	limit := a.o.Claude.DownloadTimeout
	if limit <= 0 {
		limit = dist.DefaultDownloadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	return ctx, cancel, limit
}

// waitClaude resolves the version a Claude Code run gets for requested (claudeRequest), waiting for a download when
// nothing usable is cached (bounded by wctx, see claudeWait). An exact version is that version only: it is downloaded
// when missing and never falls back to another one. It holds nothing when it returns: the caller holds the version with
// HoldCached once it is ready to build the launch.
func (a *Agent) waitClaude(wctx context.Context, limit time.Duration, requested string) (dist.Resolution, error) {
	res, release, err := a.o.Claude.Dist.Acquire(wctx, requested)
	if errors.Is(err, context.DeadlineExceeded) {
		return res, fmt.Errorf("no Claude Code version %s was available within the download timeout (%s): %w", channelName(requested), limit, err)
	}
	if err != nil {
		return res, err
	}
	release()
	return res, nil
}

func channelName(c string) string {
	if c == "" {
		return dist.ChannelLatest
	}
	return c
}

// stage runs the test hook at a managed Claude Code launch's stage (see Agent.claudeStage).
func (a *Agent) stage(name string, res dist.Resolution) {
	if a.claudeStage != nil {
		a.claudeStage(name, res)
	}
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
	for _, v := range append(used, a.claudePinned(ctx)...) {
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
		if errors.Is(err, dist.ErrVerify) && ctx.Err() == nil {
			log.Warn("claude code cache: a version failed verification and was removed", slog.String("version", v), obs.Err(err))
		} else {
			log.Warn("claude code cache: a version could not be verified and was kept", slog.String("version", v), obs.Err(err))
		}
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
	removed, err := a.o.Claude.Dist.GC(ctx, a.claudePinned(ctx), a.claudeVersionsInUse)
	if err != nil && ctx.Err() == nil {
		a.log.Warn("claude code cache: garbage collection", obs.Err(err))
	}
	if len(removed) > 0 {
		a.log.Info("claude code cache: removed unused versions", slog.Any("versions", removed))
	}
}

// claudeInfo is the capability inventory's Claude Code entry on a managed node: the cached version a new run would get
// without waiting (the channel's memoised target, else the newest cached version). It detects nothing on PATH. From
// minor 11 on it also carries the node's channel, the memoised channel targets and the cache's versions and size.
func (a *Agent) claudeInfo(ctx context.Context) proto.HarnessInfo {
	m := a.o.Claude.Dist
	h, _ := harness.Get(harness.KindClaude)
	want := a.o.Claude.Channel
	if !dist.ValidVersion(want) {
		want = m.Resolved()[channelName(want)]
	}
	installed := m.Installed()
	info := proto.HarnessInfo{Kind: harness.KindClaude, Error: "managed: no version cached yet", Capabilities: []string{}}
	for _, inst := range installed {
		if inst.Version == want {
			info = h.Detect(ctx, inst.Path)
			break
		}
	}
	if info.Path == "" && len(installed) > 0 && !dist.ValidVersion(a.o.Claude.Channel) {
		info = h.Detect(ctx, installed[0].Path)
	}
	info.Managed, info.Channel = true, channelName(a.o.Claude.Channel)
	for c, t := range m.ResolvedTargets() {
		if info.Resolved == nil {
			info.Resolved = map[string]proto.HarnessResolved{}
		}
		info.Resolved[c] = proto.HarnessResolved{Version: t.Version, ResolvedAt: t.ResolvedAt.UTC()}
	}
	for _, inst := range installed {
		info.Cached = append(info.Cached, inst.Version)
		info.CacheBytes += inst.Size
	}
	return info
}

// checkUnmanagedPin refuses a Claude Code profile pin on a node that doesn't manage Claude Code (manage: false,
// ADR-0077): the pin must equal the version detected at bin, or the launch fails; a channel pin (latest, stable) can't
// be checked there and fails too. It never runs another version than the one pinned. It returns the detected version.
func (a *Agent) checkUnmanagedPin(ctx context.Context, bin, pin string) (proto.HarnessInfo, error) {
	h, _ := harness.Get(harness.KindClaude)
	info := h.Detect(ctx, bin)
	switch {
	case !dist.ValidVersion(pin):
		return info, fmt.Errorf("this node does not manage Claude Code versions (harness.claude.manage is off), so it can't resolve the profile's pin %q; pin an exact version that matches the installed one, clear the pin, or let the node manage Claude Code", pin)
	case !info.Available:
		return info, fmt.Errorf("this node does not manage Claude Code versions, and the installed Claude Code could not be detected to check the profile's pin %s: %s", pin, info.Error)
	case info.Version != pin:
		return info, fmt.Errorf("this node does not manage Claude Code versions: the profile pins %s, but the installed Claude Code is %s", pin, info.Version)
	}
	return info, nil
}

// warnClaudeUpdateSettings logs the profile's Claude Code settings that would steer its updater. The caller warns only
// when the node turns the updater off (manage on, or self_update off): then the node decides the version and they have
// no effect on what runs (ADR-0077). With self_update on they do, and nothing is logged.
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
