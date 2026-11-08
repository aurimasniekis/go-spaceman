package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// fakeBackend offers three tools, one per class, while its token is usable.
type fakeBackend struct {
	f     *fx
	calls []string
}

var fakeClasses = map[string]string{"list_things": "read", "create_thing": "write", "delete_thing": "dangerous"}

func (b *fakeBackend) Validate(s SpacemanSettings) error {
	if slices.Contains(s.Tools, "respond_human_action") {
		return errors.New(`tool "respond_human_action" is never offered`)
	}
	return nil
}

func (b *fakeBackend) usable(ctx context.Context, t SpacemanTarget) bool {
	r, err := b.f.s.R().GetAPIToken(ctx, t.TokenID)
	return err == nil && !r.RevokedAt.Valid
}

func (b *fakeBackend) Catalog(ctx context.Context, t SpacemanTarget) []proto.AgentTool {
	if !b.usable(ctx, t) {
		return nil
	}
	var out []proto.AgentTool
	for name, class := range fakeClasses {
		out = append(out, proto.AgentTool{Name: t.Slug + Separator + name, Description: class, ReadOnly: class == "read"})
	}
	return out
}

func (b *fakeBackend) Class(_ SpacemanTarget, tool string, args json.RawMessage) (string, bool) {
	c, ok := fakeClasses[tool]
	if tool == "create_thing" && strings.Contains(string(args), "diagnostic") {
		c = "dangerous"
	}
	return c, ok
}

func (b *fakeBackend) Call(ctx context.Context, t SpacemanTarget, runID, tool string, _ json.RawMessage) (json.RawMessage, error) {
	if !b.usable(ctx, t) {
		return nil, apperr.New(apperr.CodeForbidden, "the SpaceMan MCP server %q can no longer act: its token was revoked", t.Slug)
	}
	b.calls = append(b.calls, runID+":"+tool)
	return json.RawMessage(`{"content":[]}`), nil
}

// fakeIssuer stores internal tokens as plain rows.
type fakeIssuer struct {
	minted  []auth.InternalTokenInput
	revoked []string
}

