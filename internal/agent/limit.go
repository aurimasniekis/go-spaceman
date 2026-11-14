package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Usage-limit detection (ADR-0079 decision 1). When a Claude Code run's status turns idle or error, and once when it
// ends, the agent scans the run's transcript from the offset it reached last time and reads only typed fields. The
// limit it finds is journaled with that offset (a restarted agent resumes the scan and re-reports the same since) and
// rides on every run report. Nothing read from the transcript is logged or sent except the allowlisted kind, the
// reset epoch and the record's timestamp, all normalized (proto.RunLimit.Normalized).
//
// Signals (docs/contracts/harnesses.md, "Claude Code usage limits"):
//   - set: an assistant record with apiError "usage_limit_reached" (kind rateLimitType, reset resetsAt in epoch
//     seconds); a meta user record with usageLimitNote "wrap_up" (no kind, no reset); a Harbor Pilot 429 API-error
//     record whose message starts with one of Harbor Pilot's two anchored refusals (gateway_quota, gateway_budget).
//   - cleared: an auto-continuation turn (origin.kind "auto-continuation"), or an assistant message with non-zero
//     usage after the limit, outside the turn the wrap-up note was written in (that turn finishes on a grace allowance).
//
// The auto-continue "armed" and "reset" informational records are not used: they share their type and subtype and
// differ only in display wording, which is never parsed (SM-RUN-002).

const (
	// limitLineMax is the longest record the scan decodes; longer ones (large tool results) are skipped unread.
	limitLineMax = 1 << 20
	// limitScanMax bounds the bytes one scan reads; the rest is read by the next scan.
	limitScanMax = 64 << 20
	// limitTextMax bounds the API-error text compared with Harbor Pilot's refusals.
	limitTextMax = 512
	// gatewayRefusal is Claude Code's prefix for an API error it shows with the gateway's own message.
	gatewayRefusal = "API Error: Request rejected (429) · "
)

// limitSettle is how long after an idle or error status the scan waits (debounced per run), so the records Claude Code
// writes as the turn ends are on disk. A variable for tests.
var limitSettle = 2 * time.Second

// gatewayBudget is Harbor Pilot's blocking-budget refusal (harbor-pilot.md): "budget <name> is exhausted …".
var gatewayBudget = regexp.MustCompile(`^budget .+ is exhausted `)

// runLimit is a run's usage limit as journaled. Kind "" with Since set is a limit that cleared; Since nil is none ever
// seen. Offset is how far the transcript has been scanned.
type runLimit struct {
	Kind     string
	ResetsAt *time.Time
	Since    *time.Time
	Offset   int64
}

// same reports whether two limits report the same thing (the offset aside).
func (l runLimit) same(o runLimit) bool {
	eq := func(a, b *time.Time) bool { return (a == nil) == (b == nil) && (a == nil || a.Equal(*b)) }
	return l.Kind == o.Kind && eq(l.ResetsAt, o.ResetsAt) && eq(l.Since, o.Since)
}

// wire returns the limit as a run report carries it, normalized at now; nil when the run never had one.
func (l runLimit) wire(now time.Time) *proto.RunLimit {
	if l.Since == nil {
		return nil
	}
	out, ok := proto.RunLimit{Kind: l.Kind, ResetsAt: l.ResetsAt, Since: *l.Since, Cleared: l.Kind == ""}.Normalized(now)
	if !ok {
		return nil
	}
	return &out
}

// normalized returns l as it is journaled: bounded like the wire form, so what a restarted agent re-reports is what
// it reported before.
func (l runLimit) normalized(now time.Time) runLimit {
	w := l.wire(now)
	if w == nil {
		return runLimit{Offset: l.Offset}
	}
	since := w.Since
	return runLimit{Kind: w.Kind, ResetsAt: w.ResetsAt, Since: &since, Offset: l.Offset}
}

type quotaInfo struct {
	ResetsAt      *float64 `json:"resetsAt"`
	RateLimitType string   `json:"rateLimitType"`
}

