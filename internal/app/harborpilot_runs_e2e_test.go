package app_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hpfake"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hptest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// hpRunEnv is a central with a Harbor Pilot connection, a space routing both harnesses through it (with direct
// accounts bound too, which gateway runs must never get), and a scripted node at the newest protocol minor.
type hpRunEnv struct {
	e  *fleetEnv
	sp *fleet.Space
	n  *rawNode
}

func newHPRunEnv(t *testing.T, base, parent, service, project string) *hpRunEnv {
	t.Helper()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "gateway", Name: "Gateway", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	if _, err := e.c.HarborPilot.PutConnection(ctx, admin, 0, harborpilot.ConnectionInput{BaseURL: base, AllowInsecureHTTP: true, ParentToken: parent,
		ServiceToken: service}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.HarborPilot.PutSpace(ctx, admin, sp.ID, 0, harborpilot.SpaceInput{Enabled: true, ProjectID: project}, true); err != nil {
		t.Fatal(err)
	}
	h := &hpRunEnv{e: e, sp: sp}
	h.dial(t)
	return h
}

// dial connects a new scripted node session at the newest minor (it replaces the previous one) and waits for the
// space's lease.
func (h *hpRunEnv) dial(t *testing.T) { h.dialMinor(t, proto.MaxMinor) }

func (h *hpRunEnv) dialMinor(t *testing.T, minor int) {
	t.Helper()
	if h.n != nil {
		_ = h.n.conn.CloseNow()
	}
	h.n = dialRawNode(t, h.e, minor)
	h.n.next(t, proto.TypeWelcome)
	h.n.next(t, proto.TypeLeaseGrant)
	eventually(t, "lease", func() bool {
		s, _ := h.e.c.Fleet.GetSpace(context.Background(), h.sp.ID)
		return s.Lease != nil && s.Lease.Valid
	})
}

// launchCmd waits for the launch of a run on the scripted node and returns its wire payload.
func (h *hpRunEnv) launchCmd(t *testing.T, runID string) (proto.Command, proto.LaunchPayload) {
	t.Helper()
	for {
		var c proto.Command
		if err := h.n.next(t, proto.TypeCommand).Decode(&c); err != nil {
			t.Fatal(err)
		}
		if c.Kind != proto.CmdLaunch || c.RunID != runID {
			continue
		}
		var p proto.LaunchPayload
		if err := json.Unmarshal(c.Payload, &p); err != nil {
			t.Fatal(err)
		}
		return c, p
	}
}

func (h *hpRunEnv) report(t *testing.T, r *fleet.Run, state, reason string) {
	t.Helper()
	h.n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.ID, SpaceID: r.SpaceID, Generation: r.Generation, State: state, Reason: reason})
}

func (h *hpRunEnv) credential(t *testing.T, runID string) (id string, live bool) {
	t.Helper()
	var enc []byte
	err := h.e.c.Store.ReadDB().QueryRowContext(context.Background(), `SELECT credential_id, token_enc FROM run_gateway_credentials WHERE run_id = ?`, runID).Scan(&id, &enc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	} else if err != nil {
		t.Fatal(err)
	}
	return id, enc != nil
}

var reUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// F2 (ADR-0060 Q1): a gateway launch to an agent older than proto.MinorGatewayToken (a minor-8 agent, v0.1.0-rc.8)
// fails as agent_too_old with a readable reason, before anything is minted: such an agent would drop the token and
// start the harness on the node user's own login.
func TestHarborPilotGatewayNeedsNewAgent(t *testing.T) {
	t.Parallel()
	f := hpfake.New(t)
	h := newHPRunEnv(t, f.URL(), f.Parent, f.Service, "prj_web")
	h.dialMinor(t, proto.MinorGatewayToken-1)
	ctx := context.Background()
	mints := func() int { n := 0; f.Set(func(x *hpfake.HP) { n = len(x.Bodies) }); return n }
	base := mints()
	r, err := h.e.c.Fleet.LaunchRun(ctx, admin, h.sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: "claude-code", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "failed as agent_too_old", func() bool {
		x := h.e.run1(r.ID)
		return x.Observed == "failed" && x.EndReason == "agent_too_old"
	})
	if x := h.e.run1(r.ID); !strings.Contains(x.Error, "gateway runs need protocol minor 9, which delivers the run's token to the harness; the node negotiated 8: upgrade its agent") {
		t.Fatalf("reason %q", x.Error)
	}
	if n := mints() - base; n != 0 {
		t.Fatalf("minted %d tokens for an agent that would drop them", n)
	}
	if id, _ := h.credential(t, r.ID); id != "" {
		t.Fatal("a run credential was stored")
	}
}

