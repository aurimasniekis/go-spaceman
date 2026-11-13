package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
)

// RecordingRules are the authorization rules of the recording operations (recordings are a separate permission from
// terminals and runs, SM-AUTH-004; they may contain secrets, SM-REC-004).
var RecordingRules = map[string]Rule{
	"get-recording":    InSpace(authz.RecordingRead),
	"delete-recording": {Perm: authz.RecordingDelete, Space: true, Reauth: true, MFA: true},
}

// RecordingPage is a page of recording records.
type RecordingPage struct {
	Records   []recording.Record `json:"records"`
	Truncated bool               `json:"truncated" doc:"A segment ended in an incomplete frame (interrupted writer)"`
	Gaps      int                `json:"gaps" doc:"Sequence discontinuities and explicit gap records"`
	NextAfter uint64             `json:"next_after" doc:"Pass as after= to continue"`
	More      bool               `json:"more"`
	Tail      *RecordingTail     `json:"tail,omitempty" doc:"Set when the page is a tail (tail=): what comes before its records"`
}

// RecordingTail describes the output a tail leaves out (ADR-0081): a viewer draws the tail on a fresh terminal of
// cols×rows after setting or resetting the modes, in order.
type RecordingTail struct {
	From         uint64           `json:"from" doc:"Sequence number of the tail's first record (0 when the recording is empty)"`
	SkippedBytes int64            `json:"skipped_bytes" doc:"Bytes of output before the tail; 0 when the tail is the whole recording"`
	Cols         int              `json:"cols" doc:"Terminal columns where the tail starts (0 when no size was recorded before it)"`
	Rows         int              `json:"rows" doc:"Terminal rows where the tail starts"`
	Modes        []recording.Mode `json:"modes" doc:"DEC private modes the output before the tail set (on) or reset, in the order they last changed"`
	Gaps         int              `json:"gaps" doc:"Gap records before the tail"`
	Truncated    bool             `json:"truncated" doc:"Whether a truncation record comes before the tail"`
}

func tailOf(res recording.TailResult) *RecordingTail {
	t := &RecordingTail{SkippedBytes: res.Skipped, Cols: res.Cols, Rows: res.Rows, Modes: res.Modes, Gaps: res.SkippedGaps, Truncated: res.SkippedTrunc}
	if t.Modes == nil {
		t.Modes = []recording.Mode{}
	}
	if len(res.Records) > 0 {
		t.From = res.Records[0].Seq
	}
	return t
}

// RegisterRecordings registers recording replay operations.
func RegisterRecordings(f *fleet.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"recordings"}
		huma.Register(api, huma.Operation{OperationID: "get-recording", Method: http.MethodGet, Path: "/spaces/{space_id}/runs/{run_id}/recording",
			Summary: "Replay a run's terminal recording", Description: "Records are output (base64), size, gap, close and truncation markers in sequence order.",
			Tags: tags},
			func(ctx context.Context, in *struct {
				RunPath
				After uint64 `query:"after" doc:"Return records with seq greater than this"`
				Limit int    `query:"limit" minimum:"1" maximum:"5000" default:"1000"`
				Tail  int64  `query:"tail" minimum:"0" maximum:"16777216" doc:"Return only the end of the recording: the records whose output adds up to at most this many bytes, in one page (limit and after do not apply), with what comes before them in tail (ADR-0081). 0 returns pages from after."`
			}) (*struct{ Body RecordingPage }, error) {
				if _, err := d.spaceRun(ctx, f, in.SpaceID, in.RunID); err != nil {
					return nil, err
				}
				if in.Tail > 0 {
					res, err := f.Recordings().Tail(ctx, in.RunID, in.Tail)
					if err != nil {
						return nil, d.fail(ctx, "recordings.get", err)
					}
					return &struct{ Body RecordingPage }{Body: RecordingPage{Records: res.Records, Truncated: res.Truncated, Gaps: res.Gaps,
						NextAfter: res.LastSeq, Tail: tailOf(res)}}, nil
				}
				if in.Limit == 0 {
					in.Limit = 1000
				}
				// Only the page is held in memory; the rest of the recording is decoded for the gap count and dropped.
				out := &struct{ Body RecordingPage }{Body: RecordingPage{Records: []recording.Record{}, NextAfter: in.After}}
				res, err := f.Recordings().Scan(ctx, in.RunID, in.After, func(r recording.Record) error {
					if len(out.Body.Records) == in.Limit {
						out.Body.More = true
						return nil
					}
					out.Body.Records = append(out.Body.Records, r)
					out.Body.NextAfter = r.Seq
					return nil
				})
				if err != nil {
					return nil, d.fail(ctx, "recordings.get", err)
				}
				out.Body.Truncated, out.Body.Gaps = res.Truncated, res.Gaps
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-recording", Method: http.MethodDelete, Path: "/spaces/{space_id}/runs/{run_id}/recording",
			Summary: "Delete central's copy of a run's recording", Description: "The run must have ended (409 otherwise). The deletion is audited " +
				"and recorded in the change feed; the node's own copy is not affected.", Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *RunPath) (*struct{}, error) {
				if _, err := d.spaceRun(ctx, f, in.SpaceID, in.RunID); err != nil {
					return nil, err
				}
				if err := f.DeleteRecording(ctx, Actor(ctx), in.SpaceID, in.RunID, nil); err != nil {
					return nil, d.fail(ctx, "recordings.delete", err)
				}
				return &struct{}{}, nil
			})
	}
}

