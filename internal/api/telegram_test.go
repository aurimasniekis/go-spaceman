package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Unit tests of the Telegram channel's guards in the API (ADR-0083 §3.2–§3.4). Each test names the guard it pins; the
// mutation run in the pull request shows each guard is load-bearing.

const tgSpace = "spc_0000000000000000000000000t"

type grantRows []db.RoleGrant

func (g grantRows) ListRoleGrants(context.Context) ([]db.RoleGrant, error) { return g, nil }

// tgAuthz grants the user and the link operator in tgSpace.
func tgAuthz(t *testing.T) *authz.Engine {
	t.Helper()
	e, err := authz.New(context.Background(), grantRows{
		{Subject: "user:usr_tg", Role: string(authz.RoleOperator), Domain: tgSpace},
		{Subject: "telegram:tgl_1", Role: string(authz.RoleOperator), Domain: tgSpace},
		{Subject: "token:atk_tg", Role: string(authz.RoleOperator), Domain: tgSpace},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func tgPrincipal() *auth.Principal {
	return &auth.Principal{User: &auth.User{ID: "usr_tg"}, Method: auth.MethodTelegram, TelegramLinkID: "tgl_1"}
}

// fakeSessions resolves "good" (and "other", to another link) to Telegram principals.
type fakeSessions struct {
	p     map[string]*auth.Principal
	err   error
	calls int
}

func (f *fakeSessions) Authenticate(_ context.Context, tok string) (*auth.Principal, error) {
	f.calls++
	return f.Validate(context.Background(), tok)
}

func (f *fakeSessions) Validate(_ context.Context, tok string) (*auth.Principal, error) {
	if f.err != nil {
		return nil, f.err
	}
	if p, ok := f.p[tok]; ok {
		return p, nil
	}
	return nil, auth.ErrUnauthorized
}

// probe operations: one per class, plus a personal one, a self-service one, a step-up one, a public one and the
// exchange.
var tgProbeRules = map[string]Rule{
	"probe-both":     InSpace(authz.InboxWrite),
	"probe-chat":     InSpace(authz.InboxWrite),
	"probe-miniapp":  InSpace(authz.InboxWrite),
	"probe-excluded": InSpace(authz.InboxWrite),
	"probe-unclass":  InSpace(authz.InboxWrite),
	"probe-personal": {Session: true},
	"probe-useronly": {Session: true, UserOnly: true, Filtered: true},
	"probe-self":     {Session: true, UserOnly: true, TelegramSelf: true},
	"probe-selfrea":  {Session: true, UserOnly: true, TelegramSelf: true, Reauth: true},
	"probe-filtered": FilteredRead(),
	"probe-stepup":   {Perm: authz.InboxWrite, Space: true, Reauth: true, MFA: true},
	"probe-public":   public(),
	"probe-exchange": public(),
	"probe-cookie":   InSpace(authz.InboxRead),
}

func tgProbes(api huma.API, _ *Deps) {
	for id := range tgProbeRules {
		path := "/spaces/{space_id}/" + id
		method := http.MethodPost
		if id == "probe-cookie" {
			method = http.MethodGet
		}
		if r := tgProbeRules[id]; !r.Space {
			path = "/" + id
		}
		huma.Register(api, huma.Operation{OperationID: id, Method: method, Path: path, DefaultStatus: http.StatusNoContent},
			func(context.Context, *struct{}) (*struct {
				SetCookie string `header:"Set-Cookie"`
			}, error) {
				out := &struct {
					SetCookie string `header:"Set-Cookie"`
				}{}
				if id == "probe-cookie" {
					out.SetCookie = "x=1; Path=/"
				}
				return out, nil
			})
	}
}

func tgPolicy(s TelegramSessions) *TelegramPolicy {
	return &TelegramPolicy{Ops: map[string]TelegramClass{
		"probe-both": TelegramBoth, "probe-chat": TelegramChat, "probe-miniapp": TelegramMiniApp, "probe-excluded": TelegramExcluded,
		"probe-personal": TelegramBoth, "probe-useronly": TelegramBoth, "probe-self": TelegramBoth, "probe-selfrea": TelegramBoth,
		"probe-filtered": TelegramBoth, "probe-stepup": TelegramBoth, "probe-public": TelegramBoth, "probe-exchange": TelegramExchange,
		"probe-cookie": TelegramBoth,
	}, Watch: TelegramMiniApp, Sessions: s, MiniAppOrigin: func() string { return "https://tg.example.com" }}
}

func tgDeps(t *testing.T, s TelegramSessions) *Deps {
	t.Helper()
	return &Deps{Authz: tgAuthz(t), PublicURL: "https://central.example.com", Extra: []func(huma.API, *Deps){tgProbes}, ExtraRules: tgProbeRules,
		Telegram: tgPolicy(s)}
}

func tgPath(id string) string {
	if tgProbeRules[id].Space {
		return Prefix + "/spaces/" + tgSpace + "/" + id
	}
	return Prefix + "/" + id
}

func tgMethod(id string) string {
	if id == "probe-cookie" {
		return http.MethodGet
	}
	return http.MethodPost
}

// inProcess serves one probe through InProcessHandler as p.
func inProcess(h http.Handler, p *auth.Principal, id string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(tgMethod(id), tgPath(id), nil).WithContext(WithInProcessPrincipal(context.Background(), p)))
	return rec
}

// Guard: InProcessHandler admits (ViaMCP and a token) or (ViaTelegram, no token, no root token, not via MCP), with a
// user and never root (§3.2 change 1).
func TestInProcessHandlerTelegramGuard(t *testing.T) {
	t.Parallel()
	h := tgDeps(t, nil).InProcessHandler()
	if rec := inProcess(h, tgPrincipal(), "probe-both"); rec.Code != http.StatusNoContent {
		t.Fatalf("telegram principal: %d %s", rec.Code, rec.Body.String())
	}
	for name, mod := range map[string]func(p *auth.Principal){
		"with a token ID":      func(p *auth.Principal) { p.TokenID = "atk_tg" },
		"with a root token ID": func(p *auth.Principal) { p.RootTokenID = "rtk_1" },
		"marked MCP endpoint":  func(p *auth.Principal) { p.MCPEndpoint = true },
		"with an internal purpose": func(p *auth.Principal) {
			p.TokenPurpose = auth.TokenPurposeSpacemanMCP
		},
		"without a user": func(p *auth.Principal) { p.User = nil },
		"not telegram":   func(p *auth.Principal) { p.TelegramLinkID = "" },
	} {
		p := tgPrincipal()
		mod(p)
		rec := inProcess(h, p, "probe-both")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "only an MCP or Telegram principal") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// An MCP principal still needs its token ID.
	mcp := &auth.Principal{User: &auth.User{ID: "usr_tg"}, TokenID: "atk_tg", Method: auth.MethodToken, MCPEndpoint: true}
	if rec := inProcess(h, mcp, "probe-both"); rec.Code != http.StatusNoContent {
		t.Fatalf("MCP endpoint principal: %d %s", rec.Code, rec.Body.String())
	}
	mcp.TokenID = ""
	if rec := inProcess(h, mcp, "probe-both"); rec.Code != http.StatusForbidden {
		t.Fatalf("MCP principal without a token: %d", rec.Code)
	}
}

// Guard: the classification (§3.3). The bot reaches chat and both, the Mini App miniapp and both; excluded and
// unclassified are refused on both channels, and with no policy everything is. A Telegram principal off a Telegram
// channel reaches nothing.
func TestTelegramClassificationEnforced(t *testing.T) {
	t.Parallel()
	sessions := &fakeSessions{p: map[string]*auth.Principal{"good": tgPrincipal()}}
	d := tgDeps(t, sessions)
	chat, mini := d.InProcessHandler(), d.TelegramHandler()
	for id, want := range map[string][2]int{ // [chat, miniapp]
		"probe-both":     {204, 204},
		"probe-chat":     {204, 403},
		"probe-miniapp":  {403, 204},
		"probe-excluded": {403, 403},
		"probe-unclass":  {403, 403},
	} {
		if rec := inProcess(chat, tgPrincipal(), id); rec.Code != want[0] {
			t.Errorf("chat %s: %d %s", id, rec.Code, rec.Body.String())
		} else if want[0] == 403 && !strings.Contains(rec.Body.String(), "not available from Telegram") {
			t.Errorf("chat %s: %s", id, rec.Body.String())
		}
		rec := miniApp(mini, http.MethodPost, tgPath(id), "Telegram good")
		if rec.Code != want[1] {
			t.Errorf("miniapp %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	// No policy: nothing is classified.
	nd := tgDeps(t, sessions)
	nd.Telegram = &TelegramPolicy{Sessions: sessions, MiniAppOrigin: nd.Telegram.MiniAppOrigin}
	if rec := inProcess(nd.InProcessHandler(), tgPrincipal(), "probe-both"); rec.Code != 403 {
		t.Errorf("no ops table, chat: %d", rec.Code)
	}
	if rec := miniApp(nd.TelegramHandler(), http.MethodPost, tgPath("probe-both"), "Telegram good"); rec.Code != 403 {
		t.Errorf("no ops table, miniapp: %d", rec.Code)
	}
	nd.Telegram = nil
	if rec := inProcess(nd.InProcessHandler(), tgPrincipal(), "probe-both"); rec.Code != 403 {
		t.Errorf("nil policy, chat: %d", rec.Code)
	}
	// A Telegram principal served without a Telegram channel (not through InProcessHandler or TelegramHandler).
	mux := http.NewServeMux()
	Register(mux, d)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tgPath("probe-both"), nil).WithContext(
		context.WithValue(context.Background(), principalKey{}, tgPrincipal())))
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "not available from Telegram") {
		t.Errorf("off-channel telegram principal: %d %s", rec.Code, rec.Body.String())
	}
	// Other principals don't meet the classification at all.
	bearer := &auth.Principal{User: &auth.User{ID: "usr_tg"}, TokenID: "atk_tg", Method: auth.MethodToken}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tgPath("probe-excluded"), nil).WithContext(
		context.WithValue(context.Background(), principalKey{}, bearer)))
	if rec.Code != 204 {
		t.Errorf("bearer on an excluded op: %d %s", rec.Code, rec.Body.String())
	}
}

