package harborpilot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hpfake"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics/metricstest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// gatewaySpace connects the fake and opts spc_a in for both harnesses with project prj_web.
func gatewaySpace(t *testing.T, svc *Service, f *hpfake.HP, in SpaceInput) {
	t.Helper()
	connect(t, svc, f)
	if in.ProjectID == "" {
		in.ProjectID = "prj_web"
	}
	in.Enabled = true
	if _, err := svc.PutSpace(context.Background(), admin, "spc_a", 0, in, true); err != nil {
		t.Fatal(err)
	}
}

func exec(t *testing.T, s *store.Store, q string, args ...any) {
	t.Helper()
	if err := s.W().TxNamed(context.Background(), "test", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(context.Background(), q, args...)
		return err
	}); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func count(t *testing.T, s *store.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.ReadDB().QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// gatewayRun inserts a running gateway run of spc_a and returns it as fleet sees it.
func gatewayRun(t *testing.T, s *store.Store, id string) *fleet.Run {
	t.Helper()
	spec := fleet.LaunchSpec{Gateway: &fleet.GatewaySpec{Kind: fleet.GatewayKindHarborPilot, ProjectID: "prj_web"}}
	b, _ := json.Marshal(spec)
	exec(t, s, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
		VALUES (?, 'spc_a', 'nod_1', 1, 'worker', 'implementor', 'claude-code', 'ins_'||?, 'running', 'pending', ?, 0, 't', 0)`, id, id, string(b))
	return &fleet.Run{ID: id, SpaceID: "spc_a", NodeID: "nod_1", Harness: "claude-code", InstanceID: "ins_" + id, Launch: spec}
}

func newLifecycle(svc *Service) *Lifecycle {
	return NewLifecycle(svc, LifecycleOptions{MintRetry: []time.Duration{0, 0, 0}})
}

// SM-GW-003 (simulated: hpfake): a run mints once and every re-dispatch reuses the stored token; the mint carries the
// run's bound refs (space, run, instance, session, project and its task), the space's TTL and only invoke and models;
// the token is stored sealed, never in plain text.
func TestRunTokenMintOnceAndReuse(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{TTLSeconds: 3600})
	l := newLifecycle(svc)
	ctx := context.Background()
	run := gatewayRun(t, s, "run_1")
	exec(t, s, `INSERT INTO tasks (id, space_id, title, rank, status, assignee_run, created_at, created_by, updated_at)
		VALUES ('tsk_1', 'spc_a', 'T', 1, 'running', 'run_1', 0, 't', 0)`)
	before := 0
	f.Set(func(h *hpfake.HP) { before = len(h.Minted) })

	sec, err := l.Token(ctx, run, "11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	tok := sec[proto.SecretGatewayToken]
	if !strings.HasPrefix(tok, "hpk_child_") || len(sec) != 1 {
		t.Fatalf("secrets %v", sec)
	}
	for range 3 {
		again, err := l.Token(ctx, run, "11111111-2222-4333-8444-555555555555")
		if err != nil || again[proto.SecretGatewayToken] != tok {
			t.Fatalf("re-dispatch %v %v", again, err)
		}
	}
	var n int
	var body map[string]any
	f.Set(func(h *hpfake.HP) {
		n = len(h.Minted) - before
		_ = json.Unmarshal([]byte(h.Bodies[len(h.Bodies)-1]), &body)
	})
	if n != 1 {
		t.Fatalf("minted %d times, want once", n)
	}
	refs, _ := body["bound_refs"].(map[string]any)
	want := map[string]any{RefSpace: "spc_a", RefRun: "run_1", RefInstance: "ins_run_1", RefSession: "11111111-2222-4333-8444-555555555555",
		RefProject: "prj_web", RefTask: "tsk_1"}
	if len(refs) != len(want) {
		t.Fatalf("bound refs %v", refs)
	}
	for k, v := range want {
		if refs[k] != v {
			t.Fatalf("bound ref %s = %v, want %v (%v)", k, refs[k], v, refs)
		}
	}
	scopes, _ := body["scopes"].([]any)
	if body["ttl_seconds"] != float64(3600) || len(scopes) != 2 || scopes[0] != ScopeInvoke || scopes[1] != ScopeModels || body["name"] != "spaceman-run-run_1" {
		t.Fatalf("mint body %v", body)
	}
	if count(t, s, `SELECT count(*) FROM run_gateway_credentials WHERE run_id = 'run_1' AND instr(token_enc, ?) = 0`, tok) != 1 {
		t.Fatal("the run token is not stored sealed")
	}
}

// Concurrent dispatches of one run (a node reconnecting while its old connection still sends) mint once.
func TestRunTokenSingleFlight(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	l := newLifecycle(svc)
	run := gatewayRun(t, s, "run_1")
	var mints atomic.Int32
	f.Set(func(h *hpfake.HP) {
		h.Hook = func(r *http.Request) {
			if r.Method == http.MethodPost {
				mints.Add(1)
				time.Sleep(50 * time.Millisecond)
			}
		}
	})
	var wg sync.WaitGroup
	toks := make([]string, 4)
	for i := range toks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sec, err := l.Token(context.Background(), run, "run_1")
			if err != nil {
				t.Error(err)
			}
			toks[i] = sec[proto.SecretGatewayToken]
		}()
	}
	wg.Wait()
	if mints.Load() != 1 || toks[0] == "" || toks[1] != toks[0] || toks[2] != toks[0] || toks[3] != toks[0] {
		t.Fatalf("mints %d, tokens %v", mints.Load(), toks)
	}
}

// A stored token close to its expiry is replaced at the next dispatch; the old one is queued for revocation.
func TestRunTokenReplacedNearExpiry(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	l := newLifecycle(svc)
	ctx := context.Background()
	run := gatewayRun(t, s, "run_1")
	first, err := l.Token(ctx, run, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	exec(t, s, `UPDATE run_gateway_credentials SET expires_at = ? WHERE run_id = 'run_1'`, time.Now().Add(time.Minute).UnixMicro())
	second, err := l.Token(ctx, run, "run_1")
	if err != nil || second[proto.SecretGatewayToken] == first[proto.SecretGatewayToken] {
		t.Fatalf("replacement %v %v", second, err)
	}
	if count(t, s, `SELECT count(*) FROM gateway_revocations WHERE reason = 'token_replaced' AND state = 'pending' AND run_id = 'run_1'`) != 1 {
		t.Fatal("the replaced token is not queued for revocation")
	}
}

// SM-GW-003, Q1: a mint that fails fails the run with a readable reason (it wraps fleet.ErrGatewayUnavailable, so the run
// ends as gateway_unavailable) and never returns any other secret. Failures worth retrying are retried first.
func TestRunTokenMintFailures(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	l := newLifecycle(svc)
	ctx := context.Background()

	f.Set(func(h *hpfake.HP) { h.ParentScopes = []string{ScopeInvoke, ScopeModels} })
	sec, err := l.Token(ctx, gatewayRun(t, s, "run_1"), "run_1")
	if !errors.Is(err, fleet.ErrGatewayUnavailable) || !strings.Contains(err.Error(), "parent token lacks tokens:mint") || sec != nil {
		t.Fatalf("lacks mint: %v %v", sec, err)
	}
	f.Set(func(h *hpfake.HP) { h.ParentScopes = []string{ScopeInvoke, ScopeModels, ScopeMint}; h.FailMint = 429 })
	if _, err := l.Token(ctx, gatewayRun(t, s, "run_2"), "run_2"); !errors.Is(err, fleet.ErrGatewayUnavailable) || !strings.Contains(err.Error(), "budget daily is exhausted") {
		t.Fatalf("budget refusal: %v", err)
	}
	// Two 500s, then a mint that works: retried, not failed.
	var calls atomic.Int32
	f.Set(func(h *hpfake.HP) {
		h.FailMint = 500
		h.Hook = func(r *http.Request) {
			if r.Method == http.MethodPost && calls.Add(1) == 3 {
				h.Set(func(h *hpfake.HP) { h.FailMint = 0 })
			}
		}
	})
	if _, err := l.Token(ctx, gatewayRun(t, s, "run_3"), "run_3"); err != nil || calls.Load() != 3 {
		t.Fatalf("retried mint: %v after %d calls", err, calls.Load())
	}
	f.Set(func(h *hpfake.HP) { h.FailMint, h.Hook = 500, nil })
	calls.Store(0)
	if _, err := l.Token(ctx, gatewayRun(t, s, "run_4"), "run_4"); !errors.Is(err, fleet.ErrGatewayUnavailable) || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("5xx after retries: %v", err)
	}
	// No connection: unavailable, with a reason.
	exec(t, s, `DELETE FROM harbor_pilot_connection`)
	if _, err := l.Token(ctx, gatewayRun(t, s, "run_5"), "run_5"); !errors.Is(err, fleet.ErrGatewayUnavailable) || !strings.Contains(err.Error(), "no Harbor Pilot connection") {
		t.Fatalf("no connection: %v", err)
	}
}

// A run stopped while its token was minted gets no token: the new credential is queued for revocation at once (its
// stop found nothing to revoke), and the dispatch is not failed as gateway_unavailable.
func TestRunTokenMintedForAStoppedRunIsRevoked(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	l := newLifecycle(svc)
	run := gatewayRun(t, s, "run_1")
	f.Set(func(h *hpfake.HP) {
		h.Hook = func(r *http.Request) {
			if r.Method == http.MethodPost {
				exec(t, s, `UPDATE runs SET desired = 'stopped' WHERE id = 'run_1'`)
			}
		}
	})
	sec, err := l.Token(context.Background(), run, "run_1")
	if err == nil || errors.Is(err, fleet.ErrGatewayUnavailable) || sec != nil {
		t.Fatalf("stopped run: %v %v", sec, err)
	}
	if count(t, s, `SELECT count(*) FROM gateway_revocations WHERE run_id = 'run_1' AND reason = 'run_stopped'`) != 1 ||
		count(t, s, `SELECT count(*) FROM run_gateway_credentials`) != 0 {
		t.Fatal("the stopped run's credential is not queued for revocation")
	}
	// A run whose token was revoked (its end path ran) gets no new one.
	run2 := gatewayRun(t, s, "run_2")
	exec(t, s, `INSERT INTO run_gateway_credentials (run_id, space_id, credential_id, token_enc, expires_at, created_at) VALUES ('run_2', 'spc_a', 'crd_gone', NULL, ?, 0)`,
		time.Now().Add(time.Hour).UnixMicro())
	if _, err := l.Token(context.Background(), run2, "run_2"); !errors.Is(err, fleet.ErrGatewayUnavailable) || !strings.Contains(err.Error(), "already revoked") {
		t.Fatalf("revoked token: %v", err)
	}
}

// ADR-0060 decision 4, Q1: launches of a routed harness use no account. Naming one is refused, as is a model outside the
// space's allowed aliases; a harness the space does not route keeps its accounts; no connection refuses the launch
// before and inside its transaction; the budget part of PreLaunch is HP-T5's interface.
func TestRouteAndPreLaunch(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{Harnesses: []string{"claude-code"}, AllowedModelAliases: []string{"claude-sonnet-4-5", "fast"}})
	ctx := context.Background()
	budget := &fakeBudget{}
	l := NewLifecycle(svc, LifecycleOptions{Budget: budget})
	route := func(spaceID, harness, account, model string) (*fleet.GatewaySpec, error) {
		var out *fleet.GatewaySpec
		err := s.W().TxNamed(ctx, "route", func(tx *store.Tx) error {
			var err error
			out, err = l.Route(ctx, tx, spaceID, harness, account, model)
			return err
		})
		return out, err
	}
	gw, err := route("spc_a", "claude-code", "", "fast")
	if err != nil || gw == nil || gw.Kind != fleet.GatewayKindHarborPilot || gw.ProjectID != "prj_web" {
		t.Fatalf("route %+v %v", gw, err)
	}
	if gw, err := route("spc_a", "codex", "acc_1", ""); gw != nil || err != nil {
		t.Fatalf("codex is not routed: %+v %v", gw, err)
	}
	if gw, err := route("spc_b", "claude-code", "", ""); gw != nil || err != nil {
		t.Fatalf("spc_b did not opt in: %+v %v", gw, err)
	}
	ae := appErr(t, func() error { _, err := route("spc_a", "claude-code", "acc_direct", "fast"); return err }(), apperr.CodePrecondition)
	if !strings.Contains(ae.Message, "can't name an account") {
		t.Fatalf("account named: %q", ae.Message)
	}
	ae = appErr(t, func() error { _, err := route("spc_a", "claude-code", "", "opus"); return err }(), apperr.CodePrecondition)
	if !strings.Contains(ae.Message, `model "opus" is not allowed`) {
		t.Fatalf("model: %q", ae.Message)
	}
	code(t, func() error { _, err := route("spc_a", "claude-code", "", ""); return err }(), apperr.CodePrecondition)

	if err := l.PreLaunch(ctx, "spc_a", "claude-code"); err != nil || budget.calls.Load() != 1 || budget.last != "spc_a" {
		t.Fatalf("prelaunch %v, budget calls %d", err, budget.calls.Load())
	}
	if err := l.PreLaunch(ctx, "spc_a", "codex"); err != nil || budget.calls.Load() != 1 {
		t.Fatalf("codex prelaunch %v, budget calls %d", err, budget.calls.Load())
	}
	budget.err = apperr.New(apperr.CodeRateLimited, "budget used up")
	code(t, l.PreLaunch(ctx, "spc_a", "claude-code"), apperr.CodeRateLimited)

	exec(t, s, `DELETE FROM harbor_pilot_connection`)
	ae = appErr(t, l.PreLaunch(ctx, "spc_a", "claude-code"), apperr.CodePrecondition)
	if !strings.Contains(ae.Message, "no Harbor Pilot connection") || !strings.Contains(ae.Message, "never fall back") {
		t.Fatalf("no connection: %q", ae.Message)
	}
	code(t, func() error { _, err := route("spc_a", "claude-code", "", "fast"); return err }(), apperr.CodePrecondition)
}

type fakeBudget struct {
	calls atomic.Int32
	last  string
	err   error
}

func (b *fakeBudget) CheckBudget(_ context.Context, spaceID string, s *SpaceSettings) error {
	b.calls.Add(1)
	b.last = spaceID
	if s.ProjectID != "prj_web" {
		return errors.New("wrong settings")
	}
	return b.err
}

// queue inserts a pending revocation of a credential the fake minted.
func queueMinted(t *testing.T, svc *Service, f *hpfake.HP) string {
	t.Helper()
	c, err := svc.client(f.URL(), true)
	if err != nil {
		t.Fatal(err)
	}
	cred, _, err := c.Mint(context.Background(), f.Parent, MintRequest{Name: "x", Scopes: RunScopes, TTLSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.queueRevocation(context.Background(), cred, "run_ended", ParentFingerprint(f.Parent)); err != nil {
		t.Fatal(err)
	}
	return cred.ID
}

func revocation(t *testing.T, s *store.Store, id string) (state string, attempts int, lastErr string, next int64) {
	t.Helper()
	if err := s.ReadDB().QueryRowContext(context.Background(), `SELECT state, attempts, last_error, next_attempt_at FROM gateway_revocations WHERE credential_id = ?`, id).
		Scan(&state, &attempts, &lastErr, &next); err != nil {
		t.Fatal(err)
	}
	return
}

// SM-GW-005: the revoker revokes queued credentials with the parent token. 204 and 404 (from the parent that minted
// the credential) settle as revoked, a 404 for a credential of an unknown parent is failed, a failing gateway is
// retried with backoff until the credential expires (then expired), a refusal is failed and audited, and a revoked
// parent (401) settles as revoked (its children went with it), audited.
func TestRevokerOutcomes(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	connect(t, svc, f)
	l := newLifecycle(svc)
	ctx := context.Background()

	ok := queueMinted(t, svc, f)
	exec(t, s, `INSERT INTO gateway_revocations (credential_id, reason, expires_at, next_attempt_at, requested_at, parent_fp) VALUES ('crd_unknown', 'run_ended', ?, 0, 0, ?)`,
		time.Now().Add(time.Hour).UnixMicro(), ParentFingerprint(f.Parent))
	exec(t, s, `INSERT INTO gateway_revocations (credential_id, reason, expires_at, next_attempt_at, requested_at) VALUES ('crd_nofp', 'run_ended', ?, 0, 0)`,
		time.Now().Add(time.Hour).UnixMicro())
	exec(t, s, `INSERT INTO gateway_revocations (credential_id, reason, expires_at, next_attempt_at, requested_at) VALUES ('crd_old', 'run_ended', 5, 0, 0)`)
	exec(t, s, `INSERT INTO gateway_revocations (credential_id, reason, expires_at, next_attempt_at, requested_at) VALUES ('crd_later', 'run_ended', ?, ?, 0)`,
		time.Now().Add(time.Hour).UnixMicro(), time.Now().Add(time.Minute).UnixMicro())
	n, err := l.RevokeDue(ctx)
	if err != nil || n != 4 {
		t.Fatalf("settled %d %v", n, err)
	}
	if st, _, msg, _ := revocation(t, s, "crd_nofp"); st != "failed" || !strings.Contains(msg, "unknown) parent") {
		t.Fatalf("404 for an unknown parent: %s %q", st, msg)
	}
	if st, _, _, _ := revocation(t, s, ok); st != "revoked" || len(f.Unrevoked()) != 0 {
		t.Fatalf("revoked: %s, unrevoked %v", st, f.Unrevoked())
	}
	if st, _, msg, _ := revocation(t, s, "crd_unknown"); st != "revoked" || !strings.Contains(msg, "not found") {
		t.Fatalf("404: %s %q", st, msg)
	}
	if st, _, _, _ := revocation(t, s, "crd_old"); st != "expired" {
		t.Fatalf("expired: %s", st)
	}
	if st, _, _, _ := revocation(t, s, "crd_later"); st != "pending" {
		t.Fatalf("not yet due: %s", st)
	}
	if next, err := l.NextRevocation(ctx); err != nil || next.Before(time.Now().Add(50*time.Second)) {
		t.Fatalf("next %v %v", next, err)
	}

	// A failing gateway: retried with growing waits.
	f.Set(func(h *hpfake.HP) { h.FailRevoke = true })
	retry := queueMinted(t, svc, f)
	if n, err := l.RevokeDue(ctx); err != nil || n != 0 {
		t.Fatalf("retry round %d %v", n, err)
	}
	st, attempts, msg, next1 := revocation(t, s, retry)
	if st != "pending" || attempts != 1 || !strings.Contains(msg, "status 500") || next1 <= time.Now().UnixMicro() {
		t.Fatalf("after one failure: %s %d %q %d", st, attempts, msg, next1)
	}
	exec(t, s, `UPDATE gateway_revocations SET next_attempt_at = 0 WHERE credential_id = ?`, retry)
	_, _ = l.RevokeDue(ctx)
	_, attempts, _, next2 := revocation(t, s, retry)
	if attempts != 2 || next2-time.Now().UnixMicro() < (3*time.Second).Microseconds() {
		t.Fatalf("backoff did not grow: attempts %d, next in %v", attempts, time.Duration(next2-time.Now().UnixMicro())*time.Microsecond)
	}
	f.Set(func(h *hpfake.HP) { h.FailRevoke = false })
	exec(t, s, `UPDATE gateway_revocations SET next_attempt_at = 0 WHERE credential_id = ?`, retry)
	if n, _ := l.RevokeDue(ctx); n != 1 {
		t.Fatalf("recovered round %d", n)
	}

	// The parent was revoked in Harbor Pilot (401): its children are gone too; recorded and audited.
	gone := queueMinted(t, svc, f)
	f.Set(func(h *hpfake.HP) { h.Parent = "hpk_parent_rotated_elsewhere_000000" })
	if n, _ := l.RevokeDue(ctx); n != 1 {
		t.Fatalf("401 round %d", n)
	}
	if st, _, msg, _ := revocation(t, s, gone); st != "revoked" || !strings.Contains(msg, "parent token") {
		t.Fatalf("401: %s %q", st, msg)
	}
	if count(t, s, `SELECT count(*) FROM audit_events WHERE action = 'harbor_pilot.revocation.revoked' AND target_id = ?`, gone) != 1 {
		t.Fatal("a revoked parent is not audited")
	}
}

// SM-GW-005: the queue is durable. A revocation queued before central stopped (the crash came between the commit and
// the DELETE) is done by the next central's revoker, with no state but the database.
func TestRevocationSurvivesRestart(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	connect(t, svc, f)
	id := queueMinted(t, svc, f) // committed; the process "crashes" before any revoker ran
	restarted := newLifecycle(New(s, svc.audit, svc.changes, svc.kr, Options{}))
	next, err := restarted.NextRevocation(context.Background())
	if err != nil || next.IsZero() || next.After(time.Now()) {
		t.Fatalf("not re-armed: %v %v", next, err)
	}
	if n, err := restarted.RevokeDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("after restart %d %v", n, err)
	}
	if st, _, _, _ := revocation(t, s, id); st != "revoked" || len(f.Unrevoked()) != 0 {
		t.Fatalf("state %s, unrevoked %v", st, f.Unrevoked())
	}
}

// rotate replaces the fake's parent with a new one while the old stays valid in Harbor Pilot (nobody revoked it), and
// writes the new one to the connection.
func rotate(t *testing.T, svc *Service, f *hpfake.HP, fill string) (*Connection, string) {
	t.Helper()
	ctx := context.Background()
	cur, err := svc.GetConnection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newParent := "hpk_parent_" + strings.Repeat(fill, 24)
	var old string
	f.Set(func(h *hpfake.HP) { old, h.Parent, h.OtherParents = h.Parent, newParent, append(h.OtherParents, h.Parent) })
	out, err := svc.PutConnection(ctx, admin, cur.Version, ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true, ParentToken: newParent})
	if err != nil {
		t.Fatal(err)
	}
	return out, old
}

// SM-GW-005 (end-path table, "parent rotated"): a new parent can't revoke the old one's children, so replacing the parent
// revokes, with the old parent, every live run token and every pending revocation it minted, records it, and says so.
// Harbor Pilot (hpfake with per-parent ownership) confirms each credential is really revoked.
func TestParentRotationRevokesLiveRunTokens(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	l := newLifecycle(svc)
	ctx := context.Background()
	if _, err := l.Token(ctx, gatewayRun(t, s, "run_1"), "run_1"); err != nil {
		t.Fatal(err)
	}
	// A revocation queued while Harbor Pilot was unreachable, still pending at the rotation.
	pending := queueMinted(t, svc, f)
	var revokedWith []string
	f.Set(func(h *hpfake.HP) {
		h.Hook = func(r *http.Request) {
			if r.Method == http.MethodDelete {
				revokedWith = append(revokedWith, r.Header.Get("Authorization"))
			}
		}
	})
	out, old := rotate(t, svc, f, "n")
	// The probe's own revoke uses the new parent; the two credentials of the old parent use the old one.
	if len(revokedWith) != 3 || revokedWith[1] != "Bearer "+old || revokedWith[2] != "Bearer "+old {
		t.Fatalf("revoked with %q, want the old parent for both of its credentials", revokedWith)
	}
	if un := f.Unrevoked(); len(un) != 0 {
		t.Fatalf("Harbor Pilot still has unrevoked credentials %v", un)
	}
	if !strings.Contains(strings.Join(out.Warnings, "\n"), "2 Harbor Pilot tokens minted by the old parent") || out.FailedRevocations != 0 {
		t.Fatalf("warnings %q failed %d", out.Warnings, out.FailedRevocations)
	}
	if count(t, s, `SELECT count(*) FROM gateway_revocations WHERE run_id = 'run_1' AND reason = 'parent_rotated' AND state = 'revoked'`) != 1 ||
		count(t, s, `SELECT count(*) FROM run_gateway_credentials WHERE run_id = 'run_1' AND token_enc IS NULL`) != 1 {
		t.Fatal("rotation not recorded")
	}
	if st, _, _, _ := revocation(t, s, pending); st != "revoked" {
		t.Fatalf("pending revocation of the old parent: %s", st)
	}
	if count(t, s, `SELECT count(*) FROM gateway_revocations WHERE state = 'pending'`) != 0 {
		t.Fatal("rotation left a pending revocation the new parent can't do")
	}
}

// F4: when the connection write fails after the old parent's run tokens were revoked (here: the new parent belongs to
// another binding while a space uses Harbor Pilot), the old parent stays and the run keeps a token that no longer
// works. That fails safe: no token SpaceMan believes revoked stays valid, and the run only loses model access.
func TestParentRotationWriteFailureFailsSafe(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	l := newLifecycle(svc)
	ctx := context.Background()
	if _, err := l.Token(ctx, gatewayRun(t, s, "run_1"), "run_1"); err != nil {
		t.Fatal(err)
	}
	cur, _ := svc.GetConnection(ctx)
	newParent := "hpk_parent_" + strings.Repeat("b", 24)
	f.Set(func(h *hpfake.HP) { h.OtherParents, h.Parent, h.BindingID = []string{h.Parent}, newParent, "bnd_other" })
	_, err := svc.PutConnection(ctx, admin, cur.Version, ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true, ParentToken: newParent})
	code(t, err, apperr.CodePrecondition)
	if un := f.Unrevoked(); len(un) != 0 {
		t.Fatalf("Harbor Pilot still has unrevoked credentials %v", un)
	}
	after, _ := svc.GetConnection(ctx)
	if after.Version != cur.Version || after.ParentTokenVersion != cur.ParentTokenVersion {
		t.Fatalf("the connection changed: %+v", after)
	}
	if count(t, s, `SELECT count(*) FROM run_gateway_credentials WHERE run_id = 'run_1' AND token_enc IS NOT NULL`) != 1 {
		t.Fatal("the run's (revoked) token should still be stored: the write rolled back")
	}
}

// F1: a queued credential of an earlier parent that the rotation could not see reaches the revoker, which holds the new
// parent: Harbor Pilot answers 404, which proves nothing about it, so it is recorded as failed, audited, counted in the
// connection and the metric, never as revoked.
func TestRevokerForeignParentIsFailed(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	connect(t, svc, f)
	m := metrics.New()
	l := NewLifecycle(svc, LifecycleOptions{MintRetry: []time.Duration{0, 0, 0}, Metrics: m})
	ctx := context.Background()
	_, old := rotate(t, svc, f, "n")
	c, err := svc.client(f.URL(), true)
	if err != nil {
		t.Fatal(err)
	}
	cred, _, err := c.Mint(ctx, old, MintRequest{Name: "x", Scopes: RunScopes, TTLSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.queueRevocation(ctx, cred, "run_ended", ParentFingerprint(old)); err != nil {
		t.Fatal(err)
	}
	if n, err := l.RevokeDue(ctx); err != nil || n != 1 {
		t.Fatalf("settled %d %v", n, err)
	}
	if st, _, msg, _ := revocation(t, s, cred.ID); st != "failed" || !strings.Contains(msg, "proves nothing") {
		t.Fatalf("foreign 404: %s %q", st, msg)
	}
	if un := f.Unrevoked(); len(un) != 1 || un[0] != cred.ID {
		t.Fatalf("the fake shows %v unrevoked; the test expects exactly the foreign credential", un)
	}
	if count(t, s, `SELECT count(*) FROM audit_events WHERE action = 'harbor_pilot.revocation.failed' AND target_id = ?`, cred.ID) != 1 {
		t.Fatal("the failed revocation is not audited")
	}
	conn, err := svc.GetConnection(ctx)
	if err != nil || conn.FailedRevocations != 1 || !strings.Contains(strings.Join(conn.Warnings, "\n"), "1 token revocations failed") {
		t.Fatalf("connection %+v %v", conn, err)
	}
	if v := metricstest.Value(t, m, "spaceman_gateway_revocations_failed"); v != 1 {
		t.Fatalf("spaceman_gateway_revocations_failed = %v", v)
	}
}

// F1: a run token the old parent mints between the rotation's list and its commit is caught by the commit's check and
// revoked with the old parent in another round.
func TestParentRotationCatchesATokenMintedDuringIt(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	l := newLifecycle(svc)
	ctx := context.Background()
	if _, err := l.Token(ctx, gatewayRun(t, s, "run_1"), "run_1"); err != nil {
		t.Fatal(err)
	}
	run2 := gatewayRun(t, s, "run_2")
	cur, err := s.R().GetRunGatewayCredential(ctx, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	run1Cred := cur.CredentialID
	var once sync.Once
	var mintErr error
	f.Set(func(h *hpfake.HP) {
		h.Hook = func(r *http.Request) {
			// The rotation is revoking run_1's token: a dispatch of run_2 mints with the old parent meanwhile.
			if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/"+run1Cred) {
				once.Do(func() { _, mintErr = l.Token(ctx, run2, "run_2") })
			}
		}
	})
	out, _ := rotate(t, svc, f, "n")
	if mintErr != nil {
		t.Fatal(mintErr)
	}
	if un := f.Unrevoked(); len(un) != 0 {
		t.Fatalf("Harbor Pilot still has unrevoked credentials %v", un)
	}
	if !strings.Contains(strings.Join(out.Warnings, "\n"), "2 Harbor Pilot tokens minted by the old parent") {
		t.Fatalf("warnings %q", out.Warnings)
	}
	if count(t, s, `SELECT count(*) FROM run_gateway_credentials WHERE token_enc IS NOT NULL`) != 0 {
		t.Fatal("a run token of the old parent survived the rotation")
	}
}

// F1: a mint in flight when the rotation commits is stored after it: Token sees the parent changed, revokes its
// credential with the old parent and mints again with the new one.
func TestRunTokenMintedAcrossARotationIsRedone(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	l := newLifecycle(svc)
	ctx := context.Background()
	run := gatewayRun(t, s, "run_1")
	var arm atomic.Bool
	arm.Store(true)
	var newParent string
	f.Set(func(h *hpfake.HP) {
		h.Hook = func(r *http.Request) {
			if r.Method == http.MethodPost && arm.CompareAndSwap(true, false) {
				_, _ = rotate(t, svc, f, "n") // commits before the run's mint is answered
				f.Set(func(h *hpfake.HP) { newParent = h.Parent })
			}
		}
	})
	sec, err := l.Token(ctx, run, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	cur, err := s.R().GetRunGatewayCredential(ctx, "run_1")
	if err != nil || !cur.ParentFp.Valid || cur.ParentFp.String != ParentFingerprint(newParent) || sec[proto.SecretGatewayToken] != "hpk_child_"+cur.CredentialID {
		t.Fatalf("stored %+v %v", cur, err)
	}
	if un := f.Unrevoked(); len(un) != 1 || un[0] != cur.CredentialID {
		t.Fatalf("unrevoked %v, want only the new parent's token %s", un, cur.CredentialID)
	}
}

// A revoker round that can't use the connection (none, or a parent token that doesn't open) still reschedules every due
// row, so the shared deadline timer never fires again at once.
func TestRevokerReschedulesWithoutAConnection(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	connect(t, svc, f)
	l := newLifecycle(svc)
	ctx := context.Background()
	id := queueMinted(t, svc, f)
	exec(t, s, `UPDATE harbor_pilot_connection SET parent_token_enc = x'00'`)
	if _, err := l.RevokeDue(ctx); err != nil {
		t.Fatal(err)
	}
	st, attempts, msg, next := revocation(t, s, id)
	if st != "pending" || attempts != 1 || msg != "the parent token can't be opened" || next <= time.Now().UnixMicro() {
		t.Fatalf("unopenable parent: %s %d %q %d", st, attempts, msg, next)
	}
	exec(t, s, `DELETE FROM harbor_pilot_connection`)
	exec(t, s, `UPDATE gateway_revocations SET next_attempt_at = 0`)
	if _, err := l.RevokeDue(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _, msg, next := revocation(t, s, id); st != "pending" || msg != "no Harbor Pilot connection is configured" || next <= time.Now().UnixMicro() {
		t.Fatalf("no connection: %s %q %d", st, msg, next)
	}
}