// RecordingStreams returns the raw (non-huma) recording endpoints: the live terminal stream (SSE) and the download.
func RecordingStreams(f *fleet.Service) []RawRoute {
	return []RawRoute{
		{Pattern: "GET " + Prefix + "/spaces/{space_id}/runs/{run_id}/terminal", Perm: authz.TerminalWatch, Handler: func(d *Deps) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { d.terminalStream(w, r, f) }
		}},
		{Pattern: "GET " + Prefix + "/spaces/{space_id}/runs/{run_id}/recording/download", Perm: authz.RecordingRead, Handler: func(d *Deps) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { d.recordingDownload(w, r, f) }
		}},
	}
}

// terminalStream serves a live, watch-only terminal (SM-UI-004): stored records first (catch-up), then live records.
// A viewer that falls behind is told about the gap and re-reads from storage; it never slows the node (SM-REC-004).
// The catch-up is read one record at a time, so a long recording is never held in memory.
//
// With tail=<bytes> (and no after or Last-Event-ID), the catch-up is only the end of the recording, the records whose
// output adds up to at most that many bytes, preceded by a "tail" event (RecordingTail) that says what comes before it
// (ADR-0081): the current screen shows without the whole history. A reconnect resumes after the last record as usual.
//
// Events: "tail", "record" (data: recording.Record JSON, id: seq), "gap", "heartbeat", and "end": data {} when the run ended, or
// {"reason":"unauthenticated"|"forbidden"} when the viewer's credential stopped being valid or no longer grants
// terminal:watch in the space. The stream guard re-authorizes at once after a committed revocation or grant change and
// every StreamRevalidate otherwise (SM-AUTH-007, ADR-0033).
func (d *Deps) terminalStream(w http.ResponseWriter, r *http.Request, f *fleet.Service) {
	ctx := r.Context()
	spaceID, runID := r.PathValue("space_id"), r.PathValue("run_id")
	run, err := d.spaceRun(ctx, f, spaceID, runID)
	if err != nil {
		writeProblem(w, newProblem(http.StatusNotFound, "not_found", "run not found", nil))
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, newProblem(http.StatusInternalServerError, "internal_error", "streaming unsupported", nil))
		return
	}
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		after, _ = strconv.ParseUint(v, 10, 64)
	}
	var tail int64
	if after == 0 {
		tail, _ = strconv.ParseInt(r.URL.Query().Get("tail"), 10, 64)
		tail = min(max(tail, 0), recording.MaxTailBytes)
	}
	// Guard from the start, so a revocation committed during catch-up is not missed.
	guard := d.guardStream(r, PrincipalFrom(ctx), func(p *auth.Principal) bool {
		return d.Authz != nil && d.Authz.AllowedAll(p.Subjects(), spaceID, authz.TerminalWatch)
	})
	defer guard.close()
	v := f.Recordings().Subscribe(runID)
	defer f.Recordings().Unsubscribe(v)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	d.Metrics.SSEClientConnected()
	defer d.Metrics.SSEClientDisconnected()

	send := func(rec recording.Record) error {
		b, _ := json.Marshal(rec)
		_, err := fmt.Fprintf(w, "event: record\nid: %d\ndata: %s\n\n", rec.Seq, b)
		return err
	}
	catchUp := func() bool {
		_, err := f.Recordings().Scan(ctx, runID, after, func(rec recording.Record) error {
			if err := send(rec); err != nil {
				return err
			}
			after = rec.Seq
			return nil
		})
		if err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	catchUpTail := func() bool {
		res, err := f.Recordings().Tail(ctx, runID, tail)
		if err != nil {
			return false
		}
		b, _ := json.Marshal(tailOf(res))
		if _, err := fmt.Fprintf(w, "event: tail\ndata: %s\n\n", b); err != nil {
			return false
		}
		for _, rec := range res.Records {
			if send(rec) != nil {
				return false
			}
		}
		after = res.LastSeq
		fl.Flush()
		return true
	}
	if tail > 0 && !catchUpTail() || tail == 0 && !catchUp() {
		return
	}
	if ended(run.Observed) {
		_, _ = fmt.Fprint(w, "event: end\ndata: {}\n\n")
		fl.Flush()
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case rec := <-v.C:
			if rec.Seq <= after {
				continue
			}
			if v.TakeLagged() || rec.Seq != after+1 {
				_, _ = fmt.Fprintf(w, "event: gap\ndata: {\"after\":%d}\n\n", after)
				if !catchUp() {
					return
				}
				continue
			}
			if send(rec) != nil {
				return
			}
			after = rec.Seq
			fl.Flush()
			if rec.Kind == recording.KindClose {
				_, _ = fmt.Fprint(w, "event: end\ndata: {}\n\n")
				fl.Flush()
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, "event: heartbeat\ndata: {}\n\n"); err != nil {
				return
			}
			fl.Flush()
		case <-guard.revoked():
			if reason := guard.check(ctx); reason != "" {
				endStream(w, fl, reason)
				return
			}
		case <-guard.due():
			if reason := guard.check(ctx); reason != "" {
				endStream(w, fl, reason)
				return
			}
		}
	}
}

