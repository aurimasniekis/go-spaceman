package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Spike S-7 shape: assistant records carry message.id, model and usage; a request's records repeat while streaming;
// a cost-state record carries Claude Code's estimate.
const claudeTranscript = `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"id":"msg_1","model":"claude-haiku-5-5","usage":{"input_tokens":2,"output_tokens":1,"cache_creation_input_tokens":100,"cache_read_input_tokens":50}}}
{"type":"assistant","message":{"id":"msg_1","model":"claude-haiku-5-5","usage":{"input_tokens":2,"output_tokens":4,"cache_creation_input_tokens":100,"cache_read_input_tokens":50}}}
{"type":"assistant","message":{"id":"msg_2","model":"claude-sonnet-5-5","usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":7}}}
not json
{"type":"cost-state","totalCostUSD":0.0042,"hasUnknownModelCost":false}
`

func TestClaudeUsage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(claudeTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
	models, est, err := claudeUsage(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models: %+v", models)
	}
	h := models[0]
	if h.Model != "claude-haiku-5-5" || *h.Input != 2 || *h.Output != 4 || *h.CacheWrite != 100 || *h.CacheRead != 50 || h.Requests != 1 {
		t.Fatalf("haiku (streamed records must count once, last wins): %+v in=%d out=%d", h, *h.Input, *h.Output)
	}
	if s := models[1]; *s.Output != 20 || s.Requests != 1 {
		t.Fatalf("sonnet: %+v", s)
	}
	if est == nil || est.USD != 0.0042 || est.Source == "" || est.Assumptions == "" {
		t.Fatalf("estimate: %+v", est)
	}
	if _, _, err := claudeUsage(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Fatal("a missing transcript was read")
	}
	// Codex keeps no verified usage: unknown, not zero.
	a := memAgent(t)
	rep := a.usageReport(runRow{RunID: "run_x", Harness: "codex"}, true)
	if rep.Source != proto.UsageUnavailable || len(rep.Models) != 0 || rep.Note == "" {
		t.Fatalf("codex: %+v", rep)
	}
	n, sum, trunc, err := hashPrefix(p, 10)
	if err != nil || n != 10 || !trunc || len(sum) != 64 {
		t.Fatalf("hashPrefix: %d %s %v %v", n, sum, trunc, err)
	}
}
