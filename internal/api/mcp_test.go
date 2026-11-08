package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
)

// The external MCP endpoint's guard (ADR-0063 §External endpoint) passes on only a user's bearer API token, marked
// MCPEndpoint on a copy, and refuses browsers, sessions, the root token and internal tokens; each token has its own
// call rate.
func TestMCPEndpointGuard(t *testing.T) {
	t.Parallel()
	var got *auth.Principal
	h := (&Deps{}).mcpEndpoint(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	serve := func(p *auth.Principal, hdr map[string]string) int {
		got = nil
		req := httptest.NewRequest(http.MethodPost, MCPPath, nil)
		if p != nil {
			req = req.WithContext(context.WithValue(req.Context(), principalKey{}, p))
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("CORS header")
		}
		return rec.Code
	}
	bearer := &auth.Principal{User: &auth.User{ID: "usr_1"}, SessionID: "atk_1", TokenID: "atk_1", Method: auth.MethodToken}
	if c := serve(bearer, nil); c != http.StatusNoContent || got == nil || !got.MCPEndpoint || got == bearer || bearer.MCPEndpoint {
		t.Fatalf("bearer: %d %+v", c, got)
	}
	for name, tc := range map[string]struct {
		p    *auth.Principal
		hdr  map[string]string
		code int
	}{
		"no principal":      {nil, nil, http.StatusUnauthorized},
		"origin":            {bearer, map[string]string{"Origin": "https://example.com"}, http.StatusForbidden},
		"sec-fetch-site":    {bearer, map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusForbidden},
		"sec-fetch-mode":    {bearer, map[string]string{"Sec-Fetch-Mode": "cors"}, http.StatusForbidden},
		"session":           {&auth.Principal{User: &auth.User{ID: "usr_1"}, SessionID: "ses_1", Method: auth.MethodPassword}, nil, http.StatusForbidden},
		"root":              {&auth.Principal{RootTokenID: "rtk_1", Method: auth.MethodRoot}, nil, http.StatusForbidden},
		"internal token":    {internalPrincipal(), nil, http.StatusForbidden},
		"purpose on bearer": {&auth.Principal{User: &auth.User{ID: "usr_1"}, TokenID: "atk_2", TokenPurpose: "other"}, nil, http.StatusForbidden},
	} {
		if c := serve(tc.p, tc.hdr); c != tc.code || got != nil {
			t.Errorf("%s: %d, want %d", name, c, tc.code)
		}
	}
	// One JSON-RPC message per request: a batch never reaches the handler, a single message does with its body intact;
	// a protocol version that allows batches is refused.
	post := func(tokenID, body string, hdr map[string]string) (int, string) {
		got = nil
		var seen string
		h := (&Deps{}).mcpEndpoint(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = PrincipalFrom(r.Context())
			b, _ := io.ReadAll(r.Body)
			seen = string(b)
			w.WriteHeader(http.StatusNoContent)
		}))
		req := httptest.NewRequest(http.MethodPost, MCPPath, strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), principalKey{},
			&auth.Principal{User: &auth.User{ID: "usr_1"}, TokenID: tokenID, Method: auth.MethodToken}))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, seen
	}
	single := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for name, body := range map[string]string{"batch": "[" + single + "," + single + "]", "batch after spaces": " \r\n\t[" + single + "]"} {
		if c, _ := post("atk_b", body, nil); c != http.StatusBadRequest || got != nil {
			t.Errorf("%s: %d", name, c)
		}
	}
	if c, seen := post("atk_b", single, map[string]string{"Mcp-Protocol-Version": "2025-06-18"}); c != http.StatusNoContent || seen != single {
		t.Errorf("single message: %d %q", c, seen)
	}
	if c, _ := post("atk_b", single, map[string]string{"Mcp-Protocol-Version": "2025-03-26"}); c != http.StatusBadRequest || got != nil {
		t.Errorf("protocol 2025-03-26: %d", c)
	}

	// The per-token rate: the burst passes, then 429; another token is unaffected.
	other := &auth.Principal{User: &auth.User{ID: "usr_1"}, TokenID: "atk_3", Method: auth.MethodToken}
	limited := false
	for range mcpTokenBurst + 5 {
		if serve(other, nil) == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("no rate limit per token")
	}
	if c := serve(&auth.Principal{User: &auth.User{ID: "usr_1"}, TokenID: "atk_4", Method: auth.MethodToken}, nil); c != http.StatusNoContent {
		t.Fatalf("another token: %d", c)
	}
}
