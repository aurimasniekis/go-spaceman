package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	method, path, auth, ifMatch, idem, ctype string
	body                                     string
}

// fakeAPI answers with the queued responses in order and records every request.
type fakeAPI struct {
	mu   sync.Mutex
	reqs []recorded
	resp []func(w http.ResponseWriter)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), ifMatch: r.Header.Get("If-Match"),
		idem: r.Header.Get("Idempotency-Key"), ctype: r.Header.Get("Content-Type"), body: string(b)})
	var next func(http.ResponseWriter)
	if len(f.resp) > 0 {
		next, f.resp = f.resp[0], f.resp[1:]
	}
	f.mu.Unlock()
	if next == nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	next(w)
}

func jsonResp(status int, v any) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
}

func problem(status int, code, detail string, fields ...FieldError) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://example/errors#" + code, "title": http.StatusText(status), "status": status,
			"code": code, "detail": detail, "errors": fields})
	}
}

func newTest(t *testing.T, f *fakeAPI) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "spaceman_at_test", Options{})
	if err != nil {
		t.Fatal(err)
	}
	c.retryWait = func(int) time.Duration { return time.Millisecond }
	return c
}

func TestNewEndpoint(t *testing.T) {
	for _, tc := range []struct {
		endpoint, base string
		insecure       bool
		wantErr        string
	}{
		{endpoint: "https://sm.example.com", base: "https://sm.example.com/api/v1"},
		{endpoint: "https://sm.example.com/", base: "https://sm.example.com/api/v1"},
		{endpoint: "https://sm.example.com/api/v1", base: "https://sm.example.com/api/v1"},
		{endpoint: "https://sm.example.com/prefix/api/v1/", base: "https://sm.example.com/prefix/api/v1"},
		{endpoint: "http://127.0.0.1:38180", base: "http://127.0.0.1:38180/api/v1"},
		{endpoint: "http://localhost:8080", base: "http://localhost:8080/api/v1"},
		{endpoint: "http://[::1]:8080", base: "http://[::1]:8080/api/v1"},
		{endpoint: "http://sm.internal:8080", wantErr: "plain http"},
		{endpoint: "http://sm.internal:8080", insecure: true, base: "http://sm.internal:8080/api/v1"},
		{endpoint: "sm.example.com", wantErr: "absolute"},
		{endpoint: "ftp://sm.example.com", wantErr: "absolute"},
		{endpoint: "https://user:pw@sm.example.com", wantErr: "credentials"},
		{endpoint: "https://sm.example.com/?x=1", wantErr: "query"},
	} {
		c, err := New(tc.endpoint, "tok", Options{AllowInsecureHTTP: tc.insecure})
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want %q", tc.endpoint, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.endpoint, err)
			continue
		}
		if c.BaseURL() != tc.base {
			t.Errorf("%s: base = %s, want %s", tc.endpoint, c.BaseURL(), tc.base)
		}
	}
	if _, err := New("https://sm.example.com", " ", Options{}); err == nil {
		t.Error("an empty token must be refused")
	}
}

func TestCreateSendsIdempotencyKeyAndRetriesWithTheSameKey(t *testing.T) {
	f := &fakeAPI{resp: []func(http.ResponseWriter){
		problem(http.StatusServiceUnavailable, "unavailable", "storage temporarily unavailable"),
		jsonResp(http.StatusCreated, Space{ID: "spc_1", Slug: "s", Version: 1}),
	}}
	c := newTest(t, f)
	s, err := c.CreateSpace(context.Background(), CreateSpaceInput{Slug: "s", Name: "S", NodeID: "nod_1"})
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "spc_1" || s.Version != 1 {
		t.Fatalf("space = %+v", s)
	}
	if len(f.reqs) != 2 {
		t.Fatalf("requests = %d, want a retry", len(f.reqs))
	}
	a, b := f.reqs[0], f.reqs[1]
	if a.idem == "" || a.idem != b.idem || len(a.idem) < 8 || len(a.idem) > 128 {
		t.Fatalf("idempotency keys %q, %q: want the same valid key on the retry", a.idem, b.idem)
	}
	if a.auth != "Bearer spaceman_at_test" || a.ctype != "application/json" || a.method != http.MethodPost || a.path != "/api/v1/spaces" {
		t.Fatalf("request = %+v", a)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(a.body), &body); err != nil || body["slug"] != "s" || body["node_id"] != "nod_1" {
		t.Fatalf("body = %s", a.body)
	}
	if _, ok := body["description"]; ok {
		t.Fatal("empty optional fields are omitted on create")
	}
}

