package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

type fx struct {
	t   *testing.T
	ctx context.Context
	s   *store.Store
	svc *Service
}

func newFx(t *testing.T) *fx {
	t.Helper()
	s := storetest.Open(t, storetest.WithClock(clock.NewFake(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))))
	kr := keyringtest.Load(t, s)
	f := &fx{t: t, ctx: context.Background(), s: s, svc: New(s, kr, audit.NewWriter(kr), changefeed.New(nil), Options{})}
	t.Cleanup(f.svc.Close)
	must(t, s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(f.ctx, q); err != nil {
				return err
			}
		}
		return nil
	}))
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func code(t *testing.T, err error, want apperr.Code) {
	t.Helper()
	if apperr.CodeOf(err) != want {
		t.Fatalf("got %v (%s), want %s", err, apperr.CodeOf(err), want)
	}
}

// upstream is an in-process MCP server with a few tools. It records the Authorization header it receives.
type upstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	auth []string
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-upstream", Version: "1.0"}, nil)
	type echoIn struct {
		Text string `json:"text"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "Echo text.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, req *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
			u.mu.Lock()
			u.auth = append(u.auth, req.Extra.Header.Get("Authorization"))
			u.mu.Unlock()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + in.Text}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "session", Description: "The upstream session ID."},
		func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: req.Session.ID()}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "slow", Description: "Never answers in time."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
			return &mcp.CallToolResult{}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "big", Description: "A large result."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", 8000)}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "fails", Description: "A tool error."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return nil, nil, errors.New("the upstream refused")
		})
	mcp.AddTool(s, &mcp.Tool{Name: "a_tool_name_that_is_far_too_long_to_offer_once_it_has_been_namespaced", Description: "Too long."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	u.srv = httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	t.Cleanup(u.srv.Close)
	return u
}

func (f *fx) server(in Input) *Server {
	f.t.Helper()
	if in.Name == "" {
		in.Name = "Upstream"
	}
	in.Enabled = true
	s, err := f.svc.Create(f.ctx, admin, in)
	must(f.t, err)
	return s
}

func principal(run, space, role string) *fleet.RunPrincipal {
	return &fleet.RunPrincipal{RunID: run, SpaceID: space, Kind: "worker", Role: role}
}

func TestServerConfig(t *testing.T) {
	f := newFx(t)
	bad := []Input{
		{Slug: "Up", Name: "x", URL: "https://example.com/mcp"},
		{Slug: "spaceman", Name: "x", URL: "https://example.com/mcp"},
		{Slug: "up", Name: "", URL: "https://example.com/mcp"},
		{Slug: "up", Name: "x", URL: "http://example.com/mcp"},
		{Slug: "up", Name: "x", URL: "https://user:pw@example.com/mcp"},
		{Slug: "up", Name: "x", URL: "ftp://example.com/mcp"},
		{Slug: "up", Name: "x", URL: "/relative"},
		{Slug: "up", Name: "x", URL: "https://example.com/mcp#frag"},
		{Slug: "up", Name: "x", URL: "https://example.com/mcp", Headers: map[string]string{"Host": "evil"}},
		{Slug: "up", Name: "x", URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "a\r\nb"}},
		{Slug: "up", Name: "x", URL: "https://example.com/mcp", Tools: []string{"has space"}},
		{Slug: "up", Name: "x", URL: "https://example.com/mcp", CallTimeoutMS: 60000},
		{Slug: "up", Name: "x", URL: "https://example.com/mcp", MaxConcurrency: 100},
		{Slug: "up", Name: "x", URL: "https://example.com/mcp", MaxResultBytes: 10},
	}
	for i, in := range bad {
		_, err := f.svc.Create(f.ctx, admin, in)
		if apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Errorf("case %d: got %v, want a validation error", i, err)
		}
	}
	s := f.server(Input{Slug: "up", URL: "https://example.com/mcp", Headers: map[string]string{"authorization": "Bearer s3cret", "X-Team": "a"}})
	if strings.Join(s.HeaderNames, ",") != "Authorization,X-Team" || s.CallTimeoutMS != 20000 || s.MaxConcurrency != 4 || s.MaxResultBytes != 256<<10 {
		t.Fatalf("server: %+v", s)
	}
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), "s3cret") {
		t.Fatal("a header value was returned")
	}
	row, err := f.s.R().GetMCPServer(f.ctx, s.ID)
	must(t, err)
	if strings.Contains(string(row.HeadersEnc), "s3cret") {
		t.Fatal("headers are not sealed")
	}
	_, err = f.svc.Create(f.ctx, admin, Input{Slug: "up", Name: "again", URL: "https://example.com/mcp", Enabled: true})
	code(t, err, apperr.CodeAlreadyExists)

	// An edit without set_headers keeps the stored headers; with it, replaces them.
	s2, err := f.svc.Update(f.ctx, admin, s.ID, s.Version, Input{Name: "Renamed", URL: "https://example.com/mcp2", Enabled: true})
	must(t, err)
	if s2.Name != "Renamed" || len(s2.HeaderNames) != 2 {
		t.Fatalf("edit: %+v", s2)
	}
	_, err = f.svc.Update(f.ctx, admin, s.ID, s.Version, Input{Name: "x", URL: "https://example.com/mcp", Enabled: true})
	code(t, err, apperr.CodePrecondition)
	s3, err := f.svc.Update(f.ctx, admin, s.ID, s2.Version, Input{Name: "Renamed", URL: "https://example.com/mcp2", Enabled: true, SetHeaders: true})
	must(t, err)
	if len(s3.HeaderNames) != 0 || s3.HeadersVersion != 2 {
		t.Fatalf("headers not cleared: %+v", s3)
	}

	// Grants.
	_, err = f.svc.AddGrant(f.ctx, admin, s.ID, "spc_a", "Bad Role", nil)
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.AddGrant(f.ctx, admin, s.ID, "spc_missing", "*", nil)
	code(t, err, apperr.CodeNotFound)
	_, err = f.svc.AddGrant(f.ctx, admin, "mcp_missing", "spc_a", "*", nil)
	code(t, err, apperr.CodeNotFound)
	g, err := f.svc.AddGrant(f.ctx, admin, s.ID, "spc_a", "", nil)
	must(t, err)
	if g.Role != "*" {
		t.Fatalf("grant: %+v", g)
	}
	_, err = f.svc.AddGrant(f.ctx, admin, s.ID, "spc_a", "*", nil)
	code(t, err, apperr.CodeAlreadyExists)
	g2, err := f.svc.AddGrant(f.ctx, admin, s.ID, "spc_a", "reviewer", []string{"echo"})
	must(t, err)
	gl, err := f.svc.Grants(f.ctx, s.ID)
	must(t, err)
	sg, err := f.svc.SpaceGrants(f.ctx, "spc_a")
	must(t, err)
	if len(gl) != 2 || len(sg) != 2 || sg[0].Slug != "up" {
		t.Fatalf("grants: %d, space: %+v", len(gl), sg)
	}
	got, err := f.svc.Get(f.ctx, s.ID)
	must(t, err)
	if got.Grants != 2 {
		t.Fatalf("grant count %d", got.Grants)
	}
	// "*" grants everything, so a reviewer gets every tool.
	m, err := f.svc.ForManifest(f.ctx, "spc_a", "reviewer", "worker")
	must(t, err)
	if len(m) != 1 || m[0].Slug != "up" || len(m[0].Tools) != 0 {
		t.Fatalf("manifest: %+v", m)
	}
	code(t, f.svc.RevokeGrant(f.ctx, admin, "mcp_other", g.ID), apperr.CodeNotFound)
	must(t, f.svc.RevokeGrant(f.ctx, admin, s.ID, g.ID))
	m, err = f.svc.ForManifest(f.ctx, "spc_a", "reviewer", "worker")
	must(t, err)
	if len(m) != 1 || strings.Join(m[0].Tools, ",") != "echo" {
		t.Fatalf("manifest after revoke: %+v", m)
	}
	if m, _ := f.svc.ForManifest(f.ctx, "spc_a", "control", "control"); len(m) != 0 {
		t.Fatalf("control has no grant: %+v", m)
	}
	_ = g2
	l, err := f.svc.List(f.ctx)
	must(t, err)
	if len(l) != 1 {
		t.Fatalf("list: %d", len(l))
	}
	must(t, f.svc.Delete(f.ctx, admin, s.ID))
	code(t, f.svc.Delete(f.ctx, admin, s.ID), apperr.CodeNotFound)
	if gl, _ := f.svc.Grants(f.ctx, s.ID); len(gl) != 0 {
		t.Fatal("grants survived the server")
	}
	_, err = f.svc.Get(f.ctx, s.ID)
	code(t, err, apperr.CodeNotFound)
}

func text(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var r mcp.CallToolResult
	must(t, json.Unmarshal(raw, &r))
	if len(r.Content) == 0 {
		return ""
	}
	if r.IsError {
		return "error: " + r.Content[0].(*mcp.TextContent).Text
	}
	return r.Content[0].(*mcp.TextContent).Text
}

func TestGatewayCalls(t *testing.T) {
	f := newFx(t)
	u := newUpstream(t)
	s := f.server(Input{Slug: "up", URL: u.srv.URL, AllowPrivate: true, Headers: map[string]string{"Authorization": "Bearer s3cret"},
		CallTimeoutMS: 1000, MaxResultBytes: 4096})
	_, err := f.svc.AddGrant(f.ctx, admin, s.ID, "spc_a", "worker", nil)
	must(t, err)
	a, b := principal("run_a", "spc_a", "worker"), principal("run_b", "spc_a", "worker")

	// The catalog: namespaced, described with the server's name, read-only hints kept, too-long names left out.
	cat := f.svc.Catalog(f.ctx, a)
	names := map[string]bool{}
	for _, tl := range cat {
		names[tl.Name] = true
		if !tl.Upstream || !strings.HasPrefix(tl.Description, "[Upstream] ") {
			t.Fatalf("tool %+v", tl)
		}
		if tl.Name == "up__echo" && !tl.ReadOnly {
			t.Fatal("read-only hint lost")
		}
	}
	if !names["up__echo"] || !names["up__session"] || len(names) != 5 {
		t.Fatalf("catalog: %v", names)
	}

	out, err := f.svc.Call(f.ctx, a, "up__echo", json.RawMessage(`{"text":"hi"}`))
	must(t, err)
	if text(t, out) != "echo: hi" {
		t.Fatalf("echo: %s", out)
	}
	u.mu.Lock()
	if len(u.auth) != 1 || u.auth[0] != "Bearer s3cret" {
		t.Fatalf("upstream saw %v", u.auth)
	}
	u.mu.Unlock()

	// Each run has its own upstream session, kept across its calls.
	sa1, err := f.svc.Call(f.ctx, a, "up__session", nil)
	must(t, err)
	sa2, err := f.svc.Call(f.ctx, a, "up__session", nil)
	must(t, err)
	sb, err := f.svc.Call(f.ctx, b, "up__session", nil)
	must(t, err)
	if text(t, sa1) != text(t, sa2) || text(t, sa1) == text(t, sb) || text(t, sa1) == "" {
		t.Fatalf("sessions: a=%s,%s b=%s", text(t, sa1), text(t, sa2), text(t, sb))
	}
	if f.svc.Sessions() != 2 {
		t.Fatalf("sessions open: %d", f.svc.Sessions())
	}

	// Upstream tool errors reach the agent as results; limits are enforced.
	out, err = f.svc.Call(f.ctx, a, "up__fails", nil)
	must(t, err)
	if !strings.Contains(text(t, out), "the upstream refused") {
		t.Fatalf("fails: %s", out)
	}
	_, err = f.svc.Call(f.ctx, a, "up__big", nil)
	code(t, err, apperr.CodeInvalid)
	start := time.Now()
	_, err = f.svc.Call(f.ctx, a, "up__slow", nil)
	code(t, err, apperr.CodeUnavailable)
	if time.Since(start) > 5*time.Second {
		t.Fatal("the timeout was not applied")
	}
	_, err = f.svc.Call(f.ctx, a, "up__echo", json.RawMessage(`{"text":"`+strings.Repeat("x", maxArgsBytes)+`"}`))
	code(t, err, apperr.CodeInvalid)

	// Names and grants are checked on every call.
	for _, n := range []string{"up__nope", "Up__echo", "up", "other__echo"} {
		if _, err := f.svc.Call(f.ctx, a, n, nil); err == nil {
			t.Fatalf("%s was accepted", n)
		}
	}
	_, err = f.svc.Call(f.ctx, principal("run_c", "spc_a", "control"), "up__echo", nil)
	code(t, err, apperr.CodeForbidden)
	_, err = f.svc.Call(f.ctx, principal("run_d", "spc_b", "worker"), "up__echo", nil)
	code(t, err, apperr.CodeForbidden)
	if len(f.svc.Catalog(f.ctx, principal("run_d", "spc_b", "worker"))) != 0 {
		t.Fatal("another space sees the server")
	}

	// Narrowing the server's tool list applies to the next call (the session is reopened).
	cur, _ := f.svc.Get(f.ctx, s.ID)
	_, err = f.svc.Update(f.ctx, admin, s.ID, cur.Version, Input{Name: "Upstream", URL: u.srv.URL, AllowPrivate: true, Tools: []string{"session"}, Enabled: true})
	must(t, err)
	_, err = f.svc.Call(f.ctx, a, "up__echo", json.RawMessage(`{"text":"x"}`))
	code(t, err, apperr.CodeForbidden)
	sa3, err := f.svc.Call(f.ctx, a, "up__session", nil)
	must(t, err)
	if text(t, sa3) == text(t, sa1) {
		t.Fatal("the session was not reopened after the server changed")
	}

	// Ending a run closes its sessions; revoking the grant refuses the next call.
	f.svc.EndRun("run_b")
	if f.svc.Sessions() != 1 {
		t.Fatalf("sessions after EndRun: %d", f.svc.Sessions())
	}
	gl, _ := f.svc.Grants(f.ctx, s.ID)
	must(t, f.svc.RevokeGrant(f.ctx, admin, s.ID, gl[0].ID))
	_, err = f.svc.Call(f.ctx, a, "up__session", nil)
	code(t, err, apperr.CodeForbidden)
	if f.svc.Sessions() != 0 {
		t.Fatalf("sessions after revoke: %d", f.svc.Sessions())
	}

	// Every call is audited, without arguments.
	var n int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT COUNT(*) FROM audit_events WHERE action = 'mcp_server.tool_called'`).Scan(&n))
	if n < 8 {
		t.Fatalf("audited calls: %d", n)
	}
	var leaked int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT COUNT(*) FROM audit_events WHERE details_json LIKE '%hi%' AND action = 'mcp_server.tool_called'`).Scan(&leaked))
	if leaked != 0 {
		t.Fatal("arguments were audited")
	}

	// Check lists the tools with a throwaway session.
	d, err := f.svc.Check(f.ctx, admin, s.ID)
	must(t, err)
	if !d.OK || d.Server != "fake-upstream 1.0" || len(d.Tools) != 6 {
		t.Fatalf("check: %+v", d)
	}
	for _, tl := range d.Tools {
		if strings.HasPrefix(tl.Name, "a_tool") == tl.Usable {
			t.Fatalf("usable flag: %+v", tl)
		}
	}
	if f.svc.Sessions() != 0 {
		t.Fatal("check left a session open")
	}
	_, err = f.svc.Check(f.ctx, admin, "mcp_missing")
	code(t, err, apperr.CodeNotFound)
}

func TestDisabledAndUnreachable(t *testing.T) {
	f := newFx(t)
	u := newUpstream(t)
	off, err := f.svc.Create(f.ctx, admin, Input{Slug: "off", Name: "Off", URL: u.srv.URL, AllowPrivate: true, Enabled: false})
	must(t, err)
	gone := f.server(Input{Slug: "gone", URL: "http://127.0.0.1:1/mcp", AllowPrivate: true})
	for _, id := range []string{off.ID, gone.ID} {
		_, err := f.svc.AddGrant(f.ctx, admin, id, "spc_a", "*", nil)
		must(t, err)
	}
	p := principal("run_a", "spc_a", "worker")
	if c := f.svc.Catalog(f.ctx, p); len(c) != 0 {
		t.Fatalf("catalog: %+v", c)
	}
	_, err = f.svc.Call(f.ctx, p, "off__echo", nil)
	code(t, err, apperr.CodeForbidden)
	_, err = f.svc.Call(f.ctx, p, "gone__echo", nil)
	code(t, err, apperr.CodeUnavailable)
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("the address leaked to the agent: %v", err)
	}
}

func TestEgressPolicy(t *testing.T) {
	for addr, want := range map[string][2]bool{ // [allowed without private, allowed with private]
		"8.8.8.8":         {true, true},
		"2606:4700::1111": {true, true},
		"127.0.0.1":       {false, true},
		"10.1.2.3":        {false, true},
		"172.16.0.1":      {false, true},
		"192.168.1.1":     {false, true},
		"100.64.0.1":      {false, true},
		"::1":             {false, true},
		"fd00::1":         {false, true},
		"::ffff:10.0.0.1": {false, true},
		"64:ff9b::a00:1":  {false, true},
		"169.254.169.254": {false, false},
		"fe80::1":         {false, false},
		"0.0.0.0":         {false, false},
		"224.0.0.1":       {false, false},
		"::":              {false, false},
	} {
		ip := netip.MustParseAddr(addr)
		if allowedAddr(ip, false) != want[0] || allowedAddr(ip, true) != want[1] {
			t.Errorf("%s: got %v/%v, want %v", addr, allowedAddr(ip, false), allowedAddr(ip, true), want)
		}
	}
	if allowedAddr(netip.Addr{}, true) {
		t.Error("the zero address was allowed")
	}
	c := control(false)
	if err := c("tcp4", "127.0.0.1:443", nil); !errors.Is(err, ErrEgressDenied) {
		t.Errorf("loopback: %v", err)
	}
	if err := c("tcp4", "93.184.216.34:443", nil); err != nil {
		t.Errorf("public: %v", err)
	}
	if err := c("udp4", "93.184.216.34:53", nil); !errors.Is(err, ErrEgressDenied) {
		t.Errorf("udp: %v", err)
	}
	if err := c("tcp4", "not-an-address", nil); !errors.Is(err, ErrEgressDenied) {
		t.Errorf("garbage: %v", err)
	}

	// End to end: a name that resolves to loopback is refused at dial time when private networks are not allowed.
	f := newFx(t)
	u := newUpstream(t)
	port := u.srv.URL[strings.LastIndex(u.srv.URL, ":"):]
	for _, host := range []string{"https://127.0.0.1", "https://localhost"} {
		s := f.server(Input{Slug: "s" + strings.ReplaceAll(strings.TrimPrefix(host, "https://"), ".", ""), URL: host + port})
		d, err := f.svc.Check(f.ctx, admin, s.ID)
		must(t, err)
		if d.OK || !strings.Contains(d.Error, "egress policy") {
			t.Fatalf("%s: %+v", host, d)
		}
	}

	// Redirects are not followed.
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, u.srv.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redir.Close)
	s := f.server(Input{Slug: "redir", URL: redir.URL, AllowPrivate: true})
	d, err := f.svc.Check(f.ctx, admin, s.ID)
	must(t, err)
	if d.OK {
		t.Fatal("a redirect was followed")
	}
}

func TestLimitedBodyAndSlots(t *testing.T) {
	b := &limitedBody{rc: io.NopCloser(strings.NewReader("hello world")), left: 5}
	got, err := io.ReadAll(b)
	if string(got) != "hello" || !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %q, %v", got, err)
	}
	f := newFx(t)
	s := f.server(Input{Slug: "up", URL: "https://example.com/mcp", MaxConcurrency: 1})
	row, err := f.s.R().GetMCPServer(f.ctx, s.ID)
	must(t, err)
	release, err := f.svc.rt.slot(f.ctx, row)
	must(t, err)
	ctx, cancel := context.WithTimeout(f.ctx, 50*time.Millisecond)
	defer cancel()
	_, err = f.svc.rt.slot(ctx, row)
	code(t, err, apperr.CodeRateLimited)
	release()
	r2, err := f.svc.rt.slot(f.ctx, row)
	must(t, err)
	r2()
	if !Owns("up__echo") || Owns("spaceman_inbox") || Owns("spaceman__x") || Owns("plain") {
		t.Fatal("Owns")
	}
}
