package fleet

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
)

// RunFate is what the database says about a run whose recording is in storage.
type RunFate int

const (
	// RunKeep: the run exists and its recording was not deleted (live, unfinished or ended). Never touched.
	RunKeep RunFate = iota
	// RunRecordingDeleted: the run's recording_deleted_at is set.
	RunRecordingDeleted
	// RunAbsent: the database cleanly reports no such run.
	RunAbsent
)

// ReconcileOptions bounds one reconcile pass.
type ReconcileOptions struct {
	// MaxRuns is the most runs examined per pass (the rest wait for the next pass). Every run examined counts, kept
	// ones too: a pass continues after the last run of the previous one and wraps, so with N stored runs every run is
	// examined within ceil(N/MaxRuns) passes. The worst case is a leftover waiting that many sweep intervals (with
	// 200 runs per pass and a 24h interval, 1000 stored runs take 5 days). Counting only delete candidates would need
	// a database lookup for every stored run on every pass, so it is not done.
	MaxRuns int
	// Absent allows removing the recording of a run the database cleanly reports absent. Off, such a recording is kept:
	// an absent row is also what a restored older backup or a second central on the same bucket looks like.
	Absent bool
	// MinAge protects the recording of a run absent from the database: it is deleted only when nothing in it changed
	// for this long.
	MinAge time.Duration
	// Pace is the pause after each run that was removed (rate limit).
	Pace time.Duration
}

// ReconcileResult counts one pass.
type ReconcileResult struct {
	Examined, Removed, Skipped int
	// Truncated is set when more runs were stored than MaxRuns; the next pass continues after the last one examined.
	Truncated bool
}

// Reconcile removes the stored recordings (spool folders and, with a remote store, objects) that belong to runs whose
// recording was deleted or that no longer exist. fate asks the database; any error from it (or from listing, or from
// a delete) skips the run and is logged: nothing is deleted on an error. A run that exists with its recording not
// deleted is never touched. The pass is bounded by opts.MaxRuns and paced by opts.Pace.
func (r *Recordings) Reconcile(ctx context.Context, fate func(ctx context.Context, runID string) (RunFate, error), opts ReconcileOptions) (ReconcileResult, error) {
	var res ReconcileResult
	newest := map[string]time.Time{} // run -> newest modification time seen in storage
	note := func(id string, t time.Time) {
		if _, ok := newest[id]; !ok {
			newest[id] = time.Time{} // seen; a zero time means "age unknown", never old enough to delete when absent
		}
		if t.After(newest[id]) {
			newest[id] = t
		}
	}
	ents, err := os.ReadDir(r.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}
	for _, e := range ents {
		if !e.IsDir() || !reRunID.MatchString(e.Name()) {
			continue
		}
		note(e.Name(), localNewest(filepath.Join(r.dir, e.Name())))
	}
	if r.archive {
		if pl, ok := r.store.(artifacts.PrefixLister); ok && !opts.Absent {
			// Only the run IDs are needed (an object's age matters only for absent runs), so list one folder per run
			// instead of every segment object, whose count grows with history.
			names, err := pl.ListPrefixes(ctx, artifacts.NSRecordings+"/")
			if err != nil {
				return res, err
			}
			for _, id := range names {
				if reRunID.MatchString(id) {
					note(id, time.Time{})
				}
			}
		} else {
			objs, err := r.store.List(ctx, artifacts.NSRecordings+"/")
			if err != nil {
				return res, err
			}
			for _, o := range objs {
				id, _, ok := strings.Cut(strings.TrimPrefix(o.Key, artifacts.NSRecordings+"/"), "/")
				if ok && reRunID.MatchString(id) {
					note(id, o.ModTime)
				}
			}
		}
	}
	ids := make([]string, 0, len(newest))
	for id := range newest {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if opts.MaxRuns > 0 && len(ids) > opts.MaxRuns {
		// Continue after the last run the previous pass examined (wrapping), so runs that are kept do not pin every
		// pass to the same first MaxRuns.
		r.mu.Lock()
		after := r.reconcileAfter
		r.mu.Unlock()
		start := sort.SearchStrings(ids, after)
		if start < len(ids) && ids[start] == after {
			start++
		}
		if start >= len(ids) {
			start = 0
		}
		picked := make([]string, 0, opts.MaxRuns)
		for i := 0; i < opts.MaxRuns; i++ {
			picked = append(picked, ids[(start+i)%len(ids)])
		}
		ids, res.Truncated = picked, true
		r.mu.Lock()
		r.reconcileAfter = ids[len(ids)-1]
		r.mu.Unlock()
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.Examined++
		f, err := fate(ctx, id)
		if err != nil {
			r.log.Warn("recording reconcile: run not checked, recording kept", slog.String("run.id", id), obs.Err(err))
			res.Skipped++
			continue
		}
		switch f {
		case RunRecordingDeleted:
		case RunAbsent:
			// A zero time means the folder could not be read: not "old".
			if !opts.Absent || newest[id].IsZero() || r.now().Sub(newest[id]) < opts.MinAge {
				res.Skipped++
				continue
			}
		default:
			continue
		}
		if err := r.Delete(ctx, id); err != nil {
			r.log.Warn("recording reconcile: leftover not removed; retried at the next pass", slog.String("run.id", id), obs.Err(err))
			res.Skipped++
			continue
		}
		res.Removed++
		if opts.Pace > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(opts.Pace):
			}
		}
	}
	return res, nil
}

// localNewest is the newest modification time of a folder and of the entries directly in it (not of anything nested
// deeper; a recording folder holds only segment files). A zero time means nothing could be read.
func localNewest(dir string) time.Time {
	var t time.Time
	if fi, err := os.Stat(dir); err == nil {
		t = fi.ModTime()
	}
	if es, err := os.ReadDir(dir); err == nil {
		for _, e := range es {
			if fi, err := e.Info(); err == nil && fi.ModTime().After(t) {
				t = fi.ModTime()
			}
		}
	}
	return t
}