// limitRecord is the typed part of a transcript record the scan reads. Message content is kept raw and only decoded
// for a 429 API-error record.
type limitRecord struct {
	Type              string `json:"type"`
	IsMeta            bool   `json:"isMeta"`
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	APIError          string `json:"apiError"`
	APIErrorStatus    int    `json:"apiErrorStatus"`
	APIErrorParams    *struct {
		RateLimitInfo *quotaInfo `json:"rate_limit_info"`
	} `json:"apiErrorParams"`
	QuotaLimits    *quotaInfo `json:"quotaLimits"`
	UsageLimitNote string     `json:"usageLimitNote"`
	Origin         *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Timestamp     string          `json:"timestamp"`
	Message       struct {
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input      float64 `json:"input_tokens"`
			Output     float64 `json:"output_tokens"`
			CacheWrite float64 `json:"cache_creation_input_tokens"`
			CacheRead  float64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// limitParser folds transcript records into a run's limit.
type limitParser struct {
	lim runLimit
	// wrapUp is set while the turn a wrap-up note was written in is still running; wrapAt is where that note's line
	// starts. A scan that ends inside such a turn stops its offset there, so the next scan reads the note again and
	// the turn's grace-allowance replies still don't clear the limit. Replaying from it is idempotent: until the turn
	// ends, the records after the note don't change the limit.
	wrapUp bool
	wrapAt int64
	now    time.Time
	// records counts the records decoded; skipped those that were not JSON objects or too long.
	records, skipped int
}

func (p *limitParser) set(kind string, reset *time.Time, ts string) {
	if p.lim.Kind == "" {
		since := p.now
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			since = t.UTC()
		}
		p.lim.Since = &since
	}
	p.lim.Kind, p.lim.ResetsAt = kind, reset
}

func (p *limitParser) clear() {
	if p.lim.Kind != "" {
		p.lim.Kind, p.lim.ResetsAt = "", nil
	}
}

// resetOf returns a reset epoch in seconds as a time; anything that is not a plausible epoch in seconds (a
// millisecond value, a negative one) is unknown. Normalized bounds it further against since.
func resetOf(q ...*quotaInfo) (*time.Time, string) {
	for _, v := range q {
		if v == nil {
			continue
		}
		var reset *time.Time
		if r := v.ResetsAt; r != nil && *r > 0 && *r < 1e11 {
			t := time.Unix(int64(*r), 0).UTC()
			reset = &t
		}
		return reset, v.RateLimitType
	}
	return nil, ""
}

// gatewayKind classifies a Harbor Pilot 429 by its message (anchored); "" is not a limit.
func gatewayKind(content json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil || len(blocks) == 0 || blocks[0].Type != "text" {
		return ""
	}
	text, ok := strings.CutPrefix(blocks[0].Text, gatewayRefusal)
	if !ok {
		return ""
	}
	if len(text) > limitTextMax {
		text = text[:limitTextMax]
	}
	switch {
	case strings.HasPrefix(text, "no eligible account is available for model "):
		return proto.LimitGatewayQuota
	case gatewayBudget.MatchString(text):
		return proto.LimitGatewayBudget
	}
	return ""
}

// turnStart reports whether a user record starts a turn: typed input or a task notification, not a tool result or a
// meta record.
func (r *limitRecord) turnStart() bool {
	if r.IsMeta || len(r.ToolUseResult) > 0 {
		return false
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(r.Message.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return false
			}
		}
	}
	return true
}

// feed folds one transcript line into the limit.
func (p *limitParser) feed(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var r limitRecord
	if json.Unmarshal(line, &r) != nil {
		p.skipped++
		return
	}
	p.records++
	switch r.Type {
	case "assistant":
		u := r.Message.Usage
		switch {
		case r.APIError == "usage_limit_reached":
			reset, kind := resetOf(r.QuotaLimits, rateLimitInfo(&r))
			p.set(proto.LimitKind(kind), reset, r.Timestamp)
			p.wrapUp = false
		case r.IsAPIErrorMessage && r.APIErrorStatus == 429 && r.APIError == "":
			if k := gatewayKind(r.Message.Content); k != "" {
				p.set(k, nil, r.Timestamp)
				p.wrapUp = false
			}
		case !r.IsAPIErrorMessage && u != nil && u.Input+u.Output+u.CacheWrite+u.CacheRead > 0 && !p.wrapUp:
			p.clear()
		}
	case "user":
		switch {
		case r.UsageLimitNote == "wrap_up":
			if p.lim.Kind == "" {
				p.set(proto.LimitUnknown, nil, r.Timestamp)
			}
			p.wrapUp = true
		case r.Origin != nil && r.Origin.Kind == "auto-continuation":
			p.clear()
			p.wrapUp = false
		case r.turnStart():
			p.wrapUp = false
		}
	}
}

func rateLimitInfo(r *limitRecord) *quotaInfo {
	if r.APIErrorParams == nil {
		return nil
	}
	return r.APIErrorParams.RateLimitInfo
}

