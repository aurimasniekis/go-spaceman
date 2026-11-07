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
	if err := svc.queueRevocation(context.Background(), cred, "run_ended"); err != nil {
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

// SM-GW-005: the revoker revokes queued credentials with the parent token. 204 and 404 settle as revoked, a failing
// gateway is retried with backoff until the credential expires (then expired), a refusal is failed and audited, and a
// revoked parent (401) settles as revoked (its children went with it), audited.
func TestRevokerOutcomes(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	connect(t, svc, f)
	l := newLifecycle(svc)
	ctx := context.Background()

	ok := queueMinted(t, svc, f)
	exec(t, s, `INSERT INTO gateway_revocations (credential_id, reason, expires_at, next_attempt_at, requested_at) VALUES ('crd_unknown', 'run_ended', ?, 0, 0)`,
		time.Now().Add(time.Hour).UnixMicro())
	exec(t, s, `INSERT INTO gateway_revocations (credential_id, reason, expires_at, next_attempt_at, requested_at) VALUES ('crd_old', 'run_ended', 5, 0, 0)`)
	exec(t, s, `INSERT INTO gateway_revocations (credential_id, reason, expires_at, next_attempt_at, requested_at) VALUES ('crd_later', 'run_ended', ?, ?, 0)`,
		time.Now().Add(time.Hour).UnixMicro(), time.Now().Add(time.Minute).UnixMicro())
	n, err := l.RevokeDue(ctx)
	if err != nil || n != 3 {
		t.Fatalf("settled %d %v", n, err)
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

// SM-GW-005 (end-path table, "parent rotated"): a new parent can't revoke the old one's children, so replacing the parent
// revokes every live run token with the old parent first, records it, and says so.
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
	cur, _ := svc.GetConnection(ctx)
	// Same binding, another parent. The fake knows one parent at a time: the new one for the validation probe (its mint
	// and its revoke), then the old one, which must be what revokes the run's token.
	newParent := "hpk_parent_" + strings.Repeat("n", 24)
	f.Set(func(h *hpfake.HP) { h.Parent = newParent })
	var revokedWith string
	// The fake knows one parent at a time: accept the old one for the child revocation by switching on the probe's revoke.
	var phase atomic.Int32
	old := "hpk_parent_" + strings.Repeat("p", 24)
	f.Set(func(h *hpfake.HP) {
		h.Hook = func(r *http.Request) {
			if r.Method == http.MethodDelete {
				switch phase.Add(1) {
				case 1: // the probe's own revoke, with the new parent
				case 2: // the run token, with the old parent
					revokedWith = r.Header.Get("Authorization")
					h.Set(func(h *hpfake.HP) { h.Parent = old })
				}
			}
		}
	})
	out, err := svc.PutConnection(ctx, admin, cur.Version, ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true, ParentToken: newParent})
	if err != nil {
		t.Fatal(err)
	}
	if revokedWith != "Bearer "+old {
		t.Fatalf("run token revoked with %q, want the old parent", revokedWith)
	}
	if !strings.Contains(strings.Join(out.Warnings, "\n"), "tokens of 1 live runs were revoked") {
		t.Fatalf("warnings %q", out.Warnings)
	}
	if count(t, s, `SELECT count(*) FROM gateway_revocations WHERE run_id = 'run_1' AND reason = 'parent_rotated' AND state = 'revoked'`) != 1 ||
		count(t, s, `SELECT count(*) FROM run_gateway_credentials WHERE run_id = 'run_1' AND token_enc IS NULL`) != 1 {
		t.Fatal("rotation not recorded")
	}
	if count(t, s, `SELECT count(*) FROM gateway_revocations WHERE state = 'pending'`) != 0 {
		t.Fatal("rotation left a pending revocation the new parent can't do")
	}
}