func (i *fakeIssuer) CreateInternalTokenTx(ctx context.Context, tx *store.Tx, p *auth.Principal, in auth.InternalTokenInput) (string, error) {
	if p == nil || p.User == nil {
		return "", apperr.New(apperr.CodeForbidden, "only users can issue API tokens")
	}
	id := ids.New(ids.APIToken)
	exp := int64(1 << 60)
	if !in.NeverExpires {
		exp = tx.Now.Add(in.TTL).UnixMicro()
	}
	if err := tx.Q.InsertAPIToken(ctx, db.InsertAPITokenParams{ID: id, Name: in.Name, UserID: p.User.ID, Mac: []byte(id), CreatedAt: tx.NowMicros(),
		ExpiresAt: exp, NeverExpires: b2i(in.NeverExpires), Purpose: in.Purpose}); err != nil {
		return "", err
	}
	for _, g := range in.Grants {
		if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES (?, ?, ?, 'token', 0, 't')`,
			auth.TokenSubject(id), string(g.Role), g.Domain); err != nil {
			return "", err
		}
	}
	i.minted = append(i.minted, in)
	return id, nil
}

func (i *fakeIssuer) RevokeAPITokenTx(ctx context.Context, tx *store.Tx, actor audit.Actor, id string) error {
	n, err := tx.Q.RevokeAPIToken(ctx, db.RevokeAPITokenParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, Actor: sql.NullString{String: actor.ID, Valid: true}, ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.New(apperr.CodeNotFound, "API token not found")
	}
	i.revoked = append(i.revoked, id)
	return nil
}

// fakeGate records the operations it gates; Require always asks for an approval.
type fakeGate struct{ gated, required []string }

func (g *fakeGate) Gate(_ context.Context, _ *fleet.RunPrincipal, op, _ string, _ json.RawMessage) (func(error), error) {
	g.gated = append(g.gated, op)
	return nil, nil
}

func (g *fakeGate) Require(_ context.Context, _ *fleet.RunPrincipal, op, _ string, _ json.RawMessage) (func(error), error) {
	g.required = append(g.required, op)
	return nil, apperr.New(apperr.CodePrecondition, "%s needs a person's approval", op)
}

// The spaceman kind in the gateway (ADR-0063): created with its token, granted like any server, offered to control and
// worker runs only, gated by class, refused on its own management, re-issued and deleted with its token revoked.
func TestSpacemanServers(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	must(t, f.s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `INSERT INTO users (id, display_name, enabled, provisioned_via, created_at, created_by, updated_at) VALUES ('usr_admin', 'A', 1, 'admin', 0, 't', 0)`)
		return err
	}))
	p := &auth.Principal{User: &auth.User{ID: "usr_admin"}}
	internal := audit.Actor{Type: audit.ActorUser, ID: "usr_admin", Via: auth.ViaSpacemanMCP + "atk_x", Run: "run_c"}
	endpoint := audit.Actor{Type: audit.ActorUser, ID: "usr_admin", Via: "api_token:atk_e", Channel: audit.ChannelMCPEndpoint}

	// Without a backend there is no spaceman kind.
	_, err := f.svc.CreateSpaceman(f.ctx, p, admin, SpacemanInput{Slug: "sm", Name: "SM"})
	code(t, err, apperr.CodeUnavailable)
	_, err = f.svc.UpdateSpaceman(f.ctx, admin, "mcp_x", 1, SpacemanUpdate{Name: "x"})
	code(t, err, apperr.CodeUnavailable)

	be, iss, gate := &fakeBackend{f: f}, &fakeIssuer{}, &fakeGate{}
	f.svc.Spaceman, f.svc.Tokens, f.svc.Gate = be, iss, gate

	for name, in := range map[string]SpacemanInput{
		"slug spaceman":     {Slug: "spaceman", Name: "S"},
		"no name":           {Slug: "sm"},
		"approval":          {Slug: "sm", Name: "S", SpacemanSettings: SpacemanSettings{Approval: "always"}},
		"excluded tool":     {Slug: "sm", Name: "S", SpacemanSettings: SpacemanSettings{Tools: []string{"respond_human_action"}}},
		"read needs grants": {Slug: "sm", Name: "S", SpacemanSettings: SpacemanSettings{Preset: "read"}},
		"expiry":            {Slug: "sm", Name: "S", SpacemanToken: SpacemanToken{ExpiresInDays: 400}},
	} {
		if _, err := f.svc.CreateSpaceman(f.ctx, p, admin, in); apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, a := range []audit.Actor{internal, endpoint} {
		_, err := f.svc.CreateSpaceman(f.ctx, p, a, SpacemanInput{Slug: "sm", Name: "S"})
		code(t, err, apperr.CodeForbidden)
	}
	_, err = f.svc.CreateSpaceman(f.ctx, &auth.Principal{User: p.User, TokenID: "atk_e", MCPEndpoint: true}, admin, SpacemanInput{Slug: "sm", Name: "S"})
	code(t, err, apperr.CodeForbidden)
	if len(iss.minted) != 0 {
		t.Fatalf("refused creates minted %d tokens", len(iss.minted))
	}

	srv, err := f.svc.CreateSpaceman(f.ctx, p, admin, SpacemanInput{Slug: "sm", Name: "SpaceMan",
		SpacemanSettings: SpacemanSettings{Tools: []string{"b", "a", "a"}}})
	must(t, err)
	in := iss.minted[0]
	if srv.Kind != KindSpaceman || srv.URL != "" || srv.Spaceman.Preset != "admin" || srv.Spaceman.Approval != ApprovalNone ||
		!slices.Equal(srv.Spaceman.Tools, []string{"a", "b"}) || !srv.Spaceman.NeverExpires || srv.Spaceman.TokenState != "active" ||
		srv.Spaceman.OwnerUserID != "usr_admin" || !in.NeverExpires || in.Purpose != auth.TokenPurposeSpacemanMCP ||
		len(in.Grants) != 1 || in.Grants[0].Role != authz.RoleAdmin || in.Grants[0].Domain != authz.Global {
		t.Fatalf("created %+v / %+v / minted %+v", srv, srv.Spaceman, in)
	}
	_, err = f.svc.CreateSpaceman(f.ctx, p, admin, SpacemanInput{Slug: "sm", Name: "Again"})
	code(t, err, apperr.CodeAlreadyExists)
	l, err := f.svc.List(f.ctx)
	if err != nil || len(l) != 1 || l[0].Spaceman == nil {
		t.Fatalf("list %+v %v", l, err)
	}
	_, err = f.svc.AddGrant(f.ctx, admin, srv.ID, "spc_a", "*", nil)
	must(t, err)

	run := func(kind string) *fleet.RunPrincipal {
		return &fleet.RunPrincipal{RunID: "run_" + kind, SpaceID: "spc_a", Kind: kind, Role: kind}
	}
	for _, kind := range []string{"control", "worker"} {
		if c := f.svc.Catalog(f.ctx, run(kind)); len(c) != 3 || !c[0].Upstream {
			t.Errorf("%s catalog: %+v", kind, c)
		}
	}
	for _, kind := range []string{"kb", "dreamer", "diagnostic"} {
		if c := f.svc.Catalog(f.ctx, run(kind)); len(c) != 0 {
			t.Errorf("%s catalog: %+v", kind, c)
		}
		_, err := f.svc.Call(f.ctx, run(kind), "sm__list_things", nil)
		code(t, err, apperr.CodeForbidden)
	}
	m, err := f.svc.ForManifest(f.ctx, "spc_a", "control", "control")
	if err != nil || len(m) != 1 || m[0].Kind != KindSpaceman {
		t.Fatalf("manifest %+v %v", m, err)
	}

	// Class-named operations: reads and writes go through the policies; dangerous tools (and a diagnostic create here)
	// always need an approval.
	ctrl := run("control")
	_, err = f.svc.Call(f.ctx, ctrl, "sm__list_things", nil)
	must(t, err)
	_, err = f.svc.Call(f.ctx, ctrl, "sm__create_thing", json.RawMessage(`{}`))
	must(t, err)
	_, err = f.svc.Call(f.ctx, ctrl, "sm__delete_thing", nil)
	code(t, err, apperr.CodePrecondition)
	_, err = f.svc.Call(f.ctx, ctrl, "sm__create_thing", json.RawMessage(`{"kind":"diagnostic"}`))
	code(t, err, apperr.CodePrecondition)
	_, err = f.svc.Call(f.ctx, ctrl, "sm__unknown_thing", nil)
	code(t, err, apperr.CodeNotFound)
	if !slices.Equal(gate.gated, []string{"mcp.sm.read.list_things", "mcp.sm.write.create_thing"}) ||
		!slices.Equal(gate.required, []string{"mcp.sm.dangerous.delete_thing", "mcp.sm.dangerous.create_thing"}) ||
		!slices.Equal(be.calls, []string{"run_control:list_things", "run_control:create_thing"}) {
		t.Fatalf("gated %v, required %v, calls %v", gate.gated, gate.required, be.calls)
	}
	// Without an approval gate, what always needs an approval is refused (fail closed); the rest runs.
	f.svc.Gate = nil
	_, err = f.svc.Call(f.ctx, ctrl, "sm__delete_thing", nil)
	code(t, err, apperr.CodePrecondition)
	_, err = f.svc.Call(f.ctx, ctrl, "sm__list_things", nil)
	must(t, err)
	f.svc.Gate = gate

	// The generic edit refuses a spaceman server, and the spaceman edit refuses an http server.
	_, err = f.svc.Update(f.ctx, admin, srv.ID, srv.Version, Input{Name: "x", URL: "https://x.example.com", Enabled: true})
	code(t, err, apperr.CodeInvalid)
	h, err := f.svc.Create(f.ctx, admin, Input{Slug: "tickets", Name: "T", URL: "https://mcp.example.com", Enabled: true})
	must(t, err)
	_, err = f.svc.UpdateSpaceman(f.ctx, admin, h.ID, h.Version, SpacemanUpdate{Name: "x", Enabled: true})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.UpdateSpaceman(f.ctx, admin, "mcp_missing", 1, SpacemanUpdate{Name: "x", Enabled: true})
	code(t, err, apperr.CodeNotFound)
	_, err = f.svc.UpdateSpaceman(f.ctx, admin, srv.ID, srv.Version+5, SpacemanUpdate{Name: "x", Enabled: true})
	code(t, err, apperr.CodePrecondition)

	// approval: writes gates the write tools too.
	srv, err = f.svc.UpdateSpaceman(f.ctx, admin, srv.ID, srv.Version, SpacemanUpdate{Name: "SpaceMan", Enabled: true,
		SpacemanSettings: SpacemanSettings{Preset: "operate", Approval: ApprovalWrites}})
	must(t, err)
	if srv.Spaceman.Preset != "operate" || srv.Spaceman.Approval != ApprovalWrites || len(iss.minted) != 1 {
		t.Fatalf("updated %+v", srv.Spaceman)
	}
	_, err = f.svc.Call(f.ctx, ctrl, "sm__create_thing", json.RawMessage(`{}`))
	code(t, err, apperr.CodePrecondition)
	if gate.required[len(gate.required)-1] != "mcp.sm.write.create_thing" {
		t.Fatalf("required %v", gate.required)
	}

	// Self-protection: an agent acting through a SpaceMan MCP server never manages one. An http server is unaffected.
	// Nor does a client on the external MCP endpoint (defence in depth: these tools are not in its catalog).
	grants, _ := f.svc.Grants(f.ctx, srv.ID)
	for _, a := range []audit.Actor{internal, endpoint} {
		_, err = f.svc.AddGrant(f.ctx, a, srv.ID, "spc_b", "*", nil)
		code(t, err, apperr.CodeForbidden)
		code(t, f.svc.RevokeGrant(f.ctx, a, srv.ID, grants[0].ID), apperr.CodeForbidden)
		code(t, f.svc.Delete(f.ctx, a, srv.ID), apperr.CodeForbidden)
		_, err = f.svc.Check(f.ctx, a, srv.ID)
		code(t, err, apperr.CodeForbidden)
		_, err = f.svc.UpdateSpaceman(f.ctx, a, srv.ID, srv.Version, SpacemanUpdate{Name: "x", Enabled: true})
		code(t, err, apperr.CodeForbidden)
		_, err = f.svc.ReissueSpaceman(f.ctx, p, a, srv.ID, srv.Version, SpacemanToken{})
		code(t, err, apperr.CodeForbidden)
	}
	_, err = f.svc.ReissueSpaceman(f.ctx, &auth.Principal{User: p.User, TokenID: "atk_e", MCPEndpoint: true}, admin, srv.ID, srv.Version, SpacemanToken{})
	code(t, err, apperr.CodeForbidden)
	_, err = f.svc.AddGrant(f.ctx, internal, h.ID, "spc_b", "*", nil)
	must(t, err)
	d, err := f.svc.Check(f.ctx, admin, srv.ID)
	if err != nil || !d.OK || len(d.Tools) != 3 {
		t.Fatalf("check %+v %v", d, err)
	}

	// Re-issue: the old token is revoked and a new one minted, with the given grants and expiry.
	old := srv.Spaceman.TokenID
	_, err = f.svc.ReissueSpaceman(f.ctx, p, admin, srv.ID, srv.Version, SpacemanToken{ExpiresInDays: 0})
	code(t, err, apperr.CodeInvalid) // the operate preset needs grants
	_, err = f.svc.ReissueSpaceman(f.ctx, p, admin, srv.ID, srv.Version+3, SpacemanToken{})
	code(t, err, apperr.CodePrecondition)
	srv, err = f.svc.ReissueSpaceman(f.ctx, p, admin, srv.ID, srv.Version, SpacemanToken{
		Grants: []auth.GrantInput{{Role: authz.RoleOperator, Domain: "spc_a"}}, ExpiresInDays: 30})
	must(t, err)
	if srv.Spaceman.TokenID == old || !slices.Equal(iss.revoked, []string{old}) || srv.Spaceman.NeverExpires || srv.Spaceman.ExpiresAt == nil ||
		len(srv.Spaceman.Grants) != 1 || srv.Spaceman.Grants[0].Domain != "spc_a" {
		t.Fatalf("reissued %+v (revoked %v)", srv.Spaceman, iss.revoked)
	}

	// A revoked token: the catalog empties, calls are refused with the reason, the state says so.
	must(t, f.s.W().TxNamed(f.ctx, "revoke", func(tx *store.Tx) error { return iss.RevokeAPITokenTx(f.ctx, tx, admin, srv.Spaceman.TokenID) }))
	if c := f.svc.Catalog(f.ctx, ctrl); len(c) != 0 {
		t.Fatalf("revoked catalog %+v", c)
	}
	_, err = f.svc.Call(f.ctx, ctrl, "sm__list_things", nil)
	if apperr.CodeOf(err) != apperr.CodeForbidden || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("call with a revoked token: %v", err)
	}
	s, _ := f.svc.Get(f.ctx, srv.ID)
	if s.Spaceman.TokenState != "revoked" {
		t.Fatalf("state %q", s.Spaceman.TokenState)
	}
	if d, err := f.svc.Check(f.ctx, admin, srv.ID); err != nil || d.OK || d.Error == "" {
		t.Fatalf("check %+v %v", d, err)
	}
	// Expired and disabled-owner states.
	srv, err = f.svc.ReissueSpaceman(f.ctx, p, admin, srv.ID, s.Version, SpacemanToken{Grants: []auth.GrantInput{{Role: authz.RoleOperator, Domain: "spc_a"}}})
	must(t, err)
	must(t, f.s.W().TxNamed(f.ctx, "disable", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `UPDATE users SET disabled_at = 1 WHERE id = 'usr_admin'`)
		return err
	}))
	if s, _ := f.svc.Get(f.ctx, srv.ID); s.Spaceman.TokenState != "owner_disabled" {
		t.Fatalf("state %q", s.Spaceman.TokenState)
	}
	must(t, f.s.W().TxNamed(f.ctx, "expire", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `UPDATE api_tokens SET expires_at = 1 WHERE id = ?`, srv.Spaceman.TokenID)
		return err
	}))
	if s, _ := f.svc.Get(f.ctx, srv.ID); s.Spaceman.TokenState != "expired" {
		t.Fatalf("state %q", s.Spaceman.TokenState)
	}

	// Deleting revokes the token in the same step; deleting an http server touches no token.
	n := len(iss.revoked)
	must(t, f.svc.Delete(f.ctx, admin, srv.ID))
	must(t, f.svc.Delete(f.ctx, admin, h.ID))
	if len(iss.revoked) != n+1 || iss.revoked[n] != srv.Spaceman.TokenID {
		t.Fatalf("revoked %v", iss.revoked)
	}
}
