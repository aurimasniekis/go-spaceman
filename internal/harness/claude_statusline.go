package harness

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
)

// The data SpaceMan takes from Claude Code's own hook and status-line processes. Both run as children of an untrusted
// agent process and read JSON Claude Code builds from the session, so everything is parsed into typed fields and every
// field is checked: numbers must be finite and in range, strings must be short and match a fixed pattern. Free text
// (last_assistant_message, error_details, cwd, transcript_path, workspace, any other key) is never read, so it can't cross
// the bridge socket or reach a log. The node agent applies the same checks again when it receives a snapshot.

// MaxStatusInput bounds how much of the status line's (and the hook's) input is read.
const MaxStatusInput = 64 << 10

// The string fields are bounded by their patterns: a session ID at most 64 characters, a version 32, a model ID 64.
var (
	reSessionID     = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	reStatusVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,31}$`)
	reModelID       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]@-]{0,63}$`)
)

// StopFailureErrors are the `error` values of Claude Code's StopFailure hook (docs/plans, binary of 2.1.325). Any other
// value is sent as "unknown", so a new value never crosses as text.
var StopFailureErrors = []string{
	"authentication_failed", "oauth_org_not_allowed", "account_on_hold", "verification_required", "billing_error", "rate_limit",
	"overloaded", "invalid_request", "model_not_found", "server_error", "unknown", "max_output_tokens", "cloud_credential_error",
}

// StopFailure is what the StopFailure hook forwards: the error class and the session.
type StopFailure struct {
	Error     string `json:"error"`
	SessionID string `json:"session_id,omitempty"`
}

// Valid reports whether f carries only allowlisted values.
func (f StopFailure) Valid() bool {
	ok := false
	for _, e := range StopFailureErrors {
		if f.Error == e {
			ok = true
		}
	}
	return ok && (f.SessionID == "" || reSessionID.MatchString(f.SessionID))
}

// SanitizeStopFailure reads the hook's input and returns the allowlisted fields. ok is false when the input is not a
// JSON object; an unknown or missing error becomes "unknown".
func SanitizeStopFailure(raw []byte) (StopFailure, bool) {
	var in struct {
		Error     any `json:"error"`
		SessionID any `json:"session_id"`
	}
	if json.Unmarshal(limit(raw), &in) != nil {
		return StopFailure{}, false
	}
	out := StopFailure{Error: "unknown"}
	if s, ok := in.Error.(string); ok {
		for _, e := range StopFailureErrors {
			if s == e {
				out.Error = s
			}
		}
	}
	if s, ok := in.SessionID.(string); ok && reSessionID.MatchString(s) {
		out.SessionID = s
	}
	return out, true
}

// Window is one subscription rate-limit window of the status line.
type Window struct {
	UsedPct  float64 `json:"used_pct"`
	ResetsAt int64   `json:"resets_at,omitempty"` // Unix seconds; 0 when absent
}

// StatusSnapshot is the typed subset of a status-line input that SpaceMan keeps (plan §3.3). Absent numbers are nil.
type StatusSnapshot struct {
	SessionID string `json:"session_id,omitempty"`
	Version   string `json:"version,omitempty"`
	ModelID   string `json:"model_id,omitempty"`

	CostUSD       *float64 `json:"cost_usd,omitempty"` // Claude Code's own list-price estimate
	APIDurationMS *int64   `json:"api_duration_ms,omitempty"`

	ContextUsedPct *float64 `json:"context_used_pct,omitempty"`
	InputTokens    *int64   `json:"input_tokens,omitempty"`
	OutputTokens   *int64   `json:"output_tokens,omitempty"`
	CacheWrite     *int64   `json:"cache_write_tokens,omitempty"`
	CacheRead      *int64   `json:"cache_read_tokens,omitempty"`

	FiveHour *Window `json:"five_hour,omitempty"`
	SevenDay *Window `json:"seven_day,omitempty"`
}

const (
	maxTokens = int64(1) << 40
	maxCost   = 1e7
	maxMillis = int64(1) << 42
)

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok && !math.IsNaN(f) && !math.IsInf(f, 0)
}

func pct(v any) *float64 {
	if f, ok := num(v); ok && f >= 0 && f <= 1000 {
		return &f
	}
	return nil
}

