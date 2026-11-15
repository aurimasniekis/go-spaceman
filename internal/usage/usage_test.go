package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

const (
	runA = "run_01aaaaaaaaaaaaaaaaaaaaaaaa"
	runB = "run_01bbbbbbbbbbbbbbbbbbbbbbbb"
	runC = "run_01cccccccccccccccccccccccc"
)

func newSvc(t *testing.T) (*Service, context.Context) {
	t.Helper()
	s := storetest.Open(t, storetest.WithClock(clock.NewFake(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))))
	kr := keyringtest.Load(t, s)
	ctx := context.Background()
	if err := s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		qs := []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_2', 'n2', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		}
		for _, id := range []string{runA, runB, runC} {
			h := "claude-code"
			if id == runC {
				h = "codex"
			}
			qs = append(qs, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, manifest_json, created_at, created_by, updated_at)
				VALUES ('`+id+`', 'spc_a', 'nod_1', 1, 'worker', 'implementor', '`+h+`', 'ins_1', 'running', 'running', '{}', '{}', 0, 't', 0)`)
		}
		for _, q := range qs {
			if _, err := tx.SQL.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return New(s, audit.NewWriter(kr), changefeed.New(nil), t.TempDir(), nil, nil), ctx
}

func i64(v int64) *int64 { return &v }

// SM-REC-005: usage is what the harness recorded; unknown stays unknown; a final report is not overwritten.
func TestUsageReports(t *testing.T) {
	svc, ctx := newSvc(t)
	svc.Report(ctx, "nod_2", proto.UsageReport{RunID: runA, Source: proto.UsageClaudeTranscript}) // not its node
	svc.Report(ctx, "nod_1", proto.UsageReport{RunID: runA, Source: "made-up"})
	if u, _ := svc.ForRun(ctx, "spc_a", runA); u.Known || u.Source != proto.UsageUnavailable {
		t.Fatalf("refused reports were stored: %+v", u)
	}
	svc.Report(ctx, "nod_1", proto.UsageReport{RunID: runA, Source: proto.UsageClaudeTranscript, Final: true,
		Models:   []proto.ModelUsage{{Model: "claude-haiku-5-5", Input: i64(2), Output: i64(4), CacheWrite: i64(100), CacheRead: i64(50), Requests: 1}},
		Estimate: &proto.CostEstimate{USD: 0.01, Source: "Claude Code", Assumptions: "list prices"}})
	svc.Report(ctx, "nod_1", proto.UsageReport{RunID: runA, Source: proto.UsageClaudeTranscript, Models: []proto.ModelUsage{}}) // late, ignored
	svc.Report(ctx, "nod_1", proto.UsageReport{RunID: runB, Source: proto.UsageClaudeTranscript, Models: []proto.ModelUsage{{Model: "m", Input: i64(10), Output: i64(1), CacheWrite: i64(0), CacheRead: i64(0), Requests: 2}}})
	svc.Report(ctx, "nod_1", proto.UsageReport{RunID: runC, Source: proto.UsageUnavailable, Final: true, Note: "not verified"})
	u, err := svc.ForRun(ctx, "spc_a", runA)
	if err != nil || !u.Known || !u.Final || len(u.Models) != 1 || *u.Models[0].Output != 4 || u.Estimate == nil || u.Estimate.Assumptions == "" {
		t.Fatalf("run A: %+v %v", u, err)
	}
	if _, err := svc.ForRun(ctx, "spc_b", runA); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatal("another space read the run")
	}
	s, err := svc.Space(ctx, "spc_a")
	if err != nil {
		t.Fatal(err)
	}
	if s.KnownRuns != 2 || s.UnknownRuns != 1 || s.Input != 12 || s.Output != 5 || s.Estimated != 1 || s.EstimatedUSD != 0.01 {
		t.Fatalf("summary: %+v", s)
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestTranscriptChunks(t *testing.T) {
	svc, ctx := newSvc(t)
	data := []byte(`{"a":1}` + "\n" + `{"b":2}` + "\n")
	size := int64(len(data))
	if ack := svc.Chunk(ctx, "nod_2", proto.TranscriptChunk{RunID: runA, Data: data, Size: size}); ack.Error == "" {
		t.Fatal("another node uploaded")
	}
	if ack := svc.Chunk(ctx, "nod_1", proto.TranscriptChunk{RunID: runA, Data: data[:5], Size: size}); ack.Next != 5 || ack.Done {
		t.Fatalf("first chunk: %+v", ack)
	}
	// A resend at the wrong offset learns where to continue.
	if ack := svc.Chunk(ctx, "nod_1", proto.TranscriptChunk{RunID: runA, Offset: 0, Data: data[:5], Size: size}); ack.Next != 5 {
		t.Fatalf("resend: %+v", ack)
	}
	ack := svc.Chunk(ctx, "nod_1", proto.TranscriptChunk{RunID: runA, Offset: 5, Data: data[5:], Size: size, Final: true, SHA256: sum(data)})
	if !ack.Done || ack.Error != "" || ack.Next != size {
		t.Fatalf("final: %+v", ack)
	}
	f, tr, err := svc.OpenTranscript(ctx, "spc_a", runA)
	if err != nil || tr.State != "stored" {
		t.Fatalf("open: %v %+v", err, tr)
	}
	got, _ := io.ReadAll(f)
	_ = f.Close()
	if string(got) != string(data) {
		t.Fatalf("stored: %q", got)
	}
	// After it is stored, further chunks change nothing.
	if ack := svc.Chunk(ctx, "nod_1", proto.TranscriptChunk{RunID: runA, Data: []byte("x"), Size: 1}); !ack.Done {
		t.Fatalf("after store: %+v", ack)
	}
	// A corrupted upload fails and is not served.
	if ack := svc.Chunk(ctx, "nod_1", proto.TranscriptChunk{RunID: runB, Data: data, Size: size, Final: true, SHA256: sum([]byte("other"))}); ack.Error == "" {
		t.Fatalf("bad checksum: %+v", ack)
	}
	if _, _, err := svc.OpenTranscript(ctx, "spc_a", runB); err == nil {
		t.Fatal("a failed transcript was served")
	}
	if ack := svc.Chunk(ctx, "nod_1", proto.TranscriptChunk{RunID: runC, Size: proto.TranscriptMax + 1}); ack.Error == "" {
		t.Fatal("an oversized transcript was accepted")
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(svc.DeleteTranscript(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_a"}, "spc_a", runA))
	if _, _, err := svc.OpenTranscript(ctx, "spc_a", runA); err == nil {
		t.Fatal("a deleted transcript was served")
	}
	if err := svc.DeleteTranscript(ctx, audit.Actor{}, "spc_a", runA); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("delete twice: %v", err)
	}
	if u, _ := svc.ForRun(ctx, "spc_a", runA); u.Transcript == nil || u.Transcript.State != "deleted" {
		t.Fatalf("transcript state: %+v", u.Transcript)
	}
}

// SM-GW-008: a gateway run's read-back is returned with its usage as gateway-observed figures; a node can't claim the
// harbor-pilot source, and an "unavailable" report never replaces usage Harbor Pilot filled in, while the harness's own
// figures do.
func TestGatewayUsage(t *testing.T) {
	svc, ctx := newSvc(t)
	if u, _ := svc.ForRun(ctx, "spc_a", runC); u.Gateway != nil {
		t.Fatal("gateway usage on a run without a read-back")
	}
	svc.Report(ctx, "nod_1", proto.UsageReport{RunID: runC, Source: proto.UsageHarborPilot, Final: true})
	if u, _ := svc.ForRun(ctx, "spc_a", runC); u.Source != proto.UsageUnavailable {
		t.Fatalf("a node claimed the harbor-pilot source: %+v", u)
	}
	if err := svc.s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO run_gateway_usage (run_id, space_id, state, ended_at, reads, next_attempt_at, read_at, models_json, costs_json, attempts, unknown_usage, updated_at)
				VALUES ('` + runC + `', 'spc_a', 'pending', 0, 1, 0, 5, '[{"model":"coding/default","input_tokens":9,"requests":2}]',
				'[{"kind":"estimated_api_charge","currency":"USD","amount":"0.01"}]', 2, 1, 5)`,
			`INSERT INTO run_usage (run_id, space_id, source, models_json, final, note, updated_at) VALUES ('` + runC + `', 'spc_a', 'harbor-pilot', '[{"model":"coding/default","requests":2}]', 0, '', 5)`,
		} {
			if _, err := tx.SQL.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	u, err := svc.ForRun(ctx, "spc_a", runC)
	if err != nil || u.Source != proto.UsageHarborPilot || !u.Known || u.Gateway == nil || u.Gateway.State != "pending" || u.Gateway.Requests != 2 ||
		u.Gateway.UnknownUsage != 1 || len(u.Gateway.Models) != 1 || len(u.Gateway.Costs) != 1 || u.Gateway.ReadAt == nil || u.Gateway.Source != proto.UsageHarborPilot {
		t.Fatalf("gateway usage %+v %v", u, err)
	}
	svc.Report(ctx, "nod_1", proto.UsageReport{RunID: runC, Source: proto.UsageUnavailable, Final: true})
	if u, _ := svc.ForRun(ctx, "spc_a", runC); u.Source != proto.UsageHarborPilot {
		t.Fatalf("unavailable replaced the gateway's figures: %+v", u)
	}
	svc.Report(ctx, "nod_1", proto.UsageReport{RunID: runC, Source: proto.UsageClaudeTranscript, Final: true, Models: []proto.ModelUsage{{Model: "m", Requests: 3}}})
	if u, _ := svc.ForRun(ctx, "spc_a", runC); u.Source != proto.UsageClaudeTranscript || u.Gateway == nil {
		t.Fatalf("the harness's figures didn't replace the fill: %+v", u)
	}
}
