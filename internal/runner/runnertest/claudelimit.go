package runnertest

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Claude Code transcript records for usage-limit tests (ADR-0079). The shapes are the ones recorded in
// docs/contracts/harnesses.md ("Claude Code usage limits", 2.1.317–2.1.323) with placeholder values; every text field
// carries TranscriptMarker, so a test can check that nothing from the transcript reached a log or a message.

// TranscriptMarker is in every text the fake records carry.
const TranscriptMarker = "placeholder-transcript-text"

func record(v map[string]any, at time.Time) string {
	v["timestamp"] = at.UTC().Format(time.RFC3339Nano)
	v["sessionId"] = "00000000-0000-4000-8000-000000000000"
	v["version"] = "2.1.323"
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func text(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }

// LimitRecord is the error-path record: an assistant API-error message with apiError usage_limit_reached, the kind
// (rateLimitType) and the reset in epoch seconds in both quotaLimits and apiErrorParams.
func LimitRecord(kind string, resetsAt, at time.Time) string {
	q := map[string]any{"status": "rejected", "resetsAt": resetsAt.Unix(), "rateLimitType": kind, "overageStatus": "rejected",
		"overageDisabledReason": "placeholder", "isUsingOverage": false}
	quota := map[string]any{"unifiedRateLimitFallbackAvailable": false, "upgradePaths": []string{"placeholder"}}
	for k, v := range q {
		quota[k] = v
	}
	return record(map[string]any{"type": "assistant", "isApiErrorMessage": true, "error": "rate_limit", "apiErrorStatus": 429,
		"apiError": "usage_limit_reached", "apiErrorParams": map[string]any{"rate_limit_info": q}, "quotaLimits": quota, "requestId": "req_placeholder",
		"message": map[string]any{"model": "<synthetic>", "role": "assistant", "content": text("You've hit your limit · " + TranscriptMarker),
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}}}, at)
}

// WrapUpRecord is the wrap-up path's meta user record: no kind and no reset time anywhere.
func WrapUpRecord(at time.Time) string {
	return record(map[string]any{"type": "user", "isMeta": true, "turnCompanion": true, "usageLimitNote": "wrap_up",
		"message": map[string]any{"role": "user", "content": "[Usage limit reached; " + TranscriptMarker + "]"}}, at)
}

// ReleaseRecord is the meta note Claude Code adds when typed input arrives while auto-continue is armed.
func ReleaseRecord(at time.Time) string {
	return record(map[string]any{"type": "user", "isMeta": true, "usageLimitNote": "release",
		"message": map[string]any{"role": "user", "content": "[" + TranscriptMarker + "]"}}, at)
}

// NoticeRecord is an auto-continue informational record (armed, re-armed or reset: they differ only in wording).
func NoticeRecord(at time.Time) string {
	return record(map[string]any{"type": "system", "subtype": "informational", "level": "notice", "isMeta": false,
		"content": "Usage limit · " + TranscriptMarker}, at)
}

// AutoContinueRecord is the meta user record of the turn auto-continue starts at the reset.
func AutoContinueRecord(at time.Time) string {
	return record(map[string]any{"type": "user", "isMeta": true, "origin": map[string]any{"kind": "auto-continuation"},
		"turnOrigin": "auto_continuation", "promptSource": "system", "message": map[string]any{"role": "user", "content": TranscriptMarker}}, at)
}

// TypedRecord is a typed user prompt (a SpaceMan notice or an operator's input).
func TypedRecord(at time.Time) string {
	return record(map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "promptSource": "typed",
		"message": map[string]any{"role": "user", "content": TranscriptMarker}}, at)
}

// ToolResultRecord is the user record that carries a tool's result (not a turn start).
func ToolResultRecord(at time.Time) string {
	return record(map[string]any{"type": "user", "toolUseResult": map[string]any{"stdout": TranscriptMarker},
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_placeholder",
			"content": TranscriptMarker}}}}, at)
}

// AssistantRecord is a model reply with non-zero usage.
func AssistantRecord(id string, at time.Time) string {
	return record(map[string]any{"type": "assistant", "message": map[string]any{"id": id, "model": "claude-opus-5-5", "role": "assistant",
		"content": text(TranscriptMarker), "usage": map[string]any{"input_tokens": 3, "output_tokens": 5, "cache_creation_input_tokens": 0,
			"cache_read_input_tokens": 100}}}, at)
}

// GatewayRecord is Claude Code's record of a gateway's 429 refusal; message is the gateway's error message.
func GatewayRecord(message string, at time.Time) string {
	return record(map[string]any{"type": "assistant", "isApiErrorMessage": true, "error": "rate_limit", "apiErrorStatus": 429,
		"apiErrorIsTransient": false, "failedResponseHadNoToolCall": true,
		"message": map[string]any{"model": "<synthetic>", "role": "assistant", "content": text("API Error: Request rejected (429) · " + message)}}, at)
}

// AppendTranscript appends records (one per line) to a transcript, creating it and its folder's file if needed.
func AppendTranscript(path string, records ...string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // a test's own transcript path
	if err != nil {
		return err
	}
	for _, r := range records {
		if _, err := fmt.Fprintln(f, r); err != nil {
			_ = f.Close()
			return err
		}
	}
	return f.Close()
}

// HitLimit plays a Claude Code turn that runs into a subscription usage limit: a typed prompt, the error-path limit
// record and the auto-continue arm record are appended to the run's transcript, then the program goes idle.
func (f *Fake) HitLimit(id, transcript, kind string, resetsAt, at time.Time) error {
	f.SetStatus(id, "working")
	if err := AppendTranscript(transcript, TypedRecord(at), LimitRecord(kind, resetsAt, at), NoticeRecord(at)); err != nil {
		return err
	}
	f.SetStatus(id, "idle")
	return nil
}

// LimitReset plays Claude Code's auto-continue at the reset: the reset notice, the auto-continuation turn and its
// reply are appended, then the program goes idle again.
func (f *Fake) LimitReset(id, transcript string, at time.Time) error {
	f.SetStatus(id, "working")
	if err := AppendTranscript(transcript, NoticeRecord(at), AutoContinueRecord(at), AssistantRecord("msg_continue", at)); err != nil {
		return err
	}
	f.SetStatus(id, "idle")
	return nil
}
