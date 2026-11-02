// Package usage keeps runs' normalized usage and their native transcripts (SM-REC-005, ADR-0028). Usage is only what
// a harness recorded: unknown is unknown, never zero, and a cost estimate keeps who made it and what it assumes.
// Transcripts are stored apart from terminal recordings, uploaded by the node after a run ends, and are as sensitive as
// recordings.
package usage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

var reRun = regexp.MustCompile(`^run_[0-9a-z]{26}$`)

// Usage is a run's normalized usage.
type Usage struct {
	RunID      string              `json:"run_id"`
	Role       string              `json:"role,omitempty"`
	Kind       string              `json:"kind,omitempty"`
	Harness    string              `json:"harness,omitempty"`
	Source     string              `json:"source" doc:"Where the numbers come from; unavailable means unknown"`
	Known      bool                `json:"known" doc:"Token counts are known"`
	Models     []proto.ModelUsage  `json:"models"`
	Estimate   *proto.CostEstimate `json:"estimate,omitempty"`
	Final      bool                `json:"final"`
	Note       string              `json:"note,omitempty"`
	Transcript *Transcript         `json:"transcript,omitempty"`
	UpdatedAt  time.Time           `json:"updated_at"`
	StartedAt  *time.Time          `json:"run_created_at,omitempty"`
}

// Transcript is a stored native transcript.
type Transcript struct {
	State     string `json:"state" enum:"receiving,stored,failed,deleted"`
	Size      int64  `json:"size"`
	Received  int64  `json:"received"`
	SHA256    string `json:"sha256,omitempty"`
	Truncated bool   `json:"truncated" doc:"Only the start of a longer transcript was kept"`
	Error     string `json:"error,omitempty"`
}

// Summary totals a space's known usage and counts what is unknown.
type Summary struct {
	Runs         []*Usage `json:"runs"`
	KnownRuns    int      `json:"known_runs"`
	UnknownRuns  int64    `json:"unknown_runs" doc:"Runs whose usage is not known (no records, or a harness without verified records)"`
	Input        int64    `json:"input_tokens" doc:"Sum over runs with known usage only"`
	Output       int64    `json:"output_tokens"`
	CacheWrite   int64    `json:"cache_write_tokens"`
	CacheRead    int64    `json:"cache_read_tokens"`
	EstimatedUSD float64  `json:"estimated_usd" doc:"Sum of the harnesses' own estimates where they gave one"`
	Estimated    int      `json:"estimated_runs"`
}

// Service stores usage and transcripts.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	dir     string
	log     *slog.Logger
	mu      sync.Mutex
}

// New returns the service; transcripts live under dir.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, dir string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{s: s, audit: aw, changes: ch, dir: dir, log: log}
}

func (svc *Service) owned(ctx context.Context, nodeID, runID string) (db.Run, error) {
	if !reRun.MatchString(runID) {
		return db.Run{}, errors.New("invalid run")
	}
	r, err := svc.s.R().GetRun(ctx, runID)
	if err != nil || r.NodeID != nodeID {
		return db.Run{}, errors.New("the run is not assigned to this node")
	}
	return r, nil
}

