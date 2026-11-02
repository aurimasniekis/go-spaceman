package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func spanCtx(t *testing.T) (context.Context, string, string) {
	t.Helper()
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	return trace.ContextWithSpanContext(context.Background(), sc), tid.String(), sid.String()
}

func TestWithIDs(t *testing.T) {
	ctx := context.Background()
	if IDs(ctx) != nil || ID(ctx, FieldRunID) != "" || IDs(nil) != nil { //nolint:staticcheck // nil ctx tolerated
		t.Fatal("empty context must carry no IDs")
	}
	if WithIDs(ctx, RunID("")) != ctx {
		t.Error("empty values must not derive a context")
	}
	ctx = WithIDs(ctx, RunID("run_1"), ConnectionID("con_1"))
	ctx2 := WithIDs(ctx, RunID("run_2"), NodeID("nod_1"))
	if ID(ctx, FieldRunID) != "run_1" || ID(ctx2, FieldRunID) != "run_2" || ID(ctx2, FieldConnectionID) != "con_1" {
		t.Errorf("ids: %v / %v", IDs(ctx), IDs(ctx2))
	}
	if WithIDs(ctx2, RunID("run_2")) != ctx2 {
		t.Error("unchanged IDs must not derive a context")
	}
	long := strings.Repeat("x", 500)
	if got := ID(WithIDs(ctx, TaskID(long)), FieldTaskID); len(got) != maxIDLen {
		t.Errorf("value not truncated: %d", len(got))
	}
	all := []CorrelationID{HTTPRequestID("a"), UserID("b"), TokenID("c"), SpaceID("d"), NodeID("e"), RunID("f"),
		TaskID("g"), CommandID("h"), SubscriptionID("i"), ConnectionID("j")}
	c := WithIDs(context.Background(), all...)
	for _, id := range all {
		if ID(c, id.Key()) != id.Value() {
			t.Errorf("%s lost", id.Key())
		}
	}
}

func TestTraceIDs(t *testing.T) {
	if a, b := TraceIDs(context.Background()); a != "" || b != "" {
		t.Error("no span expected")
	}
	if a, b := TraceIDs(nil); a != "" || b != "" { //nolint:staticcheck // nil ctx tolerated
		t.Error("nil ctx")
	}
	ctx, tid, sid := spanCtx(t)
	if a, b := TraceIDs(ctx); a != tid || b != sid {
		t.Errorf("got %s %s", a, b)
	}
}

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %s", l)
		}
		out = append(out, m)
	}
	return out
}

func TestLoggerCarriesCorrelation(t *testing.T) {
	var buf bytes.Buffer
	l, _ := NewLogger(&buf, LogConfig{Level: "info", Format: "json"})
	ctx, tid, sid := spanCtx(t)
	ctx = WithIDs(ctx, HTTPRequestID("req-1"), RunID("run_ctx"), TaskID("tsk_1"), SpaceID("spc_1"), ConnectionID("con_1"), NodeID("nod_1"))

	l.InfoContext(ctx, "with context")
	l.InfoContext(ctx, "explicit wins", slog.String(FieldRunID, "run_explicit"))
	l.With(slog.String(FieldTaskID, "tsk_with")).InfoContext(ctx, "logger attrs win")
	l.Info("no context")
	l.WithGroup("g").With(slog.String(FieldSpaceID, "inner")).With().InfoContext(ctx, "grouped", "k", "v", "password", planted)
	l.WithGroup("").InfoContext(context.Background(), "empty group")

	lines := decodeLines(t, &buf)
	first := lines[0]
	for k, v := range map[string]string{FieldTraceID: tid, FieldSpanID: sid, FieldHTTPRequestID: "req-1", FieldRunID: "run_ctx",
		FieldTaskID: "tsk_1", FieldSpaceID: "spc_1", FieldConnectionID: "con_1", FieldNodeID: "nod_1"} {
		if first[k] != v {
			t.Errorf("%s = %v, want %s", k, first[k], v)
		}
	}
	if lines[1][FieldRunID] != "run_explicit" || strings.Count(buf.String(), "run_explicit") != 1 {
		t.Errorf("explicit attr must win without duplication: %v", lines[1])
	}
	if lines[2][FieldTaskID] != "tsk_with" || strings.Contains(strings.Split(buf.String(), "\n")[2], "tsk_1") {
		t.Errorf("logger attr must win without duplication: %v", lines[2])
	}
	if _, has := lines[3][FieldTraceID]; has {
		t.Error("record without context must not carry trace.id")
	}
	if g, _ := lines[4]["g"].(map[string]any); g == nil || g[FieldSpaceID] != "inner" || g[FieldTraceID] != nil || g["password"] != "[REDACTED]" ||
		g["k"] != "v" || lines[4][FieldTraceID] != tid || lines[4][FieldSpaceID] != "spc_1" {
		t.Errorf("grouped attrs: %v", lines[4])
	}
	if strings.Contains(buf.String(), planted) {
		t.Error("secret leaked through grouped context handler")
	}
}

func TestContextHandlerEnabled(t *testing.T) {
	var buf bytes.Buffer
	l, lv := NewLogger(&buf, LogConfig{Level: "warn", Format: "text"})
	ctx, _, _ := spanCtx(t)
	l.InfoContext(ctx, "dropped")
	lv.Set(slog.LevelInfo)
	l.InfoContext(ctx, "kept")
	if strings.Contains(buf.String(), "dropped") || !strings.Contains(buf.String(), "trace.id=") {
		t.Errorf("text output: %s", buf.String())
	}
}
