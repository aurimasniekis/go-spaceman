// Package usage keeps runs' normalized usage and their native transcripts (SM-REC-005, ADR-0028). Usage is only what
// a harness recorded: unknown is unknown, never zero, and a cost estimate keeps who made it and what it assumes.
// Transcripts are stored apart from terminal recordings, uploaded by the node after a run ends, and are as sensitive as
// recordings. A transcript is received into a local .part file and, once complete and checked, handed to the artifact
// store (ADR-0048): renamed into place on the local disk, or uploaded to S3. A transcript whose upload failed stays in
// the local folder (served from there) until UploadSpooled moves it at the next start.
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
	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
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
	Gateway    *GatewayUsage       `json:"gateway,omitempty" doc:"What the LLM gateway (Harbor Pilot) observed for a gateway run, read back after it ended"`
	UpdatedAt  time.Time           `json:"updated_at"`
	StartedAt  *time.Time          `json:"run_created_at,omitempty"`
}

// GatewayUsage is a gateway run's usage as Harbor Pilot observed it (ADR-0060 decision 7): read back from
// usage/summary?ref=spaceman.run:<run> after the run's token was given up. It is shown next to what the harness
// reported, and fills the run's usage (source harbor-pilot) only where the harness reported none.
type GatewayUsage struct {
	Source       string             `json:"source" doc:"Always harbor-pilot"`
	State        string             `json:"state" enum:"pending,final,failed,unavailable" doc:"pending: more reads are due, figures may still grow; final: the last read is stored; failed and unavailable: see note"`
	Models       []proto.ModelUsage `json:"models"`
	Costs        []GatewayCost      `json:"costs" doc:"Harbor Pilot's cost totals per category and currency (categories are never summed)"`
	Requests     int64              `json:"requests" doc:"Attempts Harbor Pilot recorded"`
	UnknownUsage int64              `json:"unknown_usage_requests" doc:"Attempts whose token usage the provider did not report (not counted as zero)"`
	ReadAt       *time.Time         `json:"read_at,omitempty"`
	Note         string             `json:"note,omitempty"`
}

// GatewayCost is a cost total in one Harbor Pilot category (estimated_api_charge, actual_api_charge, …).
type GatewayCost struct {
	Kind     string `json:"kind"`
	Currency string `json:"currency"`
	Amount   string `json:"amount" doc:"Decimal amount"`
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
	// spool is the local folder as a store; store keeps stored transcripts (the spool itself when local).
	spool   *artifacts.Local
	store   artifacts.Store
	archive bool
	log     *slog.Logger
	mu      sync.Mutex
}

