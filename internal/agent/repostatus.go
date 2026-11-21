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
	rot int // reports started so far; rotates where each report starts spending its allowance
}

// nextRot returns the rotation of a new report.
func (c *wtStatusCache) nextRot() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rot++
	return c.rot - 1
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
type wtBudget struct {
	cache *wtStatusCache
	left  int
	max   int
	idle  time.Duration
	now   func() time.Time
	rot   int // rotation of this report, see rotate
}

// rotate starts a list at a different place on every report (by whole allowances), so that when more worktrees need a
// status than a report may run, the unchecked ones differ from report to report instead of being the same tail.
func rotate[T any](s []T, rot, step int) []T {
	if len(s) < 2 || step <= 0 {
		return s
	}
	k := (rot * step) % len(s)
	return append(append([]T{}, s[k:]...), s[:k]...)
}

func (a *Agent) newWtBudget() *wtBudget {
	return &wtBudget{cache: &a.wtStatus, left: repoStatusMax, max: repoStatusMax, idle: repoStatusIdle, now: a.now, rot: a.wtStatus.nextRot()}
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

// worktreeStatuses fills in the counts of the pending worktrees. With a nil budget every one runs git status.
func (g *projectGit) worktreeStatuses(ctx context.Context, b *wtBudget, wcs []proto.WorktreeCheck, pend []wtPending) {
	run := func(p wtPending, stamp time.Time, stampOK bool) {
		w := &wcs[p.i]
		// GIT_OPTIONAL_LOCKS=0: the status does not refresh (rewrite) the worktree's index, so it neither changes the
		// activity stamp it is cached against nor contends with the worktree's own git commands.
		d, u, _, err := g.statusEnv(ctx, p.path, []string{"GIT_OPTIONAL_LOCKS=0"})
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
	if b == nil {
		for _, p := range pend {
			run(p, time.Time{}, false)
		}
		return
	}
	type job struct {
		p     wtPending
		stamp time.Time
		ok    bool
		at    time.Time // when the cached counts were taken (refresh jobs)
	}
	var recent, idle, refresh []job
	now := b.now()
	for _, p := range pend {
		stamp, ok := worktreeStamp(p.path)
		if ok && now.Sub(stamp) > b.idle {
			if c, hit := b.cache.get(p.path); hit && c.stamp.Equal(stamp) && c.head == p.head {
				at := c.at
				w := &wcs[p.i]
				w.Dirty, w.Untracked, w.Status, w.StatusAt = c.dirty, c.untracked, proto.WorktreeStatusCached, &at
				if now.Sub(c.at) > repoStatusRefresh {
					refresh = append(refresh, job{p, stamp, ok, c.at})
				}
				continue
			}
			idle = append(idle, job{p, stamp, ok, time.Time{}})
			continue
		}
		recent = append(recent, job{p, stamp, ok, time.Time{}})
	}
	// Recently changed worktrees first: they are the ones whose counts matter; an idle one with no cached counts yet
	// takes what is left, then the refresh of old cached counts, oldest first; what an unknown worktree is left without
	// is reported unknown. The allowance is shared by the repositories of a report, so the starting point of the recent
	// and idle lists rotates between reports (and the repositories' order too, see syncRepos): nothing stays unchecked
	// just for being last. Cached counts older than repoStatusMaxAge are never kept: that is unchecked, not stale.
	sort.SliceStable(refresh, func(x, y int) bool { return refresh[x].at.Before(refresh[y].at) })
	expired := func(w *proto.WorktreeCheck) bool {
		return w.StatusAt != nil && now.Sub(*w.StatusAt) > repoStatusMaxAge
	}
	unchecked := func(w *proto.WorktreeCheck, msg string) {
		w.Dirty, w.Untracked, w.StatusAt = 0, 0, nil
		w.Status = proto.WorktreeStatusUnchecked
		w.Error = msg
	}
	all := append(append(rotate(recent, b.rot, b.max), rotate(idle, b.rot, b.max)...), refresh...)
	for _, j := range all {
		w := &wcs[j.p.i]
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
			run(j.p, j.stamp, j.ok)
			continue
		}
		saved := *w
		if run(j.p, j.stamp, j.ok); w.Error != "" { // a failed refresh keeps the older counts, still labelled cached
			err := w.Error
			*w = saved
			if expired(w) {
				unchecked(w, "status not checked: the cached counts are older than "+repoStatusMaxAge.String()+" and the refresh failed: "+err)
			}
		}
	}
}