// Guard: the refusal class (§3.2 change 2). A Telegram principal is refused on step-up and on personal Session or
// UserOnly rules, except Telegram self-service rules, which never lift a step-up; filtered reads stay open.
func TestTelegramRefusalClass(t *testing.T) {
	t.Parallel()
	h := tgDeps(t, nil).InProcessHandler()
	for id, want := range map[string]int{
		"probe-personal": 403, "probe-useronly": 403, "probe-stepup": 403, "probe-selfrea": 403,
		"probe-self": 204, "probe-filtered": 204,
	} {
		rec := inProcess(h, tgPrincipal(), id)
		if rec.Code != want || (want == 403 && !strings.Contains(rec.Body.String(), "not available from Telegram")) {
			t.Errorf("%s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
}

// Guard: CSRF (§3.2 change 3). A Telegram mutation needs no CSRF token; a cookie session's still does.
func TestTelegramCSRFExemption(t *testing.T) {
	t.Parallel()
	d := tgDeps(t, nil)
	if rec := inProcess(d.InProcessHandler(), tgPrincipal(), "probe-both"); rec.Code != http.StatusNoContent {
		t.Fatalf("telegram mutation without CSRF: %d %s", rec.Code, rec.Body.String())
	}
	mux := http.NewServeMux()
	Register(mux, d)
	session := &auth.Principal{User: &auth.User{ID: "usr_tg"}, SessionID: "ses_1", Method: auth.MethodPassword}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tgPath("probe-excluded"), nil).WithContext(
		context.WithValue(context.Background(), principalKey{}, session)))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "CSRF") {
		t.Fatalf("cookie session without CSRF: %d %s", rec.Code, rec.Body.String())
	}
}