// scanLimit reads path from lim.Offset (from the start when the file is shorter, so truncated or replaced) and
// returns the limit after the complete lines read, with the offset after the last of them. replaced forces a scan
// from the start. It reads at most limitScanMax bytes and keeps at most one line of limitLineMax in memory.
func scanLimit(path string, lim runLimit, replaced bool, now time.Time) (limitParser, error) {
	p := limitParser{lim: lim, now: now}
	f, err := os.Open(path) //nolint:gosec // the run's own transcript in its instance folder
	if err != nil {
		return p, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return p, err
	}
	if replaced || fi.Size() < p.lim.Offset || p.lim.Offset < 0 {
		p.lim.Offset = 0
	}
	if _, err := f.Seek(p.lim.Offset, io.SeekStart); err != nil {
		return p, err
	}
	br := bufio.NewReaderSize(io.LimitReader(f, limitScanMax), 64<<10)
	var line []byte
	var n, read int64 // bytes of the current line, and of the whole scan
	long := false     // inside a line longer than limitLineMax: skipped up to its newline
	for {
		chunk, err := br.ReadSlice('\n')
		n += int64(len(chunk))
		read += int64(len(chunk))
		if !long {
			if len(line)+len(chunk) > limitLineMax {
				long, line = true, line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case err == nil:
			start := p.lim.Offset
			p.lim.Offset += n
			if long {
				p.skipped++
			} else {
				was := p.wrapUp
				p.feed(line)
				if p.wrapUp && !was {
					p.wrapAt = start
				}
			}
			line, n, long = line[:0], 0, false
		case errors.Is(err, bufio.ErrBufferFull):
		default:
			// EOF: an unterminated last line is read again next time, unless it is an over-long one that used up
			// the whole scan budget (it would never be passed otherwise).
			if long && read >= limitScanMax {
				p.lim.Offset += n
				p.skipped++
			}
			if p.wrapUp {
				p.lim.Offset = p.wrapAt
			}
			return p, nil
		}
	}
}

// limitScanned reports whether a status ends a turn, so the transcript may hold a new limit record.
func limitScanned(status string) bool {
	return status == "idle" || status == "done" || status == "error"
}

// limitKick schedules a run's limit scan limitSettle after its status turned idle, done or error (debounced per run).
func (a *Agent) limitKick(ctx context.Context, runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.limitTimers[runID]; ok {
		t.Reset(limitSettle)
		return
	}
	if a.limitTimers == nil {
		a.limitTimers = map[string]*time.Timer{}
	}
	a.limitTimers[runID] = time.AfterFunc(limitSettle, func() {
		a.mu.Lock()
		delete(a.limitTimers, runID)
		a.mu.Unlock()
		a.track(func() { a.limitScan(ctx, runID, false) })
	})
}

// limitScan scans a run's transcript from its journaled offset, journals the limit and the new offset, and sends one
// run report when the limit changed. final is the scan at run end: it replaces a pending one.
func (a *Agent) limitScan(ctx context.Context, runID string, final bool) {
	if final {
		a.mu.Lock()
		if t, ok := a.limitTimers[runID]; ok {
			t.Stop()
			delete(a.limitTimers, runID)
		}
		a.mu.Unlock()
	}
	a.limitMu.Lock()
	defer a.limitMu.Unlock()
	r, err := a.j.getRun(ctx, runID)
	if err != nil || r.Harness != harness.KindClaude || (!final && ended(r.State)) {
		return
	}
	abs, _ := a.transcriptOf(ctx, r)
	if abs == "" {
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return
	}
	prev, seen := a.limitFiles[runID]
	replaced := seen && !os.SameFile(prev, fi)
	now := a.now()
	p, err := scanLimit(abs, r.Limit, replaced, now)
	if err != nil {
		a.log.Debug("usage-limit scan failed", slog.String(obs.FieldRunID, runID))
		return
	}
	if final {
		delete(a.limitFiles, runID)
	} else {
		if a.limitFiles == nil {
			a.limitFiles = map[string]os.FileInfo{}
		}
		a.limitFiles[runID] = fi
	}
	next := p.lim.normalized(now)
	if next.same(r.Limit) && next.Offset == r.Limit.Offset {
		return
	}
	a.limitSend.Lock()
	defer a.limitSend.Unlock()
	if err := a.j.setRunLimit(ctx, runID, next); err != nil || next.same(r.Limit) {
		return
	}
	// Shapes and counts only: never the transcript's text.
	a.log.Info("usage limit changed", slog.String(obs.FieldRunID, runID), slog.Bool("open", next.Kind != ""),
		slog.Bool("reset_known", next.ResetsAt != nil), slog.Int("records", p.records), slog.Int("skipped", p.skipped))
	// A central below minor 12 would get the report without its limit: nothing to tell it. A disconnected node's
	// next hello carries the journaled limit.
	if !a.protocolAtLeast(proto.MinorUsageLimit) {
		return
	}
	if cur, err := a.j.getRun(ctx, runID); err == nil {
		a.sendReport(ctx, cur)
	}
}