// New returns the service; transcripts are received under dir and kept in store (nil: a local store rooted at dir).
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, dir string, store artifacts.Store, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	spool := artifacts.NewLocal(map[string]string{artifacts.NSTranscripts: dir})
	archive := true
	if store == nil {
		store, archive = spool, false
	} else if l, ok := store.(*artifacts.Local); ok && l.Root(artifacts.NSTranscripts) == filepath.Clean(dir) {
		archive = false
	}
	return &Service{s: s, audit: aw, changes: ch, dir: dir, spool: spool, store: store, archive: archive, log: log}
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
			if err := svc.keep(ctx, r.ID, received, sum); err != nil {
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

// keep hands a complete, checked transcript to the artifact store. When the store refuses it (S3 unavailable) the file
// stays in the local folder, from where it is served and uploaded again later (UploadSpooled).
func (svc *Service) keep(ctx context.Context, runID string, size int64, sum string) error {
	part := svc.path(runID) + ".part"
	err := artifacts.PutFile(ctx, svc.store, artifacts.TranscriptKey(runID), part, size, sum)
	if err == nil || !svc.archive {
		return err
	}
	svc.log.Warn("transcript kept on the local disk: the artifact store refused it; it is uploaded at the next start",
		slog.String("run.id", runID), slog.Any("error", err))
	return os.Rename(part, svc.path(runID))
}

// UploadSpooled moves complete transcripts left in the local folder (failed uploads) to the artifact store. It returns
// how many it moved; with the local store there is nothing to move.
func (svc *Service) UploadSpooled(ctx context.Context) (int, error) {
	if !svc.archive {
		return 0, nil
	}
	infos, err := svc.spool.List(ctx, artifacts.NSTranscripts+"/")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, i := range infos {
		p, _ := svc.spool.Path(i.Key)
		svc.mu.Lock()
		err := artifacts.PutFile(ctx, svc.store, i.Key, p, 0, "")
		svc.mu.Unlock()
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
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
	u.Gateway = svc.gateway(ctx, runID)
	return u, nil
}

// gateway returns a gateway run's read-back usage (nil: not a gateway run, or not queued yet).
func (svc *Service) gateway(ctx context.Context, runID string) *GatewayUsage {
	g, err := svc.s.R().GetRunGatewayUsage(ctx, runID)
	if err != nil {
		return nil
	}
	out := &GatewayUsage{Source: proto.UsageHarborPilot, State: g.State, Models: []proto.ModelUsage{}, Costs: []GatewayCost{}, Requests: g.Attempts,
		UnknownUsage: g.UnknownUsage, Note: g.LastError}
	_ = json.Unmarshal([]byte(g.ModelsJson), &out.Models)
	_ = json.Unmarshal([]byte(g.CostsJson), &out.Costs)
	if g.ReadAt.Valid {
		t := time.UnixMicro(g.ReadAt.Int64).UTC()
		out.ReadAt = &t
	}
	if out.Models == nil {
		out.Models = []proto.ModelUsage{}
	}
	if out.Costs == nil {
		out.Costs = []GatewayCost{}
	}
	return out
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

// OpenTranscript opens a stored transcript for download. A full read of damaged content fails at its end.
func (svc *Service) OpenTranscript(ctx context.Context, spaceID, runID string) (artifacts.Object, *Transcript, error) {
	r, err := svc.s.R().GetRun(ctx, runID)
	if err != nil || r.SpaceID != spaceID {
		return nil, nil, apperr.New(apperr.CodeNotFound, "run not found")
	}
	t := svc.transcript(ctx, runID)
	if t == nil || t.State != "stored" {
		return nil, nil, apperr.New(apperr.CodeNotFound, "no stored transcript")
	}
	key := artifacts.TranscriptKey(runID)
	o, err := svc.spool.Open(ctx, key)
	if err != nil && svc.archive {
		o, err = svc.store.Open(ctx, key)
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, apperr.New(apperr.CodeNotFound, "no stored transcript")
	} else if err != nil {
		return nil, nil, err
	}
	if artifacts.ValidSum(t.SHA256) {
		o = artifacts.Verified(o, t.SHA256)
	}
	return o, t, nil
}

// PurgeTranscripts deletes the transcripts of runs whose records were purged with their space: local files and
// objects in the artifact store. Failures are logged.
func (svc *Service) PurgeTranscripts(ctx context.Context, runIDs []string) {
	for _, runID := range runIDs {
		if !reRun.MatchString(runID) {
			continue
		}
		for _, p := range []string{svc.path(runID), svc.path(runID) + ".part"} {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				svc.log.Warn("purge: transcript not deleted", slog.String("run.id", runID), slog.Any("error", err))
			}
		}
		if svc.archive {
			if err := svc.store.Delete(ctx, artifacts.TranscriptKey(runID)); err != nil {
				svc.log.Warn("purge: transcript not deleted from the artifact store", slog.String("run.id", runID), slog.Any("error", err))
			}
		}
	}
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
	if svc.archive {
		if err := svc.store.Delete(ctx, artifacts.TranscriptKey(runID)); err != nil {
			return fmt.Errorf("remove transcript: %w", err)
		}
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
