package harborpilot

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hpfake"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

const runUsageJSON = `{"group_by":"model","groups":[` +
	`{"key":"coding/default","attempts":3,"tokens":{"input":120,"output":30,"cache_read":10,"cache_write":5,"unknown_usage_attempts":1}},` +
	`{"key":"coding/fast","attempts":1,"tokens":{"unknown_usage_attempts":1}}],` +
	`"totals":[{"kind":"estimated_api_charge","currency":"USD","amount":"0.0042"},{"kind":"list_equivalent","currency":"USD","amount":"0.01"}]}`

// endRun queues a run's read-back as fleet's end path does, due at once.
func endRun(t *testing.T, s *store.Store, runID string) {
	t.Helper()
	exec(t, s, `INSERT INTO run_gateway_usage (run_id, space_id, ended_at, next_attempt_at, updated_at) VALUES (?, 'spc_a', ?, 1, 0)`, runID, s.Now().UnixMicro())
}

func dueNow(t *testing.T, s *store.Store, runID string) {
	t.Helper()
	exec(t, s, `UPDATE run_gateway_usage SET next_attempt_at = 1 WHERE run_id = ?`, runID)
}

func gwUsage(t *testing.T, s *store.Store, runID string) db.RunGatewayUsage {
	t.Helper()
	u, err := s.R().GetRunGatewayUsage(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// SM-GW-008 (simulated: hpfake): an ended gateway run's usage is read back with the run's trusted ref and the
// connection's binding, at each scheduled read; the last read is final. It fills the usage of a run whose harness
// reported none (Codex: unavailable) with source harbor-pilot, keeps unknown token counts unknown, and prices it with
// Harbor Pilot's own estimate.
func TestReadDueUsageFillsUnavailableUsage(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	l := newLifecycle(svc)
	ctx := context.Background()
	gatewayRun(t, s, "run_1")
	// The harness's final report: no usage (Codex).
	exec(t, s, `INSERT INTO run_usage (run_id, space_id, source, final, note, updated_at) VALUES ('run_1', 'spc_a', 'unavailable', 1, 'codex', 0)`)
	f.Set(func(h *hpfake.HP) { h.Usage = map[string]string{"spaceman.run:run_1": runUsageJSON} })
	if next, err := l.NextUsageRead(ctx); err != nil || !next.IsZero() {
		t.Fatalf("next %v %v", next, err)
	}
	endRun(t, s, "run_1")
	if next, _ := l.NextUsageRead(ctx); next.IsZero() {
		t.Fatal("no read due")
	}
	for i := range UsageReads {
		if n, err := l.ReadDueUsage(ctx); err != nil || n != 1 {
			t.Fatalf("read %d: %d %v", i, n, err)
		}
		u := gwUsage(t, s, "run_1")
		final := i == len(UsageReads)-1
		if (u.State == "final") != final || u.Reads != int64(i+1) || u.Attempts != 4 || u.UnknownUsage != 2 {
			t.Fatalf("read %d: %+v", i, u)
		}
		if !final {
			if want := u.EndedAt + UsageReads[i+1].Microseconds(); u.NextAttemptAt != want {
				t.Fatalf("next read at %d, want %d", u.NextAttemptAt, want)
			}
			dueNow(t, s, "run_1")
		}
		ru, err := s.R().GetRunUsage(ctx, "run_1")
		if err != nil || ru.Source != proto.UsageHarborPilot || (ru.Final == 1) != final {
			t.Fatalf("run usage %+v %v", ru, err)
		}
	}
	// Stopped after the final read: nothing is due any more, and a round reads nothing, even long after.
	if next, err := l.NextUsageRead(ctx); err != nil || !next.IsZero() {
		t.Fatalf("still due after the final read: %v %v", next, err)
	}
	queries := 0
	f.Set(func(h *hpfake.HP) { queries = len(h.UsageQueries) })
	exec(t, s, `UPDATE run_gateway_usage SET next_attempt_at = 1 WHERE run_id = 'run_1'`) // a stale due time changes nothing
	if n, _ := l.ReadDueUsage(ctx); n != 0 {
		t.Fatal("read again after the final read")
	}
	f.Set(func(h *hpfake.HP) {
		if len(h.UsageQueries) != queries {
			t.Error("Harbor Pilot was asked again after the final read")
		}
	})
	var q url.Values
	f.Set(func(h *hpfake.HP) { q, _ = url.ParseQuery(h.UsageQueries[0]) })
	if q.Get("ref") != "spaceman.run:run_1" || q.Get("binding_id") != "bnd_fake" || q.Get("group_by") != "model" || q.Get("from") == "" || q.Get("to") == "" {
		t.Fatalf("query %v", q)
	}
	ru, _ := s.R().GetRunUsage(ctx, "run_1")
	var models []proto.ModelUsage
	_ = json.Unmarshal([]byte(ru.ModelsJson), &models)
	if len(models) != 2 || models[0].Model != "coding/default" || *models[0].Input != 120 || models[0].Requests != 3 ||
		models[1].Input != nil || models[1].Requests != 1 {
		t.Fatalf("models %s", ru.ModelsJson)
	}
	var est proto.CostEstimate
	if err := json.Unmarshal([]byte(ru.EstimateJson), &est); err != nil || est.USD != 0.0042 || est.Source != "harbor-pilot" || !strings.Contains(est.Assumptions, "estimated_api_charge") {
		t.Fatalf("estimate %s", ru.EstimateJson)
	}
	if !strings.Contains(ru.Note, "2 of 4 requests had no usage reported") {
		t.Fatalf("note %q", ru.Note)
	}
	// A late "unavailable" report from the node doesn't replace the gateway's figures.
	exec(t, s, `INSERT INTO run_usage (run_id, space_id, source, models_json, final, note, updated_at) VALUES ('run_1', 'spc_a', 'unavailable', '[]', 1, 'late', 1)
		ON CONFLICT (run_id) DO UPDATE SET source = excluded.source, note = excluded.note
		WHERE (run_usage.final = 0 AND NOT (run_usage.source = 'harbor-pilot' AND excluded.source = 'unavailable'))
		   OR (run_usage.source = 'harbor-pilot' AND excluded.source NOT IN ('unavailable', 'harbor-pilot'))`)
	if ru, _ := s.R().GetRunUsage(ctx, "run_1"); ru.Source != proto.UsageHarborPilot {
		t.Fatalf("replaced by %s", ru.Source)
	}
}

// A harness's own usage (Claude's transcript) is never overwritten: the gateway's figures are kept beside it only.
func TestReadDueUsageKeepsHarnessUsage(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	l := newLifecycle(svc)
	ctx := context.Background()
	gatewayRun(t, s, "run_1")
	exec(t, s, `INSERT INTO run_usage (run_id, space_id, source, models_json, final, note, updated_at) VALUES ('run_1', 'spc_a', 'claude-transcript', '[{"model":"m","requests":7}]', 0, '', 0)`)
	f.Set(func(h *hpfake.HP) { h.Usage = map[string]string{"spaceman.run:run_1": runUsageJSON} })
	endRun(t, s, "run_1")
	if _, err := l.ReadDueUsage(ctx); err != nil {
		t.Fatal(err)
	}
	ru, _ := s.R().GetRunUsage(ctx, "run_1")
	if ru.Source != proto.UsageClaudeTranscript || !strings.Contains(ru.ModelsJson, `"requests":7`) {
		t.Fatalf("harness usage overwritten: %+v", ru)
	}
	if u := gwUsage(t, s, "run_1"); u.Attempts != 4 || u.Reads != 1 {
		t.Fatalf("gateway usage %+v", u)
	}
}

// A Claude run whose transcript arrives after the gateway's read: the harness's own figures replace the fill.
func TestHarnessReportReplacesGatewayFill(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	l := newLifecycle(svc)
	ctx := context.Background()
	gatewayRun(t, s, "run_1")
	f.Set(func(h *hpfake.HP) { h.Usage = map[string]string{"spaceman.run:run_1": runUsageJSON} })
	endRun(t, s, "run_1")
	for range UsageReads {
		_, _ = l.ReadDueUsage(ctx)
		dueNow(t, s, "run_1")
	}
	if ru, _ := s.R().GetRunUsage(ctx, "run_1"); ru.Source != proto.UsageHarborPilot || ru.Final != 1 {
		t.Fatalf("fill %+v", ru)
	}
	if err := s.W().TxNamed(ctx, "report", func(tx *store.Tx) error {
		return tx.Q.UpsertRunUsage(ctx, db.UpsertRunUsageParams{RunID: "run_1", SpaceID: "spc_a", Source: proto.UsageClaudeTranscript, ModelsJson: `[{"model":"m","requests":4}]`,
			Final: 1, UpdatedAt: 1})
	}); err != nil {
		t.Fatal(err)
	}
	if ru, _ := s.R().GetRunUsage(ctx, "run_1"); ru.Source != proto.UsageClaudeTranscript {
		t.Fatalf("the transcript didn't replace the fill: %+v", ru)
	}
}

// Failures never write zero usage: no service-principal token settles as unavailable; a refusal fails; a network or
// 5xx failure is retried with backoff until an hour after the end, then fails.
func TestReadDueUsageFailures(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	l := newLifecycle(svc)
	ctx := context.Background()
	for _, id := range []string{"run_1", "run_2", "run_3"} {
		gatewayRun(t, s, id)
	}

	f.Set(func(h *hpfake.HP) { h.FailUsage = 503 })
	endRun(t, s, "run_1")
	if n, err := l.ReadDueUsage(ctx); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	u := gwUsage(t, s, "run_1")
	if u.State != "pending" || u.Failures != 1 || u.NextAttemptAt-s.Now().UnixMicro() < (25*time.Second).Microseconds() || !strings.Contains(u.LastError, "status 503") {
		t.Fatalf("retry %+v", u)
	}
	exec(t, s, `UPDATE run_gateway_usage SET next_attempt_at = 0, ended_at = ? WHERE run_id = 'run_1'`, s.Now().Add(-2*time.Hour).UnixMicro())
	if n, _ := l.ReadDueUsage(ctx); n != 1 {
		t.Fatal("not given up after an hour")
	}
	if u := gwUsage(t, s, "run_1"); u.State != "failed" {
		t.Fatalf("give up %+v", u)
	}

	f.Set(func(h *hpfake.HP) { h.FailUsage = 403 })
	endRun(t, s, "run_2")
	_, _ = l.ReadDueUsage(ctx)
	if u := gwUsage(t, s, "run_2"); u.State != "failed" || !strings.Contains(u.LastError, "permission denied") {
		t.Fatalf("refused %+v", u)
	}
	if _, err := s.R().GetRunUsage(ctx, "run_2"); err == nil {
		t.Fatal("a failed read wrote usage")
	}

	f.Set(func(h *hpfake.HP) { h.FailUsage = 0 })
	if _, err := svc.PutConnection(ctx, admin, 1, ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true, RemoveServiceToken: true}); err != nil {
		t.Fatal(err)
	}
	endRun(t, s, "run_3")
	if n, _ := l.ReadDueUsage(ctx); n != 1 {
		t.Fatal("not settled")
	}
	if u := gwUsage(t, s, "run_3"); u.State != "unavailable" || !strings.Contains(u.LastError, "no service-principal token") {
		t.Fatalf("unavailable %+v", u)
	}
	if next, _ := l.NextUsageRead(ctx); !next.IsZero() {
		t.Fatalf("still due at %v", next)
	}
}

// The dispatch mint asks Harbor Pilot to check its budgets, and a refusal fails the run with Harbor Pilot's reason.
func TestDispatchMintChecksBudget(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	l := newLifecycle(svc)
	ctx := context.Background()
	if _, err := l.Token(ctx, gatewayRun(t, s, "run_1"), "s"); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	f.Set(func(h *hpfake.HP) { _ = json.Unmarshal([]byte(h.Bodies[len(h.Bodies)-1]), &body) })
	if body["check_budget"] != true {
		t.Fatalf("mint body %v", body)
	}
	f.Set(func(h *hpfake.HP) { h.FailMint = 429 })
	_, err := l.Token(ctx, gatewayRun(t, s, "run_2"), "s")
	if err == nil || !strings.Contains(err.Error(), "budget daily is exhausted") || !strings.Contains(err.Error(), "a blocking Harbor Pilot budget is used up") {
		t.Fatalf("refused mint: %v", err)
	}
}
