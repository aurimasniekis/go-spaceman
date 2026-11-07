// Package hpfake is a test double of the Harbor Pilot 0.1.3 endpoints SpaceMan calls (mint, revoke, budget status,
// projects; docs/contracts/harbor-pilot.md). It is never evidence of a working integration: the real gateway is
// exercised by the opt-in hptest tests.
package hpfake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	scopeInvoke = "gateway:invoke"
	scopeModels = "gateway:models"
	scopeMint   = "tokens:mint"
)

// HP is a fake gateway. Change its behaviour with Set; its fields are guarded by the mutex Set holds.
type HP struct {
	srv *httptest.Server

	mu            sync.Mutex
	Parent        string            // the valid parent token
	OtherParents  []string          // more valid parent tokens of the same binding (e.g. one SpaceMan replaced but nobody revoked)
	ParentScopes  []string          // its scopes
	Service       string            // the valid service-principal token
	ServicePerms  []string          // usage.read, config.read
	BindingID     string            // the parent's client binding
	Allowed       []string          // project ids a child may be bound to
	Projects      map[string]string // id or name → id
	ParentExpires time.Time         // zero: no expiry
	Old           bool              // a gateway older than 0.1.3 (no check_budget)
	FailMint      int               // when non-zero, every mint answers this status
	FailRevoke    bool              // revocations answer 500
	Minted        map[string]bool   // credential id → revoked
	Owner         map[string]string // credential id → the parent token that minted it: only that one can revoke it
	Bodies        []string          // raw mint bodies
	Requests      int               // every request served
	// Hook, when set, runs before each request is served (without the fake's lock), e.g. to change state mid-call.
	Hook func(r *http.Request)
	seq  int
}