func TestNonIdempotentCreateIsNotRetried(t *testing.T) {
	f := &fakeAPI{resp: []func(http.ResponseWriter){problem(http.StatusServiceUnavailable, "unavailable", "busy")}}
	c := newTest(t, f)
	_, err := c.CreatePrompt(context.Background(), CreatePromptInput{Slug: "p", Name: "P", Kind: "role"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v", err)
	}
	if len(f.reqs) != 1 || f.reqs[0].idem != "" {
		t.Fatalf("requests = %+v: a create without Idempotency-Key support must not be retried", f.reqs)
	}
}

func TestUpdateSendsIfMatch(t *testing.T) {
	f := &fakeAPI{resp: []func(http.ResponseWriter){jsonResp(http.StatusOK, Space{ID: "spc_1", Version: 8})}}
	c := newTest(t, f)
	if _, err := c.UpdateSpace(context.Background(), "spc_1", 7, UpdateSpaceInput{Name: "S", DisconnectPolicy: "kill"}); err != nil {
		t.Fatal(err)
	}
	r := f.reqs[0]
	if r.method != http.MethodPut || r.ifMatch != `"7"` || r.idem != "" {
		t.Fatalf("request = %+v", r)
	}
	// Every field of a PUT is sent, empty ones included (the API replaces them).
	if !strings.Contains(r.body, `"goals":""`) {
		t.Fatalf("body = %s", r.body)
	}
}

func TestProblemBecomesAPIError(t *testing.T) {
	f := &fakeAPI{resp: []func(http.ResponseWriter){
		problem(http.StatusUnprocessableEntity, "validation_failed", "invalid input", FieldError{Field: "slug", Message: "must be lowercase"}),
		problem(http.StatusConflict, "precondition_failed", "the space changed"),
		problem(http.StatusNotFound, "not_found", "space not found"),
		problem(http.StatusNotFound, "not_found", "space not found"),
		problem(http.StatusNotFound, "not_found", "space not found"),
		problem(http.StatusNotFound, "not_found", "space not found"),
	}}
	c := newTest(t, f)
	ctx := context.Background()
	_, err := c.CreatePrompt(ctx, CreatePromptInput{})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "validation_failed" || len(ae.Errors) != 1 || ae.Errors[0].Field != "slug" {
		t.Fatalf("err = %#v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "POST /prompts: 422 validation_failed: invalid input; slug: must be lowercase") {
		t.Fatalf("message = %q", msg)
	}
	_, err = c.UpdateSpace(ctx, "spc_1", 1, UpdateSpaceInput{})
	if !IsConflict(err) || IsNotFound(err) {
		t.Fatalf("conflict: %v", err)
	}
	// A GET 404 is not retried (four queued 404s, one request).
	n := len(f.reqs)
	if _, err := c.GetSpace(ctx, "spc_x"); !IsNotFound(err) {
		t.Fatalf("not found: %v", err)
	}
	if len(f.reqs) != n+1 {
		t.Fatal("a 404 must not be retried")
	}
	// Deleting something already gone succeeds.
	if err := c.PurgeSpace(ctx, "spc_x"); err != nil {
		t.Fatalf("delete of a missing object: %v", err)
	}
}

func TestNonProblemErrorStatus(t *testing.T) {
	f := &fakeAPI{resp: []func(http.ResponseWriter){func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html>no</html>"))
	}}}
	c := newTest(t, f)
	_, err := c.GetAccount(context.Background(), "acc_1")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden || ae.Title != "Forbidden" {
		t.Fatalf("err = %#v", err)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var hit bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer other.Close()
	f := &fakeAPI{resp: []func(http.ResponseWriter){func(w http.ResponseWriter) {
		w.Header().Set("Location", other.URL+"/steal")
		w.WriteHeader(http.StatusFound)
	}}}
	c := newTest(t, f)
	if _, err := c.GetSpace(context.Background(), "spc_1"); err == nil {
		t.Fatal("a redirect must be an error")
	}
	if hit {
		t.Fatal("the bearer token was forwarded to the redirect target")
	}
}

func TestLookupsByListing(t *testing.T) {
	f := &fakeAPI{resp: []func(http.ResponseWriter){
		jsonResp(http.StatusOK, map[string]any{"items": []Profile{{ID: "hpf_1", Slug: "fast"}, {ID: "hpf_2", Slug: "slow"}}}),
		jsonResp(http.StatusOK, map[string]any{"items": []Profile{{ID: "hpf_1", Slug: "fast"}}}),
		jsonResp(http.StatusOK, map[string]any{"items": []SpaceRole{{SpaceID: "spc_1", Name: "worker"}}}),
	}}
	c := newTest(t, f)
	ctx := context.Background()
	p, err := c.GetProfile(ctx, "slow")
	if err != nil || p.ID != "hpf_2" {
		t.Fatalf("profile = %+v, %v", p, err)
	}
	if _, err := c.GetProfile(ctx, "hpf_9"); !IsNotFound(err) {
		t.Fatalf("missing profile: %v", err)
	}
	if _, err := c.GetSpaceRole(ctx, "spc_1", "control"); !IsNotFound(err) {
		t.Fatalf("missing role: %v", err)
	}
}

func TestSetSpaceAccountsSendsOnlyBindingFields(t *testing.T) {
	f := &fakeAPI{resp: []func(http.ResponseWriter){jsonResp(http.StatusOK, map[string]any{"items": []AccountBinding{}})}}
	c := newTest(t, f)
	if _, err := c.SetSpaceAccounts(context.Background(), "spc_1", nil); err != nil {
		t.Fatal(err)
	}
	if f.reqs[0].body != `{"items":[]}`+"\n" && f.reqs[0].body != `{"items":[]}` {
		t.Fatalf("body = %q (an empty set must be an empty array, not null)", f.reqs[0].body)
	}
}

func TestDraftHelpers(t *testing.T) {
	p := &Prompt{Versions: []*PromptVersion{{ID: "d", State: "draft"}, {ID: "v2", State: "published", Number: 2}, {ID: "v1", State: "published", Number: 1}}}
	if p.Draft().ID != "d" || p.LatestPublished().ID != "v2" {
		t.Fatal("prompt helpers")
	}
	tmpl := &EventTemplate{Versions: []*EventTemplateVersion{{ID: "a", Status: "published", Version: 1}, {ID: "b", Status: "draft", Version: 2}}}
	if tmpl.Draft().ID != "b" || tmpl.LatestPublished().ID != "a" {
		t.Fatal("template helpers")
	}
	if (&Skill{}).LatestPublished() != nil || (&HumanType{}).Draft() != nil {
		t.Fatal("empty helpers")
	}
}