func count(v any, max int64) *int64 {
	if f, ok := num(v); ok && f >= 0 && f <= float64(max) && f == math.Trunc(f) {
		n := int64(f)
		return &n
	}
	return nil
}

func str(v any, re *regexp.Regexp) string {
	if s, ok := v.(string); ok && re.MatchString(s) {
		return s
	}
	return ""
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func window(v any) *Window {
	m := obj(v)
	if m == nil {
		return nil
	}
	p := pct(m["used_percentage"])
	if p == nil {
		return nil
	}
	w := &Window{UsedPct: *p}
	if f, ok := num(m["resets_at"]); ok && f >= 1e9 && f <= 4e9 && f == math.Trunc(f) {
		w.ResetsAt = int64(f)
	}
	return w
}

// SanitizeStatusLine reads a status-line input and returns the allowlisted fields. ok is false when the input is not a
// JSON object. Fields of the wrong type, out of range or not matching their pattern are dropped one by one.
func SanitizeStatusLine(raw []byte) (StatusSnapshot, bool) {
	var in map[string]any
	if json.Unmarshal(limit(raw), &in) != nil || in == nil {
		return StatusSnapshot{}, false
	}
	var s StatusSnapshot
	s.SessionID = str(in["session_id"], reSessionID)
	s.Version = str(in["version"], reStatusVersion)
	s.ModelID = str(obj(in["model"])["id"], reModelID)
	cost := obj(in["cost"])
	if f, ok := num(cost["total_cost_usd"]); ok && f >= 0 && f <= maxCost {
		s.CostUSD = &f
	}
	s.APIDurationMS = count(cost["total_api_duration_ms"], maxMillis)
	cw := obj(in["context_window"])
	s.ContextUsedPct = pct(cw["used_percentage"])
	s.InputTokens = count(cw["total_input_tokens"], maxTokens)
	s.OutputTokens = count(cw["total_output_tokens"], maxTokens)
	cur := obj(cw["current_usage"])
	s.CacheWrite = count(cur["cache_creation_input_tokens"], maxTokens)
	s.CacheRead = count(cur["cache_read_input_tokens"], maxTokens)
	rl := obj(in["rate_limits"])
	s.FiveHour = window(rl["five_hour"])
	s.SevenDay = window(rl["seven_day"])
	return s, true
}

// Valid reports whether s satisfies the same checks SanitizeStatusLine applies; the node agent uses it on what arrives
// over the socket, which an untrusted process could write directly.
func (s StatusSnapshot) Valid() bool {
	b, err := json.Marshal(s)
	if err != nil {
		return false
	}
	// A round trip through the sanitizer's own rules: build the status-line shape from s and compare.
	in := map[string]any{"session_id": s.SessionID, "version": s.Version, "model": map[string]any{"id": s.ModelID}}
	cost, cw, cur, rl := map[string]any{}, map[string]any{}, map[string]any{}, map[string]any{}
	setF := func(m map[string]any, k string, v *float64) {
		if v != nil {
			m[k] = *v
		}
	}
	setI := func(m map[string]any, k string, v *int64) {
		if v != nil {
			m[k] = float64(*v)
		}
	}
	setF(cost, "total_cost_usd", s.CostUSD)
	setI(cost, "total_api_duration_ms", s.APIDurationMS)
	setF(cw, "used_percentage", s.ContextUsedPct)
	setI(cw, "total_input_tokens", s.InputTokens)
	setI(cw, "total_output_tokens", s.OutputTokens)
	setI(cur, "cache_creation_input_tokens", s.CacheWrite)
	setI(cur, "cache_read_input_tokens", s.CacheRead)
	for k, w := range map[string]*Window{"five_hour": s.FiveHour, "seven_day": s.SevenDay} {
		if w != nil {
			rl[k] = map[string]any{"used_percentage": w.UsedPct, "resets_at": float64(w.ResetsAt)}
		}
	}
	cw["current_usage"] = cur
	in["cost"], in["context_window"], in["rate_limits"] = cost, cw, rl
	rb, _ := json.Marshal(in)
	again, ok := SanitizeStatusLine(rb)
	if !ok {
		return false
	}
	ab, _ := json.Marshal(again)
	return bytes.Equal(b, ab)
}

func limit(raw []byte) []byte {
	if len(raw) > MaxStatusInput {
		return raw[:MaxStatusInput]
	}
	return raw
}
