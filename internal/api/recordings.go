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
			}) (*struct{ Body RecordingPage }, error) {
				if _, err := d.spaceRun(ctx, f, in.SpaceID, in.RunID); err != nil {
					return nil, err
				}
				if in.Limit == 0 {
					in.Limit = 1000
				}
				res, err := f.Recordings().Read(ctx, in.RunID, in.After)
				if err != nil {
					return nil, d.fail(ctx, "recordings.get", err)
				}
				out := &struct{ Body RecordingPage }{Body: RecordingPage{Records: []recording.Record{}, Truncated: res.Truncated, Gaps: res.Gaps, NextAfter: in.After}}
				for i, r := range res.Records {
					if i == in.Limit {
						out.Body.More = true
						break
					}
					out.Body.Records = append(out.Body.Records, r)
					out.Body.NextAfter = r.Seq
				}
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
//
// Events: "record" (data: recording.Record JSON, id: seq), "gap", "heartbeat", and "end": data {} when the run ended, or
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
		res, err := f.Recordings().Read(ctx, runID, after)
		if err != nil {
			return false
		}
		for _, rec := range res.Records {
			if send(rec) != nil {
				return false
			}
			after = rec.Seq
		}
		fl.Flush()
		return true
	}
	if !catchUp() {
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