// miniApp serves one request through the Telegram-mode handler, same-origin, with the given Authorization values.
func miniApp(h http.Handler, method, path string, authz ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Origin", "https://tg.example.com")
	for _, a := range authz {
		req.Header.Add("Authorization", a)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Guard: authenticate in Telegram mode (§3.4.1). Only one "Authorization: Telegram <token>" header that resolves to a
// Telegram principal authenticates; anything else is 401, and an anonymous request reaches only the exchange.
func TestTelegramModeAuthentication(t *testing.T) {
	t.Parallel()
	bearerLike := &auth.Principal{User: &auth.User{ID: "usr_tg"}, TokenID: "atk_tg", Method: auth.MethodToken}
	tokenAndLink := tgPrincipal()
	tokenAndLink.TokenID = "atk_tg"
	rootAndLink := tgPrincipal()
	rootAndLink.RootTokenID = "rtk_1"
	nouser := tgPrincipal()
	nouser.User = nil
	plain := &auth.Principal{User: &auth.User{ID: "usr_tg"}, SessionID: "ses_1", Method: auth.MethodPassword}
	long := strings.Repeat("a", 257)
	// The resolver accepts the empty and the oversized token too, so only authenticateTelegram's own checks refuse them.
	sessions := &fakeSessions{p: map[string]*auth.Principal{"good": tgPrincipal(), "bearer": bearerLike, "tok": tokenAndLink,
		"root": rootAndLink, "nouser": nouser, "plain": plain, "": tgPrincipal(), long: tgPrincipal()}}
	h := tgDeps(t, sessions).TelegramHandler()
	path := tgPath("probe-both")
	if rec := miniApp(h, http.MethodPost, path, "Telegram good"); rec.Code != 204 {
		t.Fatalf("good session: %d %s", rec.Code, rec.Body.String())
	}
	if rec := miniApp(h, http.MethodPost, path, "telegram   good"); rec.Code != 204 {
		t.Fatalf("scheme is case-insensitive: %d %s", rec.Code, rec.Body.String())
	}
	for name, hdrs := range map[string][]string{
		"bearer api token":            {"Bearer spaceman_at_x"},
		"bearer root token":           {"Bearer spaceman_rt_x"},
		"basic":                       {"Basic Zm9vOmJhcg=="},
		"a session under bearer":      {"Bearer good"},
		"a session under basic":       {"Basic good"},
		"unknown session":             {"Telegram nope"},
		"empty token":                 {"Telegram"},
		"two headers":                 {"Telegram good", "Telegram good"},
		"telegram after another":      {"Basic x", "Telegram good"},
		"resolver returns bearer":     {"Telegram bearer"},
		"resolver returns token+link": {"Telegram tok"},
		"resolver returns root+link":  {"Telegram root"},
		"resolver returns no user":    {"Telegram nouser"},
		"resolver returns a session":  {"Telegram plain"},
		"oversized token":             {"Telegram " + long},
	} {
		if rec := miniApp(h, http.MethodPost, path, hdrs...); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// A resolver failure that isn't "unauthorized" is an internal error, never a pass.
	failing := tgDeps(t, &fakeSessions{err: errors.New("boom")}).TelegramHandler()
	if rec := miniApp(failing, http.MethodPost, path, "Telegram good"); rec.Code != http.StatusInternalServerError {
		t.Errorf("resolver error: %d", rec.Code)
	}
	// No resolver: the scheme never authenticates.
	if rec := miniApp(tgDeps(t, nil).TelegramHandler(), http.MethodPost, path, "Telegram good"); rec.Code != http.StatusUnauthorized {
		t.Errorf("no resolver: %d", rec.Code)
	}
	// Anonymous: only the exchange; any other public op (and every other op) is 401.
	if rec := miniApp(h, http.MethodPost, tgPath("probe-exchange")); rec.Code != 204 {
		t.Errorf("anonymous exchange: %d %s", rec.Code, rec.Body.String())
	}
	for _, id := range []string{"probe-public", "probe-both"} {
		if rec := miniApp(h, http.MethodPost, tgPath(id)); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	// A signed-in Telegram principal gets no other public op either.
	if rec := miniApp(h, http.MethodPost, tgPath("probe-public"), "Telegram good"); rec.Code != http.StatusUnauthorized {
		t.Errorf("telegram on a public op: %d", rec.Code)
	}
	// The bot in process gets no public op, and never the exchange.
	chat := tgDeps(t, nil).InProcessHandler()
	for _, id := range []string{"probe-public", "probe-exchange"} {
		if rec := inProcess(chat, tgPrincipal(), id); rec.Code != http.StatusForbidden {
			t.Errorf("chat %s: %d", id, rec.Code)
		}
	}
}

// Guard: the Mini App's cross-site check (§3.4.1). Mutations need the Mini App origin, not the public URL; without a
// configured origin every mutation is refused. Reads don't need it.
func TestTelegramModeSameOrigin(t *testing.T) {
	t.Parallel()
	sessions := &fakeSessions{p: map[string]*auth.Principal{"good": tgPrincipal()}}
	d := tgDeps(t, sessions)
	h := d.TelegramHandler()
	try := func(h http.Handler, method string, hdr map[string]string) int {
		req := httptest.NewRequest(method, tgPath("probe-both"), nil)
		req.Header.Set("Authorization", "Telegram good")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for name, tc := range map[string]struct {
		hdr  map[string]string
		want int
	}{
		"mini app origin":      {map[string]string{"Origin": "https://tg.example.com"}, 204},
		"mini app origin case": {map[string]string{"Origin": "https://TG.example.com"}, 204},
		"sec-fetch same":       {map[string]string{"Sec-Fetch-Site": "same-origin"}, 204},
		"public url origin":    {map[string]string{"Origin": "https://central.example.com"}, 403},
		"foreign origin":       {map[string]string{"Origin": "https://evil.example"}, 403},
		"origin prefix":        {map[string]string{"Origin": "https://tg.example.com.evil.com"}, 403},
		"origin suffix":        {map[string]string{"Origin": "https://evil.tg.example.com"}, 403},
		"http mini app origin": {map[string]string{"Origin": "http://tg.example.com"}, 403},
		"sec-fetch cross":      {map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		"nothing":              {nil, 403},
	} {
		if got := try(h, http.MethodPost, tc.hdr); got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
	for name, origin := range map[string]func() string{
		"nil":       nil,
		"empty":     func() string { return "" },
		"no scheme": func() string { return "tg.example.com" },
		"ftp":       func() string { return "ftp://tg.example.com" },
	} {
		nd := tgDeps(t, sessions)
		nd.Telegram.MiniAppOrigin = origin
		if got := try(nd.TelegramHandler(), http.MethodPost, map[string]string{"Origin": "https://tg.example.com"}); got != 403 {
			t.Errorf("origin %s: %d", name, got)
		}
		if got := try(nd.TelegramHandler(), http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin"}); got != 403 {
			t.Errorf("origin %s, Sec-Fetch-Site same-origin: %d", name, got)
		}
	}
}

// Guard: Telegram mode drops the Cookie request header and Set-Cookie on responses, and doesn't serve the MCP
// endpoint. The main host answers the Telegram scheme with 401 before looking at the cookie.
func TestTelegramModeCookiesAndHosts(t *testing.T) {
	t.Parallel()
	sessions := &fakeSessions{p: map[string]*auth.Principal{"good": tgPrincipal()}}
	d := tgDeps(t, sessions)
	var sawCookie []string
	d.Raw = []RawRoute{{Pattern: "GET " + Prefix + "/spaces/{space_id}/echo", Perm: authz.InboxRead, Telegram: TelegramMiniApp,
		Handler: func(*Deps) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				sawCookie = append(sawCookie, r.Header.Get("Cookie"))
				http.SetCookie(w, &http.Cookie{Name: "planted", Value: "1"})
				_, _ = w.Write([]byte("ok")) // no WriteHeader: the implicit 200 must not carry the cookie either
			}
		}}}
	d.MCP = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := d.TelegramHandler()
	req := httptest.NewRequest(http.MethodGet, Prefix+"/spaces/"+tgSpace+"/echo", nil)
	req.Header.Set("Authorization", "Telegram good")
	req.Header.Set("Cookie", "__Host-sm_session=abc; sm_session=abc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || len(sawCookie) != 1 || sawCookie[0] != "" || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("raw route: %d cookie %q set-cookie %q", rec.Code, sawCookie, rec.Header().Get("Set-Cookie"))
	}
	// A huma operation that sets a cookie: dropped too (the probe sets one on probe-cookie).
	rec = miniApp(h, http.MethodGet, tgPath("probe-cookie"), "Telegram good")
	if rec.Code != 204 || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("op: %d set-cookie %q", rec.Code, rec.Header().Get("Set-Cookie"))
	}
	main := d.Handler()
	// The MCP endpoint isn't served on the Mini App host.
	if rec := miniApp(h, http.MethodPost, MCPPath, "Telegram good"); rec.Code != http.StatusNotFound {
		t.Errorf("MCP path in Telegram mode: %d", rec.Code)
	}
	// Main host: the Telegram scheme is 401 whatever else the request carries, and never reaches the mux. The target is
	// a public operation, which answers 204 to the same request without the Telegram header.
	before := sessions.calls
	mainReq := func(hdrs ...string) int {
		req := httptest.NewRequest(http.MethodPost, tgPath("probe-public"), nil)
		req.Header.Set("Origin", "https://central.example.com")
		for _, v := range hdrs {
			req.Header.Add("Authorization", v)
		}
		req.Header.Set("Cookie", "__Host-sm_session=abc")
		rec := httptest.NewRecorder()
		main.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := mainReq(); got != http.StatusNoContent {
		t.Fatalf("main host, public op without Authorization: %d", got)
	}
	if got := mainReq("Basic x"); got != http.StatusNoContent {
		t.Fatalf("main host, another scheme: %d", got)
	}
	for name, hdrs := range map[string][]string{
		"telegram":           {"Telegram good"},
		"lower case":         {"telegram good"},
		"upper case":         {"TELEGRAM good"},
		"second header":      {"Basic x", "Telegram good"},
		"padded":             {"  Telegram good"},
		"scheme without arg": {"Telegram"},
	} {
		if got := mainReq(hdrs...); got != http.StatusUnauthorized {
			t.Errorf("main host %s: %d", name, got)
		}
	}
	if sessions.calls != before {
		t.Errorf("the main host must never resolve a Telegram session: %d resolver calls", sessions.calls)
	}
}

// Guard: authorizeRaw's Telegram class and feature (§3.3). Excluded, chat and unclassified raw routes are refused to
// the Mini App; a feature-gated route needs the space's feature, checked only after the permission (no access: 404).
func TestTelegramRawRoutes(t *testing.T) {
	t.Parallel()
	sessions := &fakeSessions{p: map[string]*auth.Principal{"good": tgPrincipal()}}
	d := tgDeps(t, sessions)
	features := map[string]bool{}
	d.Telegram.SpaceFeature = func(_ context.Context, space, feature string) bool { return features[space+"/"+feature] }
	ok := func(*Deps) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	}
	d.Raw = []RawRoute{
		{Pattern: "GET " + Prefix + "/spaces/{space_id}/r-mini", Perm: authz.InboxRead, Telegram: TelegramMiniApp, Handler: ok},
		{Pattern: "GET " + Prefix + "/spaces/{space_id}/r-both", Perm: authz.InboxRead, Telegram: TelegramBoth, Handler: ok},
		{Pattern: "GET " + Prefix + "/spaces/{space_id}/r-chat", Perm: authz.InboxRead, Telegram: TelegramChat, Handler: ok},
		{Pattern: "GET " + Prefix + "/spaces/{space_id}/r-excl", Perm: authz.InboxRead, Telegram: TelegramExcluded, Handler: ok},
		{Pattern: "GET " + Prefix + "/spaces/{space_id}/r-none", Perm: authz.InboxRead, Handler: ok},
		{Pattern: "GET " + Prefix + "/spaces/{space_id}/r-feat", Perm: authz.InboxRead, Telegram: TelegramMiniApp,
			TelegramFeature: TelegramFeatureTerminalWatch, Handler: ok},
	}
	h := d.TelegramHandler()
	get := func(space, name string) *httptest.ResponseRecorder {
		return miniApp(h, http.MethodGet, Prefix+"/spaces/"+space+"/"+name, "Telegram good")
	}
	for name, want := range map[string]int{"r-mini": 200, "r-both": 200, "r-chat": 403, "r-excl": 403, "r-none": 403, "r-feat": 403} {
		if rec := get(tgSpace, name); rec.Code != want {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	other := "spc_0000000000000000000000000o"
	features[other+"/"+TelegramFeatureTerminalWatch] = true
	if rec := get(other, "r-feat"); rec.Code != http.StatusNotFound {
		t.Errorf("feature on, no access: %d", rec.Code)
	}
	features[tgSpace+"/"+TelegramFeatureTerminalWatch] = true
	if rec := get(tgSpace, "r-feat"); rec.Code != 200 {
		t.Errorf("feature on: %d %s", rec.Code, rec.Body.String())
	}
	// No feature callback: off.
	d.Telegram.SpaceFeature = nil
	if rec := miniApp(d.TelegramHandler(), http.MethodGet, Prefix+"/spaces/"+tgSpace+"/r-feat", "Telegram good"); rec.Code != 403 {
		t.Errorf("no feature callback: %d", rec.Code)
	}
	// Other principals don't meet the class or the feature: a bearer token reads the excluded and gated routes.
	mux := http.NewServeMux()
	for _, rt := range d.Raw {
		mux.Handle(rt.Pattern, d.authorizeRaw(rt))
	}
	bearer := &auth.Principal{User: &auth.User{ID: "usr_tg"}, TokenID: "atk_tg", Method: auth.MethodToken}
	for _, name := range []string{"r-excl", "r-none", "r-feat"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"/spaces/"+tgSpace+"/"+name, nil).WithContext(
			context.WithValue(context.Background(), principalKey{}, bearer)))
		if rec.Code != 200 {
			t.Errorf("bearer on %s: %d", name, rec.Code)
		}
		// A Telegram principal off the Mini App channel gets nothing.
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"/spaces/"+tgSpace+"/r-mini", nil).WithContext(
			context.WithValue(context.Background(), principalKey{}, tgPrincipal())))
		if rec.Code != 403 {
			t.Errorf("off-channel telegram on a raw route: %d", rec.Code)
		}
	}
}

// Guard: watchHandler's class (§3.3). The feed is refused to the Mini App unless the policy classes it miniapp or
// both (the allowed stream is covered end to end in internal/app).
func TestTelegramWatchClass(t *testing.T) {
	t.Parallel()
	sessions := &fakeSessions{p: map[string]*auth.Principal{"good": tgPrincipal()}}
	for _, c := range []TelegramClass{"", TelegramExcluded, TelegramChat, TelegramExchange} {
		d := tgDeps(t, sessions)
		d.Telegram.Watch = c
		if rec := miniApp(d.TelegramHandler(), http.MethodGet, Prefix+"/watch", "Telegram good"); rec.Code != http.StatusForbidden {
			t.Errorf("watch class %q: %d %s", c, rec.Code, rec.Body.String())
		}
	}
	// A Telegram principal off the Mini App channel.
	d := tgDeps(t, sessions)
	rec := httptest.NewRecorder()
	d.watchHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"/watch", nil).WithContext(
		context.WithValue(context.Background(), principalKey{}, tgPrincipal())))
	if rec.Code != http.StatusForbidden {
		t.Errorf("off-channel watch: %d", rec.Code)
	}
}

