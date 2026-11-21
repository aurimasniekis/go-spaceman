package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// A repository report runs `git status` in every linked worktree. With around a thousand worktrees that is a fork and
// CPU cost per report, so the report throttles it: a worktree whose tree has been idle for repoStatusIdle reuses the
// counts of an earlier status while nothing about it changed, and at most repoStatusMax status calls run per report.
// Both are visible in the report (WorktreeCheck.Status) and neither ever turns an unknown status into a clean one: a
// worktree that is neither cached nor within the limit is reported with an error and unknown counts.
//
// The activity stamp cannot see an edit deep inside an existing subfolder, so cached counts are also refreshed: an entry
// older than repoStatusRefresh is checked again with whatever allowance the report has left after the worktrees that
// need it more (and stays reported as cached, with its time, when there is none).
var (
	repoStatusIdle    = 7 * 24 * time.Hour
	repoStatusMax     = 200
	repoStatusRefresh = 24 * time.Hour
	// repoStatusMaxAge is the hard limit on cached counts: past it without a refresh an entry is reported unchecked
	// (unknown) rather than shown with counts that may be long out of date.
	repoStatusMaxAge = 7 * 24 * time.Hour
)

// wtCacheKeep is how long an unrefreshed entry is kept before eviction (worktrees that were removed).
const wtCacheKeep = 30 * 24 * time.Hour

// wtCached is the counts of one earlier worktree status and what they were taken against.
type wtCached struct {
	stamp            time.Time // the worktree's activity stamp when the status ran
	head             string
	dirty, untracked int
	at               time.Time
}

// wtStatusCache remembers worktree status counts across reports (in memory: after a restart the first reports refill it,
// each within the per-report limit, and what is not refilled yet is reported unknown).
type wtStatusCache struct {
	mu  sync.Mutex
	m   map[string]wtCached
	try map[string]time.Time // when a status was last attempted per worktree, successful or not
}

// attempted records that a status call was made for the worktree.
func (c *wtStatusCache) attempted(path string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.try == nil {
		c.try = map[string]time.Time{}
	}
	c.try[path] = at
	if len(c.try) > 4096 {
		for k, e := range c.try {
			if at.Sub(e) > wtCacheKeep {
				delete(c.try, k)
			}
		}
	}
}

func (c *wtStatusCache) lastTry(path string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.try[path]
}

func (c *wtStatusCache) get(path string) (wtCached, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[path]
	return v, ok
}

func (c *wtStatusCache) put(path string, v wtCached) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]wtCached{}
	}
	c.m[path] = v
	if len(c.m) > 4096 { // drop what nothing has refreshed for a long time (removed worktrees)
		for k, e := range c.m {
			if v.at.Sub(e.at) > wtCacheKeep {
				delete(c.m, k)
			}
		}
	}
}

// wtBudget is one report's worktree status allowance. A nil budget means no throttling (relocation needs exact counts).
// The repositories of a report only classify their worktrees into it; spend then runs the statuses once for the whole
// report, so the allowance is shared by every repository in order of need, not of configuration.
type wtBudget struct {
	cache *wtStatusCache
	left  int
	max   int
	idle  time.Duration
	now   func() time.Time

	recent, idle2, refresh []wtJob
}

// wtJob is one worktree that wants a status call this report.
type wtJob struct {
	w     *proto.WorktreeCheck
	p     wtPending
	stamp time.Time
	ok    bool
	tried time.Time // when a status was last attempted (zero: never), the fairness order
}

func (a *Agent) newWtBudget() *wtBudget {
	return &wtBudget{cache: &a.wtStatus, left: repoStatusMax, max: repoStatusMax, idle: repoStatusIdle, now: a.now}
}

// worktreeStamp is when a worktree's tree last visibly changed: the newest modification time among the worktree folder,
// its top-level entries and its Git directory's index and HEAD. It reads no file contents and runs no git. It cannot see
// an edit deep in an existing subfolder, which is why a cached result is only ever reported as such. ok is false when
// it cannot be read, and the worktree is then treated as recently changed.
func worktreeStamp(path string) (t time.Time, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return t, false
	}
	t = fi.ModTime()
	ents, err := os.ReadDir(path)
	if err != nil {
		return t, false
	}
	for _, e := range ents {
		if e.Name() == ".git" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return t, false
		}
		if m := info.ModTime(); m.After(t) {
			t = m
		}
	}
	raw, err := os.ReadFile(filepath.Join(path, ".git")) //nolint:gosec // a worktree's gitdir pointer
	if err != nil {
		return t, false
	}
	gd, found := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !found {
		return t, false
	}
	gd = strings.TrimSpace(gd)
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(path, gd)
	}
	for _, f := range []string{"index", "HEAD"} {
		info, err := os.Stat(filepath.Join(gd, f)) //nolint:gosec // the worktree's own gitdir pointer, only stat-ed
		if err != nil {
			if f == "index" && os.IsNotExist(err) {
				continue // a worktree never checked out has none
			}
			return t, false
		}
		if m := info.ModTime(); m.After(t) {
			t = m
		}
	}
	return t, true
}