// SM-GW-003/005 (simulated: hpfake and a scripted node): a Claude Code run in a space that routes it through Harbor
// Pilot gets exactly its child token on the wire (no account secret, though the space has accounts bound) and a
// central-chosen fresh session; a re-dispatch after a reconnect mints nothing new and sends the same token; the node's
// report of the end queues the revocation, which the revoker does on the deadline timer. A launch that names an account
// is refused, and a mint Harbor Pilot refuses fails the run as gateway_unavailable with a readable reason.
func TestHarborPilotRunTokenLifecycle(t *testing.T) {
	t.Parallel()
	f := hpfake.New(t)
	h := newHPRunEnv(t, f.URL(), f.Parent, f.Service, "prj_web")
	ctx := context.Background()
	mints := func() int { n := 0; f.Set(func(x *hpfake.HP) { n = len(x.Bodies) }); return n }
	base := mints() // the connection and project probes

	r, err := h.e.c.Fleet.LaunchRun(ctx, admin, h.sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: "claude-code", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Launch.Gateway == nil || r.Launch.Gateway.ProjectID != "prj_web" || r.Launch.AccountRef != "" || !strings.Contains(string(r.Manifest), `"gateway":{"kind":"harbor-pilot","project_id":"prj_web"}`) {
		t.Fatalf("launch spec %+v manifest %s", r.Launch, r.Manifest)
	}
	_, p := h.launchCmd(t, r.ID)
	tok := p.Secrets[proto.SecretGatewayToken]
	if len(p.Secrets) != 1 || !strings.HasPrefix(tok, "hpk_child_") || p.RunToken == "" {
		t.Fatalf("secrets %v", keys(p.Secrets))
	}
	if !reUUID.MatchString(p.SessionID) || p.Resume {
		t.Fatalf("session %q resume %v", p.SessionID, p.Resume)
	}
	// The run-facing address travels with the token (insecure HTTP is allowed on this loopback test connection).
	if p.Gateway == nil || p.Gateway.Kind != proto.GatewayHarborPilot || p.Gateway.BaseURL != f.URL() || !p.Gateway.AllowInsecureHTTP {
		t.Fatalf("gateway %+v", p.Gateway)
	}
	// The stored command holds neither the token nor any other secret.
	var stored string
	if err := h.e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT payload_json FROM node_commands WHERE run_id = ? AND kind = 'run.launch'`, r.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, tok) || strings.Contains(stored, "stub-claude-token") {
		t.Fatal("the stored launch command holds a secret")
	}

	// The node reconnects before acknowledging: the launch is sent again with the same token, minted once.
	h.dial(t)
	_, again := h.launchCmd(t, r.ID)
	if again.Secrets[proto.SecretGatewayToken] != tok || again.SessionID != p.SessionID {
		t.Fatal("the re-dispatch carries another token or session")
	}
	if n := mints() - base; n != 1 {
		t.Fatalf("minted %d times, want once", n)
	}

	// The run ends: its token is revoked through the durable queue.
	h.report(t, r, proto.RunRunning, "")
	h.report(t, r, proto.RunExited, "exited")
	eventually(t, "revocation queued", func() bool { _, live := h.credential(t, r.ID); return !live })
	cred, _ := h.credential(t, r.ID)
	// The revoker's round (central's deadline timer, which this harness does not run).
	if n, err := h.e.c.HarborPilotRuns.RevokeDue(ctx); err != nil || n != 1 {
		t.Fatalf("revoker round %d %v", n, err)
	}
	revoked := false
	f.Set(func(x *hpfake.HP) { revoked = x.Minted[cred] })
	if !revoked {
		t.Fatal("the token was not revoked")
	}
	if _, live := h.credential(t, r.ID); live {
		t.Fatal("the sealed token is kept after the run ended")
	}

	// Naming an account for a routed harness is refused: no fallback, not even on request.
	_, err = h.e.c.Fleet.LaunchRun(ctx, admin, h.sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: "claude-code", AccountRef: "stub-claude-" + h.sp.ID[len(h.sp.ID)-6:]})
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.CodePrecondition || !strings.Contains(ae.Message, "can't name an account") {
		t.Fatalf("account named: %v", err)
	}

	// Harbor Pilot refuses the mint: the run fails as gateway_unavailable, and its launch never reaches the node.
	f.Set(func(x *hpfake.HP) { x.ParentScopes = []string{harborpilot.ScopeInvoke, harborpilot.ScopeModels} })
	r2, err := h.e.c.Fleet.LaunchRun(ctx, admin, h.sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: "codex", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "failed as gateway_unavailable", func() bool {
		x := h.e.run1(r2.ID)
		return x.Observed == "failed" && x.EndReason == "gateway_unavailable"
	})
	if x := h.e.run1(r2.ID); !strings.Contains(x.Error, "Harbor Pilot refused to mint the run's token: parent token lacks tokens:mint") {
		t.Fatalf("reason %q", x.Error)
	}
	var state string
	if err := h.e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT state FROM node_commands WHERE run_id = ? AND kind = 'run.launch'`, r2.ID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("launch command %q %v", state, err)
	}
}

