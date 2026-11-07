package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
}