type wtPending struct {
	i    int // index in the repository's worktree list
	path string
	head string
}

// wtRun runs git status for one pending worktree and records the result.
func (g *projectGit) wtRun(ctx context.Context, b *wtBudget, w *proto.WorktreeCheck, p wtPending, stamp time.Time, stampOK bool) {
	// GIT_OPTIONAL_LOCKS=0: the status does not refresh (rewrite) the worktree's index, so it neither changes the
	// activity stamp it is cached against nor contends with the worktree's own git commands.
	d, u, _, err := g.statusEnv(ctx, p.path, []string{"GIT_OPTIONAL_LOCKS=0"})
	if b != nil {
		b.cache.attempted(p.path, b.now())
	}
	if err != nil {
		if b != nil {
			w.Status = proto.WorktreeStatusUnchecked
		}
		w.Error = err.Error()
		return
	}
	w.Dirty, w.Untracked = d, u
	if b == nil {
		return
	}
	now := b.now()
	w.Status, w.StatusAt = proto.WorktreeStatusFresh, &now
	if stampOK {
		b.cache.put(p.path, wtCached{stamp: stamp, head: p.head, dirty: d, untracked: u, at: now})
	}
}

// worktreeStatuses fills in the counts of the pending worktrees. With a nil budget every one runs git status; with a
// budget they are classified (cached ones are filled in at once) and queued for budget.spend.
func (g *projectGit) worktreeStatuses(ctx context.Context, b *wtBudget, wcs []proto.WorktreeCheck, pend []wtPending) {
	if b == nil {
		for _, p := range pend {
			g.wtRun(ctx, nil, &wcs[p.i], p, time.Time{}, false)
		}
		return
	}
	now := b.now()
	for _, p := range pend {
		w := &wcs[p.i]
		stamp, ok := worktreeStamp(p.path)
		j := wtJob{w: w, p: p, stamp: stamp, ok: ok, tried: b.cache.lastTry(p.path)}
		if ok && now.Sub(stamp) > b.idle {
			if c, hit := b.cache.get(p.path); hit && c.stamp.Equal(stamp) && c.head == p.head {
				at := c.at
				w.Dirty, w.Untracked, w.Status, w.StatusAt = c.dirty, c.untracked, proto.WorktreeStatusCached, &at
				if now.Sub(c.at) > repoStatusRefresh {
					b.refresh = append(b.refresh, j)
				}
				continue
			}
			b.idle2 = append(b.idle2, j)
			continue
		}
		b.recent = append(b.recent, j)
	}
}

// spend runs the queued statuses within the allowance: recently changed worktrees first, then idle ones with no usable
// cached counts, then the refresh of old cached counts. Within each group the one attempted longest ago (or never) goes
// first, whichever repository it is in, so every worktree is reached over successive reports and nothing stays
// unchecked just for being last. What is left without an allowance is reported unknown; cached counts older than
// repoStatusMaxAge are never kept: that is unchecked, not stale.
func (b *wtBudget) spend(ctx context.Context, g *projectGit) {
	now := b.now()
	oldest := func(js []wtJob) {
		sort.SliceStable(js, func(x, y int) bool { return js[x].tried.Before(js[y].tried) })
	}
	oldest(b.recent)
	oldest(b.idle2)
	oldest(b.refresh)
	expired := func(w *proto.WorktreeCheck) bool {
		return w.StatusAt != nil && now.Sub(*w.StatusAt) > repoStatusMaxAge
	}
	unchecked := func(w *proto.WorktreeCheck, msg string) {
		w.Dirty, w.Untracked, w.StatusAt = 0, 0, nil
		w.Status = proto.WorktreeStatusUnchecked
		w.Error = msg
	}
	all := append(append(append([]wtJob{}, b.recent...), b.idle2...), b.refresh...)
	b.recent, b.idle2, b.refresh = nil, nil, nil
	for _, j := range all {
		w := j.w
		if b.left <= 0 {
			if w.Status == proto.WorktreeStatusCached && !expired(w) {
				continue // old counts stay, labelled with their time
			}
			if w.Status == proto.WorktreeStatusCached {
				unchecked(w, fmt.Sprintf("status not checked: the cached counts are older than %s and this report ran its limit of %d worktree status calls; the counts are unknown, not clean", repoStatusMaxAge, b.max))
				continue
			}
			w.Status = proto.WorktreeStatusUnchecked
			w.Error = fmt.Sprintf("status not checked: this report ran its limit of %d worktree status calls; the counts are unknown, not clean", b.max)
			continue
		}
		b.left--
		if w.Status != proto.WorktreeStatusCached {
			g.wtRun(ctx, b, w, j.p, j.stamp, j.ok)
			continue
		}
		saved := *w
		if g.wtRun(ctx, b, w, j.p, j.stamp, j.ok); w.Error != "" { // a failed refresh keeps the older counts, still labelled cached
			err := w.Error
			*w = saved
			if expired(w) {
				unchecked(w, "status not checked: the cached counts are older than "+repoStatusMaxAge.String()+" and the refresh failed: "+err)
			}
		}
	}
}