// SM-GW-005 (simulated): a revocation queued before central stopped is done by the next central (the queue is durable,
// and the deadline timer re-arms from the database).
func TestHarborPilotRevocationAfterRestart(t *testing.T) {
	// Not parallel: restarts central, which must finish within the node lease.
	f := hpfake.New(t)
	h := newHPRunEnv(t, f.URL(), f.Parent, f.Service, "prj_web")
	ctx := context.Background()
	r, err := h.e.c.Fleet.LaunchRun(ctx, admin, h.sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: "codex", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	h.launchCmd(t, r.ID)
	cred, _ := h.credential(t, r.ID)
	// Harbor Pilot is down when the run ends: the revocation stays queued.
	f.Set(func(x *hpfake.HP) { x.FailRevoke = true })
	h.report(t, r, proto.RunExited, "exited")
	eventually(t, "revocation queued", func() bool { _, live := h.credential(t, r.ID); return !live })
	if n, err := h.e.c.HarborPilotRuns.RevokeDue(ctx); err != nil || n != 0 {
		t.Fatalf("failing round %d %v", n, err)
	}
	f.Set(func(x *hpfake.HP) { x.FailRevoke = false })
	h.e.restartCentral()
	// The restarted central knows the revocation only from the database; it is due a second after the failed attempt.
	eventuallyFor(t, "revoked after restart", 20*time.Second, func() bool {
		if _, err := h.e.c.HarborPilotRuns.RevokeDue(ctx); err != nil {
			t.Fatal(err)
		}
		revoked := false
		f.Set(func(x *hpfake.HP) { revoked = x.Minted[cred] })
		return revoked
	})
	var state string
	if err := h.e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT state FROM gateway_revocations WHERE credential_id = ?`, cred).Scan(&state); err != nil || state != "revoked" {
		t.Fatalf("state %q %v", state, err)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// SM-GW-003/005/010 (real: `harbor-pilot serve --demo` v0.1.3, opt-in SPACEMAN_HARBOR_PILOT=1, make test-harbor-pilot;
// the node is scripted). Checked against the real gateway:
//   - the token on the wire works: GET /v1/models answers 200 with it;
//   - Harbor Pilot recorded the bound refs SpaceMan sent (space, run, instance, session, project) on the run's credential;
//   - after the run's reported end and a revoker round, the token gets 401;
//   - after the parent is revoked in Harbor Pilot, the next launch fails as gateway_unavailable with a readable reason.
func TestRealHarborPilotRunToken(t *testing.T) {
	t.Parallel()
	g := hptest.Start(t)
	h := newHPRunEnv(t, g.URL, g.ParentToken, g.ServiceToken, g.ProjectID)
	ctx := context.Background()
	models := func(tok string) int {
		t.Helper()
		code, _, err := g.Do(ctx, http.MethodGet, "/v1/models", tok, nil)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}

	r, err := h.e.c.Fleet.LaunchRun(ctx, admin, h.sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: "claude-code", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	_, p := h.launchCmd(t, r.ID)
	tok := p.Secrets[proto.SecretGatewayToken]
	if len(p.Secrets) != 1 || !strings.HasPrefix(tok, "hpk_") {
		t.Fatalf("secrets %v", keys(p.Secrets))
	}
	if code := models(tok); code != http.StatusOK {
		t.Fatalf("/v1/models with the run token: %d", code)
	}
	credID, _ := h.credential(t, r.ID)
	cred, err := g.CredentialInfo(ctx, credID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{harborpilot.RefSpace: h.sp.ID, harborpilot.RefRun: r.ID, harborpilot.RefInstance: r.InstanceID,
		harborpilot.RefSession: p.SessionID, harborpilot.RefProject: g.ProjectID}
	for k, v := range want {
		if cred.BoundRefs[k] != v {
			t.Fatalf("bound ref %s = %q, want %q (all: %v)", k, cred.BoundRefs[k], v, cred.BoundRefs)
		}
	}
	if cred.ParentID != g.ParentID || cred.BindingID != g.BindingID || len(cred.Scopes) != 2 {
		t.Fatalf("credential %+v", cred)
	}

	h.report(t, r, proto.RunRunning, "")
	h.report(t, r, proto.RunExited, "exited")
	eventually(t, "revocation queued", func() bool { _, live := h.credential(t, r.ID); return !live })
	if n, err := h.e.c.HarborPilotRuns.RevokeDue(ctx); err != nil || n != 1 {
		t.Fatalf("revoker round %d %v", n, err)
	}
	if code := models(tok); code != http.StatusUnauthorized {
		t.Fatalf("/v1/models after the run ended: %d, want 401", code)
	}

	if err := g.RevokeParent(ctx); err != nil {
		t.Fatal(err)
	}
	r2, err := h.e.c.Fleet.LaunchRun(ctx, admin, h.sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: "codex", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "failed as gateway_unavailable", func() bool {
		x := h.e.run1(r2.ID)
		return x.Observed == "failed" && x.EndReason == "gateway_unavailable"
	})
	if x := h.e.run1(r2.ID); !strings.Contains(x.Error, "Harbor Pilot refused to mint the run's token: the token is invalid, expired or revoked") {
		t.Fatalf("reason %q", x.Error)
	}
}
