package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
)

func internalPrincipal() *auth.Principal {
	return &auth.Principal{User: &auth.User{ID: "usr_owner"}, SessionID: "atk_mcp", TokenID: "atk_mcp",
		TokenPurpose: auth.TokenPurposeSpacemanMCP, RunID: "run_x", Method: auth.MethodInternal}
}

// A SpaceMan MCP call (ADR-0063) is audited as its owner, via the internal token, for the run; an ordinary bearer token
// keeps its own "via" and no run.
func TestActorOfInternalPrincipal(t *testing.T) {
	t.Parallel()
	ctx := context.WithValue(context.Background(), principalKey{}, internalPrincipal())
	if got, want := Actor(ctx), (audit.Actor{Type: audit.ActorUser, ID: "usr_owner", Via: "spaceman_mcp:atk_mcp", Run: "run_x"}); got != want {
		t.Fatalf("actor %+v, want %+v", got, want)
	}
	bearer := &auth.Principal{User: &auth.User{ID: "usr_owner"}, TokenID: "atk_ci", Method: auth.MethodToken}
	ctx = context.WithValue(context.Background(), principalKey{}, bearer)
	if got, want := Actor(ctx), (audit.Actor{Type: audit.ActorUser, ID: "usr_owner", Via: "api_token:atk_ci"}); got != want {
		t.Fatalf("actor %+v, want %+v", got, want)
	}
	ep := *bearer
	ep.MCPEndpoint = true
	ctx = context.WithValue(context.Background(), principalKey{}, &ep)
	if got, want := Actor(ctx), (audit.Actor{Type: audit.ActorUser, ID: "usr_owner", Via: "api_token:atk_ci", Channel: audit.ChannelMCPEndpoint}); got != want {
		t.Fatalf("actor %+v, want %+v", got, want)
	}
}

// An internal principal never reaches an operation that needs a person at the keyboard: own-account (Session,
// UserOnly) and step-up (Reauth, MFA) operations are refused before any permission check, although bearer tokens skip
// step-up.
func TestInternalPrincipalRefusedForPersonalOperations(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	Register(mux, &Deps{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, internalPrincipal())))
	})
	for _, rq := range []struct{ method, path, body string }{
		{http.MethodGet, "/auth/me", ""},                         // Session
		{http.MethodGet, "/auth/sessions", ""},                   // Session, UserOnly
		{http.MethodPost, "/tokens", `{"name":"x"}`},             // Session, UserOnly, Reauth, MFA
		{http.MethodPost, "/users", `{"email":"a@example.com"}`}, // StepUp
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(rq.method, Prefix+rq.path, strings.NewReader(rq.body)))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not available through the SpaceMan MCP") {
			t.Errorf("%s %s: %d %s", rq.method, rq.path, rec.Code, rec.Body.String())
		}
	}
}

// InProcessHandler (ADR-0063) serves only huma operations, only to a caller that injected a principal: a request
// without one is unauthenticated (a bearer header is ignored), and neither the watch stream nor raw routes exist on it.
func TestInProcessHandler(t *testing.T) {
	t.Parallel()
	d := &Deps{Raw: []RawRoute{{Pattern: "GET " + Prefix + "/raw", Handler: func(*Deps) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }
	}}}}
	h := d.InProcessHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, Prefix+"/auth/me", nil)
	req.Header.Set("Authorization", "Bearer spm_whatever")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get(RequestIDHeader) == "" {
		t.Fatalf("no principal: %d %s", rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/watch", "/raw"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+path, nil).WithContext(WithInProcessPrincipal(context.Background(), internalPrincipal())))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"/auth/me", nil).WithContext(WithInProcessPrincipal(context.Background(), internalPrincipal())))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not available through the SpaceMan MCP") {
		t.Fatalf("internal principal on a session rule: %d %s", rec.Code, rec.Body.String())
	}
	// The external MCP endpoint's principal gets the same refusal.
	ep := &auth.Principal{User: &auth.User{ID: "usr_owner"}, SessionID: "atk_ci", TokenID: "atk_ci", Method: auth.MethodToken, MCPEndpoint: true}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"/auth/me", nil).WithContext(WithInProcessPrincipal(context.Background(), ep)))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not available through the SpaceMan MCP") {
		t.Fatalf("MCP endpoint principal on a session rule: %d %s", rec.Code, rec.Body.String())
	}
	// Only an internal token's principal or the MCP endpoint's may be injected: a bearer token's (which skips step-up)
	// or root's is refused before any rule runs, even on a Session rule that any signed-in principal passes over HTTP.
	for name, p := range map[string]*auth.Principal{
		"bearer":                        {User: &auth.User{ID: "usr_owner"}, SessionID: "atk_ci", TokenID: "atk_ci", Method: auth.MethodToken},
		"root":                          {RootTokenID: "rtk_1", Method: auth.MethodRoot},
		"session":                       {User: &auth.User{ID: "usr_owner"}, SessionID: "ses_1", Method: auth.MethodPassword},
		"root with an internal purpose": {RootTokenID: "rtk_1", TokenPurpose: auth.TokenPurposeSpacemanMCP},
		"session marked MCP endpoint":   {User: &auth.User{ID: "usr_owner"}, SessionID: "ses_1", Method: auth.MethodPassword, MCPEndpoint: true},
		"root marked MCP endpoint":      {RootTokenID: "rtk_1", Method: auth.MethodRoot, MCPEndpoint: true},
	} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"/auth/me", nil).WithContext(WithInProcessPrincipal(context.Background(), p)))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "only an MCP or Telegram principal") {
			t.Errorf("%s principal: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

// A Session read filtered by the principal's grants (Rule.Filtered) is open to an internal principal; a personal
// Session operation is not. Both stay open to an ordinary bearer token.
func TestInternalPrincipalOnFilteredReads(t *testing.T) {
	t.Parallel()
	ok := func(api huma.API, _ *Deps) {
		for _, id := range []string{"probe-filtered", "probe-personal"} {
			huma.Register(api, huma.Operation{OperationID: id, Method: http.MethodGet, Path: "/" + id, DefaultStatus: http.StatusNoContent},
				func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
		}
	}
	d := &Deps{Extra: []func(huma.API, *Deps){ok}, ExtraRules: map[string]Rule{"probe-filtered": FilteredRead(), "probe-personal": {Session: true}}}
	h := d.InProcessHandler()
	mux := http.NewServeMux()
	Register(mux, d)
	bearer := &auth.Principal{User: &auth.User{ID: "usr_owner"}, SessionID: "atk_ci", TokenID: "atk_ci", Method: auth.MethodToken}
	for _, tc := range []struct {
		path      string
		internal  int
		bearerGot int
	}{
		{"/probe-filtered", http.StatusNoContent, http.StatusNoContent},
		{"/probe-personal", http.StatusForbidden, http.StatusNoContent},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+tc.path, nil).WithContext(WithInProcessPrincipal(context.Background(), internalPrincipal())))
		if rec.Code != tc.internal {
			t.Errorf("internal %s: %d %s", tc.path, rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+tc.path, nil).WithContext(context.WithValue(context.Background(), principalKey{}, bearer)))
		if rec.Code != tc.bearerGot {
			t.Errorf("bearer %s: %d %s", tc.path, rec.Code, rec.Body.String())
		}
	}
}
