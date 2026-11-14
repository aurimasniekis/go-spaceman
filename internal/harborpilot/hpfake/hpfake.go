// Package hpfake is a test double of the Harbor Pilot 0.1.3 endpoints SpaceMan calls (mint, revoke, budget status,
// projects, usage summary, quota board; docs/contracts/harbor-pilot.md), and, when Version is set, of 0.1.4's (GET /api/v1/system
// and budget status filtered by project). It is never evidence of a working integration: the real gateway is
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
	// Budgets is the body of GET /api/v1/budgets/status's items (JSON array); "" serves one ok project budget on
	// prj_web. FailBudgets, when non-zero, makes budget status answer that status. BudgetReads counts its reads.
	Budgets     string
	FailBudgets int
	BudgetReads int
	// BudgetQueries records every budget status read's raw query string.
	BudgetQueries []string
	// Version, when set, makes the fake a Harbor Pilot 0.1.4 or later: GET /api/v1/system answers it (config.read),
	// and budget status honours project_id and binding_id (only the budgets covering that project, its workspace and
	// the binding). Empty: a 0.1.3 gateway, which answers 404 there and ignores the filters. SystemReads counts reads.
	Version     string
	SystemReads int
	// Usage maps a usage summary query to its JSON answer: the key is the "ref" filter (e.g. "spaceman.run:run_x"), or
	// "group_by=<dim>" for a query without one; an unknown query answers no groups. FailUsage, when non-zero, makes
	// usage summary answer that status. UsageQueries records every usage query's raw query string.
	Usage        map[string]string
	FailUsage    int
	UsageQueries []string
	// Board is the body of GET /api/v1/quota/board (usage.read); "" serves one available account. FailBoard, when
	// non-zero, makes it answer that status. BoardReads counts its reads.
	Board      string
	FailBoard  int
	BoardReads int
	// Workspace is the workspace every project belongs to (default wsp_1). ProjectReads counts project lookups.
	Workspace    string
	ProjectReads int
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
		f.BudgetReads++
		f.BudgetQueries = append(f.BudgetQueries, r.URL.RawQuery)
		if f.FailBudgets != 0 {
			problem(w, f.FailBudgets, "budget status failed")
			return
		}
		items := f.Budgets
		if items == "" {
			items = `[{"budget_id":"bdg_1","name":"daily","scope":"project","scope_id":"prj_web","period":"day","currency":"USD",` +
				`"enforcement":"block","enabled":true,"limit_e10":10000000000,"spent_e10":5,"reserved_e10":0,"fraction":0.1,"state":"ok"}]`
		}
		if f.Version != "" && r.URL.Query().Get("project_id") != "" {
			var ok bool
			if items, ok = f.filterCovering(w, items, r.URL.Query().Get("project_id"), r.URL.Query().Get("binding_id")); !ok {
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":`+items+`}`)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/system" && f.Version != "":
		if !f.authAPI(w, r, "config.read") {
			return
		}
		f.SystemReads++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"version": f.Version, "commit": "0123456789abcdef0123456789abcdef01234567",
			"started_at": "2026-11-08T10:00:00Z", "schema_version": 20261108100000000})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/quota/board":
		if !f.authAPI(w, r, "usage.read") {
			return
		}
		f.BoardReads++
		if f.FailBoard != 0 {
			problem(w, f.FailBoard, "quota board failed")
			return
		}
		body := f.Board
		if body == "" {
			body = `{"monitors":[],"observed_accounts":[],"account_health":[{"account_id":"acc_1","name":"one","billing":"metered_api",` +
				`"available":true,"reason":"","until":null,"remaining":null,"reset_at":null,"stale":false}],"dropped_observations":0}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body) //nolint:gosec // a test double serving the JSON the test configured
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/usage/summary":
		if !f.authAPI(w, r, "usage.read") {
			return
		}
		f.UsageQueries = append(f.UsageQueries, r.URL.RawQuery)
		if f.FailUsage != 0 {
			problem(w, f.FailUsage, "usage failed")
			return
		}
		key := r.URL.Query().Get("ref")
		if key == "" {
			key = "group_by=" + r.URL.Query().Get("group_by")
		}
		body, ok := f.Usage[key]
		if !ok {
			body = `{"group_by":"` + r.URL.Query().Get("group_by") + `","groups":[],"totals":[]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body) //nolint:gosec // a test double serving the JSON the test configured
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/projects/"):
		if !f.authAPI(w, r, "config.read") {
			return
		}
		f.ProjectReads++
		id, ok := f.Projects[strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")]
		if !ok {
			problem(w, 404, "project not found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "kind": "project", "name": id + "-name", "spec": map[string]any{"workspace_id": f.workspace()}})
	default:
		problem(w, 404, "no route")
	}
}

// filterCovering keeps the budgets covering a project as Harbor Pilot 0.1.4 does: global, the project's workspace, the
// project and, with bindingID, the binding; an unknown project or binding is 404.
func (f *HP) filterCovering(w http.ResponseWriter, items, projectID, bindingID string) (string, bool) {
	id, ok := f.Projects[projectID]
	if !ok || id != projectID {
		problem(w, 404, "project not found")
		return "", false
	}
	if bindingID != "" && bindingID != f.BindingID {
		problem(w, 404, "client binding not found")
		return "", false
	}
	var all []map[string]any
	if err := json.Unmarshal([]byte(items), &all); err != nil {
		problem(w, 500, "bad fixture")
		return "", false
	}
	out := []map[string]any{}
	for _, b := range all {
		sid, _ := b["scope_id"].(string)
		switch b["scope"] {
		case "global":
		case "workspace":
			if sid != f.workspace() {
				continue
			}
		case "project":
			if sid != projectID {
				continue
			}
		case "binding":
			if bindingID == "" || sid != bindingID {
				continue
			}
		default:
			continue
		}
		if _, ok := b["remaining_e10"]; !ok { // 0.1.4 always sends it
			lim, _ := b["limit_e10"].(float64)
			spent, _ := b["spent_e10"].(float64)
			res, _ := b["reserved_e10"].(float64)
			b["remaining_e10"] = max(int64(lim-spent-res), 0)
		}
		out = append(out, b)
	}
	raw, _ := json.Marshal(out)
	return string(raw), true
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

func (f *HP) workspace() string {
	if f.Workspace == "" {
		return "wsp_1"
	}
	return f.Workspace
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
