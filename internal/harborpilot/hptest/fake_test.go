package hptest

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// fake is a minimal stand-in for the Harbor Pilot v0.1.1 endpoints hptest uses, shaped on the behaviour recorded in
// docs/contracts/harbor-pilot.md. It lets the package's own logic be tested without the real binary; it is never
// evidence about Harbor Pilot itself.
type fake struct {
	mu      sync.Mutex
	fail    string // a path whose first POST answers 500
	csrf    string
	tokens  map[string]string // token -> credential id
	revoked map[string]bool   // credential id
	parent  string            // parent credential id
	allowed map[string]bool   // allowed project ids
}

func newFake() *fake {
	return &fake{csrf: "csrf-" + rand.Text(), tokens: map[string]string{}, revoked: map[string]bool{}, allowed: map[string]bool{}}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func oaiErr(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": typ}})
}

func (f *fake) issue(id string) string {
	tok := "hpk_" + rand.Text()
	f.tokens[tok] = id
	return tok
}

func (f *fake) bearer(r *http.Request) (string, bool) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	id, ok := f.tokens[tok]
	return id, ok && !f.revoked[id]
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	if r.Method == http.MethodPost && p == f.fail {
		f.fail = ""
		writeJSON(w, http.StatusInternalServerError, map[string]any{"title": "Internal Server Error", "detail": "injected"})
		return
	}
	admin := strings.HasPrefix(p, "/api/v1/") && p != "/api/v1/budgets/status"
	if admin && p != "/api/v1/session" {
		if c, err := r.Cookie("hp_session"); err != nil || c.Value != "ok" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"title": "Unauthorized"})
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-CSRF-Token") != f.csrf {
			writeJSON(w, http.StatusForbidden, map[string]any{"title": "Forbidden", "detail": "csrf"})
			return
		}
	}
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	switch {
	case p == "/readyz":
		w.WriteHeader(http.StatusOK)
	case p == "/api/v1/session" && r.Method == http.MethodPost:
		if body["email"] != demoAdminEmail || body["password"] != demoAdminPassword {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"title": "Unauthorized", "detail": "invalid credentials"})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "hp_session", Value: "ok", Path: "/"})
		writeJSON(w, http.StatusOK, map[string]any{"csrf_token": f.csrf})
	case p == "/api/v1/routing-plans":
		writeJSON(w, http.StatusOK, map[string]any{"items": []named{{ID: "pln_1", Name: "team-default"}}})
	case p == "/api/v1/projects":
		writeJSON(w, http.StatusOK, map[string]any{"items": []named{{ID: "prj_web", Name: "web-app"}, {ID: "prj_data", Name: "data-pipeline"}}})
	case p == "/api/v1/client-bindings" && r.Method == http.MethodPost:
		spec, _ := body["spec"].(map[string]any)
		ids, _ := spec["allowed_project_ids"].([]any)
		for _, id := range ids {
			f.allowed[id.(string)] = true
		}
		writeJSON(w, http.StatusCreated, named{ID: "cbn_1", Name: body["name"].(string)})
	case p == "/api/v1/budgets" && r.Method == http.MethodPost:
		writeJSON(w, http.StatusCreated, named{ID: "bdg_1", Name: body["name"].(string)})
	case p == "/api/v1/service-principals" && r.Method == http.MethodPost:
		writeJSON(w, http.StatusCreated, named{ID: "pri_1", Name: body["name"].(string)})
	case p == "/api/v1/service-principals/pri_1/tokens" && r.Method == http.MethodPost:
		tok := "hpa_" + rand.Text()
		f.tokens[tok] = "sp"
		writeJSON(w, http.StatusCreated, map[string]any{"plaintext": tok})
	case p == "/api/v1/client-bindings/cbn_1/credentials" && r.Method == http.MethodPost:
		f.parent = "crd_parent"
		writeJSON(w, http.StatusCreated, map[string]any{"credential": map[string]any{"id": f.parent, "binding_id": "cbn_1"}, "token": f.issue(f.parent)})
	case p == "/api/v1/budgets/status":
		if id, ok := f.bearer(r); !ok || id != "sp" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"title": "Unauthorized"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": []map[string]any{{"budget_id": "bdg_1", "name": BudgetName, "scope": "project",
			"scope_id": "prj_data", "enforcement": "block", "state": "exceeded"}}})
	case p == "/v1/harbor/credentials" && r.Method == http.MethodPost:
		if id, ok := f.bearer(r); !ok || id != f.parent {
			oaiErr(w, http.StatusUnauthorized, "invalid_api_key", "invalid or revoked credential")
			return
		}
		refs, _ := body["bound_refs"].(map[string]any)
		if pid, ok := refs["project_id"].(string); ok && !f.allowed[pid] {
			oaiErr(w, http.StatusBadRequest, "invalid_request_error", "project_id is not allowed for this binding")
			return
		}
		id := "crd_" + rand.Text()
		writeJSON(w, http.StatusCreated, map[string]any{"credential": map[string]any{"id": id, "binding_id": "cbn_1", "parent_id": f.parent,
			"bound_refs": refs}, "token": f.issue(id)})
	case strings.HasPrefix(p, "/v1/harbor/credentials/") && r.Method == http.MethodDelete:
		if _, ok := f.bearer(r); !ok {
			oaiErr(w, http.StatusUnauthorized, "invalid_api_key", "invalid or revoked credential")
			return
		}
		f.revoked[strings.TrimPrefix(p, "/v1/harbor/credentials/")] = true
		w.WriteHeader(http.StatusNoContent)
	case p == "/v1/models":
		if _, ok := f.bearer(r); !ok {
			oaiErr(w, http.StatusUnauthorized, "invalid_api_key", "invalid or revoked credential")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []map[string]any{{"id": "coding/default"}}})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"title": "Not Found"})
	}
}
