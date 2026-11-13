package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	rt "github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
)

var limitNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return limitNow.Add(-d) }

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// rawLimit is an error-path record with its quota fields replaced (reset forms the fake doesn't write).
func rawLimit(t *testing.T, at time.Time, edit func(r map[string]any)) string {
	t.Helper()
	var r map[string]any
	if err := json.Unmarshal([]byte(rt.LimitRecord(proto.LimitFiveHour, at.Add(time.Hour), at)), &r); err != nil {
		t.Fatal(err)
	}
	edit(r)
	b, _ := json.Marshal(r)
	return string(b)
}

// ADR-0079 decision 1: the typed signals that set and clear a run's usage limit, from the contract's record shapes
// with placeholder values.
func TestLimitParser(t *testing.T) {
	h := time.Hour
	at := ago(2 * h)
	reset := at.Add(3 * h)
	ms := func(r map[string]any) {
		r["quotaLimits"].(map[string]any)["resetsAt"] = reset.UnixMilli()
		r["apiErrorParams"].(map[string]any)["rate_limit_info"].(map[string]any)["resetsAt"] = reset.UnixMilli()
	}
	type want struct {
		kind    string // "" with cleared false: no limit ever
		reset   *time.Time
		since   time.Time
		cleared bool
	}
	open := func(kind string, reset *time.Time, since time.Time) want {
		return want{kind: kind, reset: reset, since: since}
	}
	for _, c := range []struct {
		name  string
		lines []string
		want  want
	}{
		{"no limit", []string{rt.TypedRecord(at), rt.ToolResultRecord(at), rt.AssistantRecord("m1", at)}, want{}},
		{"five hour", []string{rt.TypedRecord(at), rt.LimitRecord("five_hour", reset, at), rt.NoticeRecord(at)}, open("five_hour", &reset, at)},
		{"seven day", []string{rt.LimitRecord("seven_day", reset, at)}, open("seven_day", &reset, at)},
		{"seven day opus", []string{rt.LimitRecord("seven_day_opus", reset, at)}, open("seven_day_opus", &reset, at)},
		{"seven day sonnet", []string{rt.LimitRecord("seven_day_sonnet", reset, at)}, open("seven_day_sonnet", &reset, at)},
		{"overage included", []string{rt.LimitRecord("seven_day_overage_included", reset, at)}, open("seven_day_overage_included", &reset, at)},
		{"overage", []string{rt.LimitRecord("overage", reset, at)}, open("overage", &reset, at)},
		{"unlisted kind", []string{rt.LimitRecord("daily", reset, at)}, open(proto.LimitUnknown, &reset, at)},
		{"no reset", []string{rawLimit(t, at, func(r map[string]any) {
			delete(r["quotaLimits"].(map[string]any), "resetsAt")
			delete(r, "apiErrorParams")
		})}, open("five_hour", nil, at)},
		{"reset only in apiErrorParams", []string{rawLimit(t, at, func(r map[string]any) { delete(r, "quotaLimits") })}, open("five_hour", ptrTime(at.Add(h)), at)},
		// Review: a millisecond epoch read as seconds would be in the year 50000 and fail json.Marshal.
		{"millisecond reset", []string{rawLimit(t, at, ms)}, open("five_hour", nil, at)},
		{"negative reset", []string{rawLimit(t, at, func(r map[string]any) { r["quotaLimits"].(map[string]any)["resetsAt"] = -5 })}, open("five_hour", nil, at)},
		{"reset too far", []string{rt.LimitRecord("seven_day", at.Add(9*24*h), at)}, open("seven_day", nil, at)},
		{"string reset", []string{strings.Replace(rt.LimitRecord("five_hour", reset, at), `"resetsAt":`, `"resetsAt":"x","r":`, 2)}, want{}},
		{"no timestamp", []string{strings.Replace(rt.LimitRecord("five_hour", reset, at), `"timestamp"`, `"ts"`, 1)}, open("five_hour", &reset, limitNow)},
		{"wrap-up path", []string{rt.TypedRecord(at), rt.WrapUpRecord(at), rt.AssistantRecord("grace", at.Add(time.Minute)), rt.NoticeRecord(at)},
			open(proto.LimitUnknown, nil, at)},
		{"wrap-up keeps a known kind", []string{rt.LimitRecord("five_hour", reset, at), rt.WrapUpRecord(at.Add(h))}, open("five_hour", &reset, at)},
		{"wrap-up then auto-continue", []string{rt.WrapUpRecord(at), rt.AssistantRecord("grace", at), rt.NoticeRecord(at), rt.NoticeRecord(at.Add(h)),
			rt.AutoContinueRecord(at.Add(h))}, want{since: at, cleared: true}},
		{"wrap-up then task notification reply", []string{rt.WrapUpRecord(at), rt.AssistantRecord("grace", at), rt.NoticeRecord(at),
			strings.Replace(rt.TypedRecord(at.Add(h)), `"human"`, `"task-notification"`, 1), rt.AssistantRecord("m2", at.Add(h))}, want{since: at, cleared: true}},
		{"error then auto-continue", []string{rt.LimitRecord("five_hour", reset, at), rt.NoticeRecord(at), rt.NoticeRecord(reset), rt.AutoContinueRecord(reset)},
			want{since: at, cleared: true}},
		{"error then a later reply", []string{rt.LimitRecord("seven_day", reset, at), rt.TypedRecord(at.Add(h)), rt.AssistantRecord("m2", at.Add(h))},
			want{since: at, cleared: true}},
		{"a reply with no usage does not clear", []string{rt.LimitRecord("five_hour", reset, at), rt.TypedRecord(at),
			`{"type":"assistant","message":{"id":"m2","usage":{"input_tokens":0,"output_tokens":0}}}`}, open("five_hour", &reset, at)},
		{"a typed turn alone does not clear", []string{rt.LimitRecord("five_hour", reset, at), rt.TypedRecord(at.Add(h))}, open("five_hour", &reset, at)},
		// A nudge while armed: release note, then the refused request. The limit stays open since the first record;
		// kind and reset follow the newest.
		{"nudge refused", []string{rt.WrapUpRecord(at), rt.NoticeRecord(at), rt.TypedRecord(at.Add(h)), rt.ReleaseRecord(at.Add(h)), rt.NoticeRecord(at.Add(h)),
			rt.LimitRecord("five_hour", reset, at.Add(h))}, open("five_hour", &reset, at)},
		{"limit again after a clear", []string{rt.LimitRecord("five_hour", at.Add(-h), at.Add(-3*h)), rt.AutoContinueRecord(at.Add(-h)), rt.AssistantRecord("m", at.Add(-h)),
			rt.LimitRecord("seven_day", reset, at)}, open("seven_day", &reset, at)},
		{"gateway quota", []string{rt.GatewayRecord("no eligible account is available for model claude-opus-5-5", at)}, open(proto.LimitGatewayQuota, nil, at)},
		{"gateway budget", []string{rt.GatewayRecord("budget team spend is exhausted for this day (1 of 1 USD)", at)}, open(proto.LimitGatewayBudget, nil, at)},
		{"gateway cleared", []string{rt.GatewayRecord("no eligible account is available for model m", at), rt.TypedRecord(at.Add(h)), rt.AssistantRecord("m", at.Add(h))},
			want{since: at, cleared: true}},
		{"other 429", []string{rt.GatewayRecord("overloaded "+rt.TranscriptMarker, at)}, want{}},
		{"budget not anchored", []string{rt.GatewayRecord("the budget x is exhausted now", at)}, want{}},
		{"403 no eligible account", []string{strings.Replace(rt.GatewayRecord("no eligible account is available for model m", at), "429", "403", 1)}, want{}},
		{"no Claude Code prefix", []string{strings.Replace(rt.GatewayRecord("no eligible account is available for model m", at), "API Error: Request rejected (429) · ", "", 1)}, want{}},
		{"garbage", []string{"not json", "[1,2]", `"x"`, "", "{", rt.LimitRecord("five_hour", reset, at), "null"}, open("five_hour", &reset, at)},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := scanLimit(writeTranscript(t, c.lines...), runLimit{}, false, limitNow)
			if err != nil {
				t.Fatal(err)
			}
			got := p.lim.normalized(limitNow)
			w := got.wire(limitNow)
			if c.want == (want{}) {
				if w != nil {
					t.Fatalf("limit %+v", *w)
				}
				return
			}
			if w == nil {
				t.Fatalf("no limit (%+v)", got)
			}
			if w.Kind != c.want.kind || w.Cleared != c.want.cleared || !w.Since.Equal(c.want.since) {
				t.Errorf("got %+v, want %+v", *w, c.want)
			}
			if (w.ResetsAt == nil) != (c.want.reset == nil) || (w.ResetsAt != nil && !w.ResetsAt.Equal(*c.want.reset)) {
				t.Errorf("reset %v, want %v", w.ResetsAt, c.want.reset)
			}
			if _, err := json.Marshal(proto.RunReport{RunID: "run_1", Limit: w}); err != nil {
				t.Errorf("marshal: %v", err)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// The scan reads only what was appended since its offset, never a partial last line, starts over when the file is
// shorter (truncated) or replaced, and skips an over-long record without holding it.
func TestLimitScanOffsets(t *testing.T) {
	at := ago(time.Hour)
	reset := at.Add(2 * time.Hour)
	first := rt.TypedRecord(at) + "\n" + rt.LimitRecord("five_hour", reset, at) + "\n"
	path := writeTranscript(t, strings.TrimSuffix(first, "\n"))
	p, err := scanLimit(path, runLimit{}, false, limitNow)
	if err != nil || p.lim.Kind != "five_hour" || p.lim.Offset != int64(len(first)) || p.records != 2 {
		t.Fatalf("first scan %+v %v", p, err)
	}
	// A partial line is left for the next scan.
	partial := rt.AutoContinueRecord(reset)
	appendRaw(t, path, partial[:20])
	q, err := scanLimit(path, p.lim, false, limitNow)
	if err != nil || q.lim.Offset != p.lim.Offset || q.records != 0 || q.lim.Kind != "five_hour" {
		t.Fatalf("partial %+v %v", q, err)
	}
	appendRaw(t, path, partial[20:]+"\n")
	q, err = scanLimit(path, q.lim, false, limitNow)
	if err != nil || q.lim.Kind != "" || q.lim.Since == nil || q.records != 1 {
		t.Fatalf("completed line %+v %v", q, err)
	}
	// Nothing new: nothing read.
	if r, _ := scanLimit(path, q.lim, false, limitNow); r.records != 0 || r.lim.Offset != q.lim.Offset {
		t.Fatalf("rescan read %d records", r.records)
	}
	// Truncated below the offset: read from the start again.
	if err := os.WriteFile(path, []byte(rt.LimitRecord("seven_day", reset, at)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := scanLimit(path, q.lim, false, limitNow)
	if r.lim.Kind != "seven_day" || r.records != 1 {
		t.Fatalf("after truncation %+v", r)
	}
	// Replaced by a longer file: the caller says so.
	if r2, _ := scanLimit(path, runLimit{Offset: 3}, true, limitNow); r2.records != 1 || r2.lim.Kind != "seven_day" {
		t.Fatalf("replaced %+v", r2)
	}
	// An over-long record is skipped, and the record after it is read.
	long := `{"type":"user","message":{"content":"` + strings.Repeat("x", limitLineMax) + `"}}`
	path2 := writeTranscript(t, long, rt.LimitRecord("five_hour", reset, at))
	s, err := scanLimit(path2, runLimit{}, false, limitNow)
	if err != nil || s.skipped != 1 || s.records != 1 || s.lim.Kind != "five_hour" || s.lim.Offset != int64(len(long)+1+len(rt.LimitRecord("five_hour", reset, at))+1) {
		t.Fatalf("long line %+v %v", s, err)
	}
	if _, err := scanLimit(filepath.Join(t.TempDir(), "missing"), runLimit{}, false, limitNow); err == nil {
		t.Fatal("missing transcript scanned")
	}
}

// A scan that ends inside the wrap-up turn resumes at the note, so the turn's grace replies, read by the next scan,
// don't clear the limit.
func TestLimitScanWrapUpAcrossScans(t *testing.T) {
	at := ago(time.Hour)
	path := writeTranscript(t, rt.TypedRecord(at), rt.AssistantRecord("m0", at))
	before, _ := os.Stat(path)
	appendRaw(t, path, rt.WrapUpRecord(at)+"\n")
	p, _ := scanLimit(path, runLimit{}, false, limitNow)
	if p.lim.Kind != proto.LimitUnknown || p.lim.Offset != before.Size() {
		t.Fatalf("wrap-up scan %+v (offset %d, want %d)", p.lim, p.lim.Offset, before.Size())
	}
	appendRaw(t, path, rt.AssistantRecord("grace", at)+"\n"+rt.NoticeRecord(at)+"\n")
	q, _ := scanLimit(path, p.lim, false, limitNow)
	if q.lim.Kind != proto.LimitUnknown || q.lim.Offset != before.Size() {
		t.Fatalf("grace reply cleared the limit: %+v", q.lim)
	}
	appendRaw(t, path, rt.AutoContinueRecord(at.Add(time.Hour))+"\n")
	r, _ := scanLimit(path, q.lim, false, limitNow)
	fi, _ := os.Stat(path)
	if r.lim.Kind != "" || r.lim.Since == nil || !r.lim.Since.Equal(at) || r.lim.Offset != fi.Size() {
		t.Fatalf("auto-continue %+v", r.lim)
	}
}

func appendRaw(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// FuzzLimitScan: any transcript scans without panicking. Its limit is on the allowlist and always marshals, and
// scanning it in two parts (split at any line) gives the same limit as one scan.
func FuzzLimitScan(f *testing.F) {
	at := ago(time.Hour)
	for _, seed := range [][]string{
		{rt.TypedRecord(at), rt.LimitRecord("five_hour", at.Add(time.Hour), at), rt.NoticeRecord(at)},
		{rt.WrapUpRecord(at), rt.AssistantRecord("g", at), rt.NoticeRecord(at), rt.AutoContinueRecord(at), rt.AssistantRecord("m", at)},
		{rt.GatewayRecord("budget b is exhausted for this day", at), rt.TypedRecord(at), rt.AssistantRecord("m", at)},
		{`{"type":"assistant","apiError":"usage_limit_reached","quotaLimits":{"resetsAt":1e300,"rateLimitType":"x"},"timestamp":"0001-01-01T00:00:00Z"}`},
		{`{"type":"assistant","apiError":"usage_limit_reached","quotaLimits":{"resetsAt":99999999999},"timestamp":"9999-12-31T23:59:59Z"}`},
	} {
		f.Add([]byte(strings.Join(seed, "\n")+"\n"), uint(1))
	}
	f.Fuzz(func(t *testing.T, data []byte, cut uint) {
		dir := t.TempDir()
		whole := filepath.Join(dir, "whole.jsonl")
		if err := os.WriteFile(whole, data, 0o600); err != nil {
			t.Fatal(err)
		}
		p, err := scanLimit(whole, runLimit{}, false, limitNow)
		if err != nil {
			t.Fatal(err)
		}
		w := p.lim.wire(limitNow)
		if w != nil {
			if !w.Cleared && proto.LimitKind(w.Kind) != w.Kind {
				t.Fatalf("kind %q", w.Kind)
			}
			if _, err := json.Marshal(proto.RunReport{RunID: "r", Limit: w}); err != nil {
				t.Fatalf("marshal %+v: %v", *w, err)
			}
		}
		// Two scans: the first sees the lines up to a cut, the second the rest appended.
		lines := bytes.SplitAfter(data, []byte("\n"))
		k := int(cut % uint(len(lines)+1))
		head := bytes.Join(lines[:k], nil)
		split := filepath.Join(dir, "split.jsonl")
		if err := os.WriteFile(split, head, 0o600); err != nil {
			t.Fatal(err)
		}
		q, err := scanLimit(split, runLimit{}, false, limitNow)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(split, data, 0o600); err != nil {
			t.Fatal(err)
		}
		r, err := scanLimit(split, q.lim, false, limitNow)
		if err != nil {
			t.Fatal(err)
		}
		if !r.lim.same(p.lim) || r.lim.Offset != p.lim.Offset {
			t.Fatalf("split at line %d: %+v, whole: %+v", k, r.lim, p.lim)
		}
	})
}