// Guard: a Mini App stream re-validates through its session header, never the cookie: the session must still resolve,
// to the same link, without a token and not Limited.
func TestTelegramStreamRevalidation(t *testing.T) {
	t.Parallel()
	other := tgPrincipal()
	other.TelegramLinkID = "tgl_2"
	limited := tgPrincipal()
	limited.Limited = true
	withTok := tgPrincipal()
	withTok.TokenID = "atk_tg"
	withRoot := tgPrincipal()
	withRoot.RootTokenID = "rtk_1"
	plain := &auth.Principal{User: &auth.User{ID: "usr_tg"}, SessionID: "ses_1", Method: auth.MethodPassword}
	sessions := &fakeSessions{p: map[string]*auth.Principal{"good": tgPrincipal(), "other": other, "limited": limited, "tok": withTok,
		"root": withRoot, "plain": plain}}
	d := tgDeps(t, sessions)
	r := httptest.NewRequest(http.MethodGet, Prefix+"/watch", nil)
	check := func(tok string) bool {
		ctx := context.WithValue(context.Background(), telegramTokenKey{}, tok)
		_, ok := d.revalidate(ctx, r, tgPrincipal())
		return ok
	}
	if !check("good") {
		t.Fatal("a valid session must keep its stream")
	}
	for _, tok := range []string{"", "nope", "other", "limited", "tok", "root", "plain"} {
		if check(tok) {
			t.Errorf("session %q kept the stream", tok)
		}
	}
	d.Telegram.Sessions = nil
	if check("good") {
		t.Error("no resolver kept the stream")
	}
}

