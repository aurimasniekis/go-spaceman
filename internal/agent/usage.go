package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// Usage and native transcripts (SM-REC-005). Usage comes only from what the harness itself records: for Claude Code,
// its session transcript ($CLAUDE_CONFIG_DIR/projects/<dir>/<session>.jsonl, verified in spike S-7) gives token counts
// per request and model, and Claude Code's own cost estimate. Codex's records are not verified, so its usage is
// reported as unavailable, never as zero. After a run ends its transcript is uploaded to central in acknowledged
// chunks, resumable after a reconnect.

const usageDebounce = 30 * time.Second

var usageTimers sync.Map // run ID → *time.Timer

// transcriptOf finds a run's native transcript (absolute path and its path under the space's instances folder).
func (a *Agent) transcriptOf(r runRow) (string, string) {
	if r.Harness != harness.KindClaude || r.SessionID == "" || r.InstanceID == "" {
		return "", ""
	}
	l, _ := a.lease(r.SpaceID, r.Generation)
	if l.Workdir == "" {
		return "", ""
	}
	base := filepath.Join(a.spaceDir(l.Workdir), "instances")
	matches, _ := filepath.Glob(filepath.Join(base, r.InstanceID, "claude", "projects", "*", r.SessionID+".jsonl"))
	if len(matches) == 0 {
		return "", ""
	}
	rel, _ := filepath.Rel(base, matches[0])
	return matches[0], rel
}

type claudeRecord struct {
	Type    string `json:"type"`
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	TotalCostUSD        *float64 `json:"totalCostUSD"`
	HasUnknownModelCost bool     `json:"hasUnknownModelCost"`
}