func ended(observed string) bool {
	return observed == "exited" || observed == "failed" || observed == "lost"
}

// recordingDownload streams central's segments as one zstd stream (concatenated frames of JSONL records).
func (d *Deps) recordingDownload(w http.ResponseWriter, r *http.Request, f *fleet.Service) {
	ctx := r.Context()
	spaceID, runID := r.PathValue("space_id"), r.PathValue("run_id")
	if _, err := d.spaceRun(ctx, f, spaceID, runID); err != nil {
		writeProblem(w, newProblem(http.StatusNotFound, "not_found", "run not found", nil))
		return
	}
	segs, err := f.Recordings().Segments(ctx, runID)
	if err != nil || len(segs) == 0 {
		writeProblem(w, newProblem(http.StatusNotFound, "not_found", "no recording stored for this run", nil))
		return
	}
	w.Header().Set("Content-Type", "application/zstd")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", runID+".smrec.zst"))
	for _, s := range segs {
		fh, err := f.Recordings().OpenSegment(ctx, runID, s)
		if err != nil {
			return
		}
		_, _ = io.Copy(w, fh)
		_ = fh.Close()
	}
}

// RawRoute is a plain net/http route (streams, downloads) with the same authorization as huma operations.
type RawRoute struct {
	Pattern string
	Perm    authz.Permission
	Handler func(d *Deps) http.HandlerFunc
}

// authorizeRaw applies a space-scoped permission to a raw route.
func (d *Deps) authorizeRaw(rt RawRoute) http.Handler {
	h := rt.Handler(d)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := PrincipalFrom(r.Context())
		if p == nil {
			writeProblem(w, errUnauthenticated())
			return
		}
		if p.Limited {
			writeProblem(w, errLimited())
			return
		}
		space := r.PathValue("space_id")
		if !authz.ValidDomain(space) || space == authz.Global || d.Authz == nil || !d.Authz.AllowedAll(p.Subjects(), space, rt.Perm) {
			writeProblem(w, newProblem(http.StatusNotFound, "not_found", "run not found", nil))
			return
		}
		h(w, r)
	})
}