// Report stores a run's usage from its node. Once a final report is stored, later reports change nothing.
func (svc *Service) Report(ctx context.Context, nodeID string, rep proto.UsageReport) {
	r, err := svc.owned(ctx, nodeID, rep.RunID)
	if err != nil {
		return
	}
	if rep.Source != proto.UsageClaudeTranscript && rep.Source != proto.UsageUnavailable {
		return
	}
	if len(rep.Models) > 50 || len(rep.Note) > 500 {
		return
	}
	models, _ := json.Marshal(nonNil(rep.Models))
	est := ""
	if rep.Estimate != nil {
		b, _ := json.Marshal(rep.Estimate)
		est = string(b)
	}
	var tb sql.NullInt64
	if rep.Transcript != nil {
		tb = sql.NullInt64{Int64: rep.Transcript.Bytes, Valid: true}
	}
	final := int64(0)
	if rep.Final {
		final = 1
	}
	err = svc.s.W().TxNamed(ctx, "usage.report", func(tx *store.Tx) error {
		if err := tx.Q.UpsertRunUsage(ctx, db.UpsertRunUsageParams{RunID: r.ID, SpaceID: r.SpaceID, Source: rep.Source, ModelsJson: string(models), EstimateJson: est,
			TranscriptBytes: tb, Final: final, Note: rep.Note, UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		return svc.changes.Record(ctx, tx, r.SpaceID, "run", r.ID, "updated", 0)
	})
	if err != nil {
		svc.log.Warn("store usage", slog.String("run.id", r.ID), slog.Any("error", err))
	}
}

func nonNil(m []proto.ModelUsage) []proto.ModelUsage {
	if m == nil {
		return []proto.ModelUsage{}
	}
	return m
}

func (svc *Service) path(runID string) string { return filepath.Join(svc.dir, runID+".jsonl") }

// Chunk stores part of a run's transcript and says what to send next. Chunks arrive in order; a chunk at the wrong
// offset (a resend after a reconnect) is answered with the offset central has.
func (svc *Service) Chunk(ctx context.Context, nodeID string, ch proto.TranscriptChunk) proto.TranscriptAck {
	ack := proto.TranscriptAck{RunID: ch.RunID}
	r, err := svc.owned(ctx, nodeID, ch.RunID)
	if err != nil {
		ack.Error = err.Error()
		return ack
	}
	if ch.Size < 0 || ch.Size > proto.TranscriptMax || len(ch.Data) > proto.TranscriptChunkMax || ch.Offset < 0 {
		ack.Error = "invalid transcript chunk"
		return ack
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if err := svc.s.W().TxNamed(ctx, "usage.transcript_start", func(tx *store.Tx) error {
		return tx.Q.InsertRunTranscript(ctx, db.InsertRunTranscriptParams{RunID: r.ID, SpaceID: r.SpaceID, Size: ch.Size, Sha256: "", Truncated: b2i(ch.Truncated),
			CreatedAt: tx.NowMicros(), UpdatedAt: tx.NowMicros()})
	}); err != nil {
		ack.Error = "internal error"
		return ack
	}
	t, err := svc.s.R().GetRunTranscript(ctx, r.ID)
	if err != nil {
		ack.Error = "internal error"
		return ack
	}
	if t.State != "receiving" {
		ack.Next, ack.Done = t.Received, true
		return ack
	}
	if ch.Offset != t.Received {
		ack.Next = t.Received
		return ack
	}
	if err := os.MkdirAll(svc.dir, 0o700); err != nil {
		ack.Error = "internal error"
		return ack
	}
	f, err := os.OpenFile(svc.path(r.ID)+".part", os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // a validated run ID under the transcripts folder
	if err == nil {
		_, err = f.WriteAt(ch.Data, ch.Offset)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		ack.Error = "internal error"
		return ack
	}
	received := ch.Offset + int64(len(ch.Data))
	state, msg := "receiving", ""
	if ch.Final {
		switch sum, err := fileSHA(svc.path(r.ID) + ".part"); {
		case err != nil || received != ch.Size || sum != ch.SHA256:
			state, msg = "failed", "the transcript did not arrive intact"
			_ = os.Remove(svc.path(r.ID) + ".part")
		default:
			if err := os.Rename(svc.path(r.ID)+".part", svc.path(r.ID)); err != nil {
				state, msg = "failed", "internal error"
			} else {
				state = "stored"
			}
		}
	}
	err = svc.s.W().TxNamed(ctx, "usage.transcript", func(tx *store.Tx) error {
		if err := tx.Q.SetRunTranscriptProgress(ctx, db.SetRunTranscriptProgressParams{Received: received, State: state, Error: msg, UpdatedAt: tx.NowMicros(), RunID: r.ID}); err != nil {
			return err
		}
		if state == "stored" {
			if _, err := tx.SQL.ExecContext(ctx, `UPDATE run_transcripts SET sha256 = ? WHERE run_id = ?`, ch.SHA256, r.ID); err != nil {
				return err
			}
		}
		return svc.changes.Record(ctx, tx, r.SpaceID, "run", r.ID, "updated", 0)
	})
	if err != nil {
		ack.Error = "internal error"
		return ack
	}
	ack.Next, ack.Done = received, state != "receiving"
	if state == "failed" {
		ack.Error = msg
	}
	return ack
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func fileSHA(p string) (string, error) {
	f, err := os.Open(p) //nolint:gosec // our own file
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func usageOut(runID, source, models, est string, tb sql.NullInt64, final int64, note string, updated int64) *Usage {
	u := &Usage{RunID: runID, Source: source, Models: []proto.ModelUsage{}, Final: final == 1, Note: note, UpdatedAt: time.UnixMicro(updated).UTC()}
	_ = json.Unmarshal([]byte(models), &u.Models)
	u.Known = source != proto.UsageUnavailable
	if est != "" {
		var e proto.CostEstimate
		if json.Unmarshal([]byte(est), &e) == nil {
			u.Estimate = &e
		}
	}
	return u
}

func (svc *Service) transcript(ctx context.Context, runID string) *Transcript {
	t, err := svc.s.R().GetRunTranscript(ctx, runID)
	if err != nil {
		return nil
	}
	return &Transcript{State: t.State, Size: t.Size, Received: t.Received, SHA256: t.Sha256, Truncated: t.Truncated == 1, Error: t.Error}
}

// ForRun returns a run's usage. A run without a report has unknown usage.
func (svc *Service) ForRun(ctx context.Context, spaceID, runID string) (*Usage, error) {
	r, err := svc.s.R().GetRun(ctx, runID)
	if err != nil || r.SpaceID != spaceID {
		return nil, apperr.New(apperr.CodeNotFound, "run not found")
	}
	u := &Usage{RunID: runID, Source: proto.UsageUnavailable, Models: []proto.ModelUsage{}, Note: "no usage reported yet"}
	if row, err := svc.s.R().GetRunUsage(ctx, runID); err == nil {
		u = usageOut(row.RunID, row.Source, row.ModelsJson, row.EstimateJson, row.TranscriptBytes, row.Final, row.Note, row.UpdatedAt)
	}
	u.Role, u.Kind, u.Harness = r.Role, r.Kind, r.Harness
	u.Transcript = svc.transcript(ctx, runID)
	return u, nil
}

// Space summarizes a space's usage over its most recent runs.
func (svc *Service) Space(ctx context.Context, spaceID string) (*Summary, error) {
	rows, err := svc.s.R().ListSpaceUsage(ctx, db.ListSpaceUsageParams{SpaceID: spaceID, Limit: 500})
	if err != nil {
		return nil, err
	}
	missing, err := svc.s.R().CountRunsWithoutUsage(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	s := &Summary{Runs: []*Usage{}, UnknownRuns: missing}
	for _, r := range rows {
		u := usageOut(r.RunID, r.Source, r.ModelsJson, r.EstimateJson, r.TranscriptBytes, r.Final, r.Note, r.UpdatedAt)
		u.Role, u.Kind, u.Harness = r.Role, r.Kind, r.Harness
		t := time.UnixMicro(r.RunCreatedAt).UTC()
		u.StartedAt = &t
		s.Runs = append(s.Runs, u)
		if !u.Known {
			s.UnknownRuns++
			continue
		}
		s.KnownRuns++
		for _, m := range u.Models {
			s.Input += val(m.Input)
			s.Output += val(m.Output)
			s.CacheWrite += val(m.CacheWrite)
			s.CacheRead += val(m.CacheRead)
		}
		if u.Estimate != nil {
			s.EstimatedUSD += u.Estimate.USD
			s.Estimated++
		}
	}
	return s, nil
}

func val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// OpenTranscript opens a stored transcript for download.
func (svc *Service) OpenTranscript(ctx context.Context, spaceID, runID string) (*os.File, *Transcript, error) {
	r, err := svc.s.R().GetRun(ctx, runID)
	if err != nil || r.SpaceID != spaceID {
		return nil, nil, apperr.New(apperr.CodeNotFound, "run not found")
	}
	t := svc.transcript(ctx, runID)
	if t == nil || t.State != "stored" {
		return nil, nil, apperr.New(apperr.CodeNotFound, "no stored transcript")
	}
	f, err := os.Open(svc.path(runID))
	if err != nil {
		return nil, nil, apperr.New(apperr.CodeNotFound, "no stored transcript")
	}
	return f, t, nil
}

// DeleteTranscript removes a stored transcript (the record that it existed stays).
func (svc *Service) DeleteTranscript(ctx context.Context, actor audit.Actor, spaceID, runID string) error {
	r, err := svc.s.R().GetRun(ctx, runID)
	if err != nil || r.SpaceID != spaceID {
		return apperr.New(apperr.CodeNotFound, "run not found")
	}
	t := svc.transcript(ctx, runID)
	if t == nil || t.State == "deleted" {
		return apperr.New(apperr.CodeNotFound, "no transcript")
	}
	if err := os.Remove(svc.path(runID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove transcript: %w", err)
	}
	return svc.s.W().TxNamed(ctx, "usage.transcript_delete", func(tx *store.Tx) error {
		if err := tx.Q.SetRunTranscriptProgress(ctx, db.SetRunTranscriptProgressParams{Received: 0, State: "deleted", Error: "", UpdatedAt: tx.NowMicros(), RunID: runID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "run", runID, "updated", 0); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "transcript.deleted", Target: audit.Target{Type: "run", ID: runID},
			Details: map[string]any{"space_id": spaceID}})
		return err
	})
}