// claudeUsage reads token counts per model from a Claude Code transcript. A request's records repeat while it streams,
// so the last usage per message ID counts.
func claudeUsage(path string) ([]proto.ModelUsage, *proto.CostEstimate, error) {
	f, err := os.Open(path) //nolint:gosec // the run's own transcript in its instance folder
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	type counts struct {
		model           string
		in, out, cw, cr int64
	}
	byID := map[string]counts{}
	var est *proto.CostEstimate
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var r claudeRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		switch {
		case r.Type == "assistant" && r.Message.Usage != nil && r.Message.ID != "":
			u := r.Message.Usage
			byID[r.Message.ID] = counts{r.Message.Model, u.Input, u.Output, u.CacheWrite, u.CacheRead}
		case r.Type == "cost-state" && r.TotalCostUSD != nil:
			est = &proto.CostEstimate{USD: *r.TotalCostUSD, Source: "Claude Code (cost-state record in its transcript)",
				Assumptions: "Claude Code's own estimate at API list prices; subscription accounts are not billed per token", UnknownModels: r.HasUnknownModelCost}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	per := map[string]*proto.ModelUsage{}
	for _, c := range byID {
		m := per[c.model]
		if m == nil {
			z := func() *int64 { v := int64(0); return &v }
			m = &proto.ModelUsage{Model: c.model, Input: z(), Output: z(), CacheWrite: z(), CacheRead: z()}
			per[c.model] = m
		}
		*m.Input += c.in
		*m.Output += c.out
		*m.CacheWrite += c.cw
		*m.CacheRead += c.cr
		m.Requests++
	}
	out := make([]proto.ModelUsage, 0, len(per))
	for _, m := range per {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, est, nil
}

// usageReport builds a run's report from its transcript.
func (a *Agent) usageReport(r runRow, final bool) proto.UsageReport {
	rep := proto.UsageReport{RunID: r.RunID, Source: proto.UsageUnavailable, Models: []proto.ModelUsage{}, Final: final}
	if r.Harness != harness.KindClaude {
		rep.Note = "this harness's usage records are not verified; usage is unknown"
		return rep
	}
	abs, rel := a.transcriptOf(r)
	if abs == "" {
		rep.Note = "no transcript found for the run's session"
		return rep
	}
	models, est, err := claudeUsage(abs)
	if err != nil {
		rep.Note = "the transcript could not be read"
		return rep
	}
	rep.Source, rep.Models, rep.Estimate = proto.UsageClaudeTranscript, models, est
	if fi, err := os.Stat(abs); err == nil {
		rep.Transcript = &proto.TranscriptRef{Path: rel, Bytes: fi.Size()}
	}
	return rep
}

func (a *Agent) protocolAtLeast(minor int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connected && a.minor >= minor
}

// usageKick reports usage after the run has been idle for a while (debounced per run).
func (a *Agent) usageKick(ctx context.Context, r runRow) {
	if v, ok := usageTimers.Load(r.RunID); ok {
		v.(*time.Timer).Reset(usageDebounce)
		return
	}
	t := time.AfterFunc(usageDebounce, func() {
		usageTimers.Delete(r.RunID)
		if cur, err := a.j.getRun(ctx, r.RunID); err == nil && !ended(cur.State) && a.protocolAtLeast(proto.MinorMemory) {
			_ = a.send(ctx, proto.TypeUsage, a.usageReport(cur, false))
		}
	})
	usageTimers.Store(r.RunID, t)
}

// usageFinal reports a run's final usage and starts uploading its transcript.
func (a *Agent) usageFinal(ctx context.Context, r runRow) {
	if v, ok := usageTimers.LoadAndDelete(r.RunID); ok {
		v.(*time.Timer).Stop()
	}
	rep := a.usageReport(r, true)
	if a.protocolAtLeast(proto.MinorMemory) {
		_ = a.send(ctx, proto.TypeUsage, rep)
	}
	abs, rel := a.transcriptOf(r)
	if abs == "" {
		return
	}
	size, sum, truncated, err := hashPrefix(abs, proto.TranscriptMax)
	if err != nil {
		return
	}
	if err := a.j.s.W().TxNamed(ctx, "journal.transcript", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO transcript_uploads (run_id, path, size, sha256, truncated, updated_at) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (run_id) DO NOTHING`, r.RunID, rel, size, sum, boolToInt(truncated), tx.NowMicros())
		return err
	}); err != nil {
		return
	}
	a.sendTranscriptChunk(ctx, r.RunID)
}

// hashPrefix returns the size (capped), the SHA-256 of those bytes and whether the file was longer.
func hashPrefix(path string, limit int64) (int64, string, bool, error) {
	f, err := os.Open(path) //nolint:gosec // the run's own transcript
	if err != nil {
		return 0, "", false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, "", false, err
	}
	n := min(fi.Size(), limit)
	h := sha256.New()
	if _, err := io.CopyN(h, f, n); err != nil {
		return 0, "", false, err
	}
	return n, hex.EncodeToString(h.Sum(nil)), fi.Size() > limit, nil
}

type upload struct {
	run, path, sha  string
	size, next      int64
	truncated, done bool
}

func (a *Agent) upload(ctx context.Context, runID string) (upload, bool) {
	var u upload
	var tr, done int64
	err := a.j.s.ReadDB().QueryRowContext(ctx, `SELECT run_id, path, size, sha256, truncated, next, done FROM transcript_uploads WHERE run_id = ?`, runID).
		Scan(&u.run, &u.path, &u.size, &u.sha, &tr, &u.next, &done)
	u.truncated, u.done = tr == 1, done == 1
	return u, err == nil
}

// sendTranscriptChunk sends the next chunk of a pending upload (stop-and-wait: the ack asks for the next one).
func (a *Agent) sendTranscriptChunk(ctx context.Context, runID string) {
	if !a.protocolAtLeast(proto.MinorMemory) {
		return
	}
	u, ok := a.upload(ctx, runID)
	if !ok || u.done {
		return
	}
	r, err := a.j.getRun(ctx, runID)
	if err != nil {
		return
	}
	l, _ := a.lease(r.SpaceID, r.Generation)
	if l.Workdir == "" {
		return
	}
	f, err := os.Open(filepath.Join(a.spaceDir(l.Workdir), "instances", u.path)) //nolint:gosec // the run's own transcript
	if err != nil {
		return
	}
	defer f.Close()
	n := min(int64(proto.TranscriptChunkMax), u.size-u.next)
	buf := make([]byte, max(n, 0))
	if n > 0 {
		if _, err := f.ReadAt(buf, u.next); err != nil && err != io.EOF {
			return
		}
	}
	final := u.next+n >= u.size
	ch := proto.TranscriptChunk{RunID: runID, Offset: u.next, Data: buf, Final: final, Size: u.size, Truncated: u.truncated}
	if final {
		ch.SHA256 = u.sha
	}
	_ = a.send(ctx, proto.TypeTranscript, ch)
}

// transcriptAck records central's progress and sends the next chunk.
func (a *Agent) transcriptAck(ctx context.Context, ack proto.TranscriptAck) {
	if err := a.j.s.W().TxNamed(ctx, "journal.transcript_ack", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE transcript_uploads SET next = ?, done = ?, updated_at = ? WHERE run_id = ?`,
			ack.Next, boolToInt(ack.Done || ack.Error != ""), tx.NowMicros(), ack.RunID)
		return err
	}); err != nil {
		return
	}
	if !ack.Done && ack.Error == "" {
		a.sendTranscriptChunk(ctx, ack.RunID)
	}
}

// resumeTranscripts continues unfinished uploads after connecting.
func (a *Agent) resumeTranscripts(ctx context.Context) {
	runs, err := a.j.pendingUploads(ctx)
	if err != nil {
		return
	}
	for _, id := range runs {
		a.sendTranscriptChunk(ctx, id)
	}
}

func (j *Journal) pendingUploads(ctx context.Context) ([]string, error) {
	rows, err := j.s.ReadDB().QueryContext(ctx, `SELECT run_id FROM transcript_uploads WHERE done = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		runs = append(runs, id)
	}
	return runs, rows.Err()
}