// New starts a fake gateway for t, with a valid parent (invoke, models, mint) and service token (usage.read,
// config.read), binding bnd_fake, allowed projects prj_web and prj_data, and projects prj_web (name web-app),
// prj_data and prj_other.
func New(t testing.TB) *HP {
	t.Helper()
	f := &HP{Parent: "hpk_parent_" + strings.Repeat("p", 24), ParentScopes: []string{scopeInvoke, scopeModels, scopeMint},
		Service: "hpa_service_" + strings.Repeat("s", 24), ServicePerms: []string{"usage.read", "config.read"}, BindingID: "bnd_fake",
		Allowed: []string{"prj_web", "prj_data"}, Projects: map[string]string{"prj_web": "prj_web", "web-app": "prj_web", "prj_data": "prj_data", "prj_other": "prj_other"},
		Minted: map[string]bool{}, Owner: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the gateway origin.
func (f *HP) URL() string { return f.srv.URL }

func gwErr(w http.ResponseWriter, status int, typ, msg string, code any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": typ, "param": nil, "code": code}})
}

func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"title": http.StatusText(status), "status": status, "detail": detail})
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (f *HP) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	hook := f.Hook
	f.mu.Unlock()
	if hook != nil {
		hook(r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests++
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/harbor/credentials":
		f.mint(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/harbor/credentials/"):
		if !f.validParent(bearer(r)) {
			gwErr(w, 401, "invalid_request_error", "invalid, expired or revoked gateway credential", "invalid_api_key")
			return
		}
		if f.FailRevoke {
			gwErr(w, 500, "api_error", "revocation failed", nil)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/harbor/credentials/")
		if _, ok := f.Minted[id]; !ok || f.Owner[id] != bearer(r) {
			gwErr(w, 404, "invalid_request_error", "no child credential with this id is owned by the caller", "model_not_found")
			return
		}
		f.Minted[id] = true
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/budgets/status":
		if !f.authAPI(w, r, "usage.read") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"budget_id":"bdg_1","name":"daily","scope":"project","scope_id":"prj_web","period":"day","currency":"USD",`+
			`"enforcement":"block","enabled":true,"limit_e10":10000000000,"spent_e10":5,"reserved_e10":0,"fraction":0.1,"state":"ok"}]}`)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/projects/"):
		if !f.authAPI(w, r, "config.read") {
			return
		}
		id, ok := f.Projects[strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")]
		if !ok {
			problem(w, 404, "project not found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "kind": "project", "name": id + "-name", "spec": map[string]any{"workspace_id": "wsp_1"}})
	default:
		problem(w, 404, "no route")
	}
}

func (f *HP) authAPI(w http.ResponseWriter, r *http.Request, perm string) bool {
	if bearer(r) != f.Service {
		problem(w, 401, "authentication required")
		return false
	}
	for _, p := range f.ServicePerms {
		if p == perm {
			return true
		}
	}
	problem(w, 403, "missing permission "+perm)
	return false
}

// validParent reports whether tok is a valid parent token (Harbor Pilot keeps a replaced parent valid until it is revoked).
func (f *HP) validParent(tok string) bool {
	return tok == f.Parent || contains(f.OtherParents, tok)
}

func (f *HP) mint(w http.ResponseWriter, r *http.Request) {
	if !f.validParent(bearer(r)) {
		gwErr(w, 401, "invalid_request_error", "invalid, expired or revoked gateway credential", "invalid_api_key")
		return
	}
	raw, _ := io.ReadAll(r.Body)
	f.Bodies = append(f.Bodies, string(raw))
	var in map[string]json.RawMessage
	if err := json.Unmarshal(raw, &in); err != nil {
		gwErr(w, 400, "invalid_request_error", "invalid body: "+err.Error(), nil)
		return
	}
	for k := range in {
		known := k == "name" || k == "scopes" || k == "ttl_seconds" || k == "bound_refs" || (k == "check_budget" && !f.Old)
		if !known {
			gwErr(w, 400, "invalid_request_error", `invalid body: json: unknown field "`+k+`"`, nil)
			return
		}
	}
	if f.FailMint != 0 {
		switch f.FailMint {
		case 429:
			gwErr(w, 429, "insufficient_quota", "budget daily is exhausted for this day (0.1 of 0.1 USD) (bdg_1)", "budget_exceeded")
		default:
			gwErr(w, f.FailMint, "api_error", "could not mint the credential", nil)
		}
		return
	}
	if !contains(f.ParentScopes, scopeMint) {
		gwErr(w, 403, "permission_error", "invalid, expired or revoked gateway credential: parent credential lacks tokens:mint", nil)
		return
	}
	var body struct {
		Scopes    []string          `json:"scopes"`
		TTL       int64             `json:"ttl_seconds"`
		BoundRefs map[string]string `json:"bound_refs"`
	}
	_ = json.Unmarshal(raw, &body)
	for _, s := range body.Scopes {
		if s == scopeMint || !contains(f.ParentScopes, s) {
			gwErr(w, 400, "invalid_request_error", "body.scopes: child scopes must be a subset of the parent's, excluding tokens:mint", nil)
			return
		}
	}
	if p, ok := body.BoundRefs["project_id"]; ok && !contains(f.Allowed, p) {
		gwErr(w, 400, "invalid_request_error", "body.bound_refs.project_id: project must be the client's project or one of its allowed projects", nil)
		return
	}
	ttl := time.Duration(body.TTL) * time.Second
	if ttl == 0 {
		ttl = 12 * time.Hour
	}
	exp := time.Now().Add(ttl)
	if !f.ParentExpires.IsZero() && exp.After(f.ParentExpires) {
		exp = f.ParentExpires
	}
	f.seq++
	id := fmt.Sprintf("crd_%04d", f.seq)
	f.Minted[id] = false
	f.Owner[id] = bearer(r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"credential": map[string]any{"id": id, "binding_id": f.BindingID, "parent_id": "crd_parent",
		"scopes": body.Scopes, "bound_refs": body.BoundRefs, "expires_at": exp.UTC()}, "token": "hpk_child_" + id})
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Unrevoked lists minted credentials that were not revoked.
func (f *HP) Unrevoked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id, rev := range f.Minted {
		if !rev {
			out = append(out, id)
		}
	}
	return out
}

// Count is the number of requests served.
func (f *HP) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Requests
}

// Set changes the fake's state under its lock.
func (f *HP) Set(fn func(f *HP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}