// Audit: a Telegram principal's actions are the user's, via the link, on the telegram channel.
func TestActorOfTelegramPrincipal(t *testing.T) {
	t.Parallel()
	ctx := context.WithValue(context.Background(), principalKey{}, tgPrincipal())
	if got, want := Actor(ctx), (audit.Actor{Type: audit.ActorUser, ID: "usr_tg", Via: "telegram:tgl_1", Channel: "telegram"}); got != want {
		t.Fatalf("actor %+v, want %+v", got, want)
	}
}

// Guard: telegramGate itself. Not a Telegram request: no opinion. On a Telegram channel only a Telegram principal is
// authenticated; a Telegram principal needs a class that admits its channel.
func TestTelegramGate(t *testing.T) {
	t.Parallel()
	bearer := &auth.Principal{User: &auth.User{ID: "usr_tg"}, TokenID: "atk_tg", Method: auth.MethodToken}
	for name, tc := range map[string]struct {
		ch    telegramChannel
		p     *auth.Principal
		class TelegramClass
		want  int // 0: allowed
	}{
		"bearer, no channel":          {channelNone, bearer, "", 0},
		"bearer on the mini app":      {channelMiniApp, bearer, TelegramBoth, http.StatusUnauthorized},
		"bearer in chat":              {channelChat, bearer, TelegramBoth, http.StatusUnauthorized},
		"telegram, no channel":        {channelNone, tgPrincipal(), TelegramBoth, http.StatusForbidden},
		"telegram chat, both":         {channelChat, tgPrincipal(), TelegramBoth, 0},
		"telegram chat, miniapp":      {channelChat, tgPrincipal(), TelegramMiniApp, http.StatusForbidden},
		"telegram mini app, chat":     {channelMiniApp, tgPrincipal(), TelegramChat, http.StatusForbidden},
		"telegram mini app, exchange": {channelMiniApp, tgPrincipal(), TelegramExchange, http.StatusForbidden},
		"telegram mini app, none":     {channelMiniApp, tgPrincipal(), "", http.StatusForbidden},
	} {
		got := 0
		if prob := telegramGate(tc.ch, tc.p, tc.class); prob != nil {
			got = prob.Status
		}
		if got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
}

// Guard: a flushed streaming response on the Mini App host has Set-Cookie stripped before the headers go out, even when
// the handler flushes before it writes (the change feed and the terminal do). The recorder snapshots the headers at
// the flush, as a real server sends them.
func TestTelegramModeFlushStripsCookies(t *testing.T) {
	t.Parallel()
	d := tgDeps(t, &fakeSessions{p: map[string]*auth.Principal{"good": tgPrincipal()}})
	d.Raw = []RawRoute{{Pattern: "GET " + Prefix + "/spaces/{space_id}/stream", Perm: authz.InboxRead, Telegram: TelegramMiniApp,
		Handler: func(*Deps) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				http.SetCookie(w, &http.Cookie{Name: "planted", Value: "1"})
				w.(http.Flusher).Flush()
				_, _ = w.Write([]byte("data"))
			}
		}}}
	req := httptest.NewRequest(http.MethodGet, Prefix+"/spaces/"+tgSpace+"/stream", nil)
	req.Header.Set("Authorization", "Telegram good")
	rec := httptest.NewRecorder()
	d.TelegramHandler().ServeHTTP(rec, req)
	if rec.Code != 200 || !rec.Flushed {
		t.Fatalf("stream: %d flushed %v", rec.Code, rec.Flushed)
	}
	if got := rec.Result().Header.Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("flushed response carries Set-Cookie: %q", got)
	}
}

// Guard: huma's anonymous documentation routes (OpenAPI documents, JSON schemas) are not served on the Mini App host,
// so an anonymous request reaches only the session exchange; the main host still serves them.
func TestTelegramModeHidesSpec(t *testing.T) {
	t.Parallel()
	d := tgDeps(t, &fakeSessions{p: map[string]*auth.Principal{"good": tgPrincipal()}})
	mini, main := d.TelegramHandler(), d.Handler()
	for _, path := range []string{"/openapi.json", "/openapi.yaml", "/openapi-3.0.json", "/openapi-3.0.yaml", "/schemas/Problem.json"} {
		if rec := miniApp(mini, http.MethodGet, Prefix+path); rec.Code != http.StatusNotFound {
			t.Errorf("mini app %s anonymous: %d", path, rec.Code)
		}
		if rec := miniApp(mini, http.MethodGet, Prefix+path, "Telegram good"); rec.Code != http.StatusNotFound {
			t.Errorf("mini app %s signed in: %d", path, rec.Code)
		}
	}
	if rec := miniApp(main, http.MethodGet, Prefix+"/openapi.json"); rec.Code != http.StatusOK {
		t.Errorf("main host openapi.json: %d", rec.Code)
	}
}
