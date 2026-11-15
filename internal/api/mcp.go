package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"golang.org/x/time/rate"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
)

// MCPPath is the external MCP endpoint (ADR-0063 §External endpoint, SM-MCP-009): SpaceMan's tools for an operator's
// own MCP client, authenticated with the operator's API token as a bearer.
const MCPPath = Prefix + "/mcp"

// Calls per token through the MCP endpoint: 5 per second, bursts of 50. Failed bearer authentications are limited per
// address before this, as for every API request. The limit counts HTTP requests, so it counts calls only because a
// request carries one JSON-RPC message: batches are refused (mcpRefuseBatch).
const (
	mcpTokenRate  = 5
	mcpTokenBurst = 50
)

// mcpMinProtocolVersion is the oldest MCP protocol version the endpoint accepts in the Mcp-Protocol-Version header:
// 2025-06-18 dropped JSON-RPC batching, which older versions allow. A request without the header is accepted (a
// client's initialize may not send it), and batches are refused whatever the version.
const mcpMinProtocolVersion = "2025-06-18"

// mcpEndpoint guards the external MCP endpoint, which runs behind the API's own middleware (security headers, request
// IDs, the body limit, bearer authentication and its failed-attempt limit). It serves only an operator's MCP client:
//
//   - a request a browser sends (Origin or Sec-Fetch-* headers) is refused, and no CORS headers are ever sent, so no
//     web page can call it with the operator's credentials;
//   - only a bearer API token: a session cookie and the root token are refused; internal tokens (purpose
//     spaceman_mcp) never authenticate as bearers, and are refused here as well;
//   - each token gets its own call rate, and a request carries one JSON-RPC message: a batch (a body that starts with
//     "[") is refused before anything in it runs, as is a protocol version older than mcpMinProtocolVersion.
//
// The principal passed on is a copy marked MCPEndpoint (auth.Principal.ViaMCP), which gives the call the MCP rails in
// authorizeOps and the launch-kind check, and the "mcp_endpoint" channel in the audit.
func (d *Deps) mcpEndpoint(next http.Handler) http.Handler {
	perToken := newIPLimiter(rate.Limit(mcpTokenRate), mcpTokenBurst)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("Sec-Fetch-Mode") != "" {
			writeProblem(w, newProblem(http.StatusForbidden, apperr.CodeForbidden, "the MCP endpoint does not serve browsers", nil))
			return
		}
		p := PrincipalFrom(r.Context())
		switch {
		case p == nil:
			w.Header().Set("WWW-Authenticate", `Bearer realm="spaceman"`)
			writeProblem(w, errUnauthenticated())
			return
		case p.TokenID == "" || p.User == nil || p.IsRoot():
			writeProblem(w, newProblem(http.StatusForbidden, apperr.CodeForbidden,
				"the MCP endpoint needs a user's API token (Authorization: Bearer spaceman_at_…)", nil))
			return
		case p.Internal() || p.TokenPurpose != "":
			writeProblem(w, newProblem(http.StatusForbidden, apperr.CodeForbidden, "an internal token cannot use the MCP endpoint", nil))
			return
		}
		if !perToken.allow(p.TokenID) {
			writeProblem(w, newProblem(http.StatusTooManyRequests, apperr.CodeRateLimited, "too many MCP requests for this token; slow down", nil))
			return
		}
		if v := r.Header.Get("Mcp-Protocol-Version"); v != "" && v < mcpMinProtocolVersion {
			writeProblem(w, newProblem(http.StatusBadRequest, apperr.CodeInvalid,
				"the MCP endpoint needs protocol version "+mcpMinProtocolVersion+" or later", nil))
			return
		}
		if r.Method == http.MethodPost {
			if prob := mcpRefuseBatch(r); prob != nil {
				writeProblem(w, prob)
				return
			}
		}
		cp := *p
		cp.MCPEndpoint = true
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, &cp)))
	})
}

// mcpRefuseBatch reads the request body (bounded by the API's body limit) and refuses a JSON-RPC batch: one request
// could otherwise carry any number of tool calls past the per-token rate. The body is put back for the MCP handler.
func mcpRefuseBatch(r *http.Request) *Problem {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return newProblem(http.StatusRequestEntityTooLarge, "", "the request body is too large", nil)
		}
		return newProblem(http.StatusBadRequest, apperr.CodeInvalid, "the request body could not be read", nil)
	}
	if t := bytes.TrimLeft(body, " \t\r\n"); len(t) > 0 && t[0] == '[' {
		return newProblem(http.StatusBadRequest, apperr.CodeInvalid,
			"the MCP endpoint does not accept JSON-RPC batches: send one message per request", nil)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return nil
}
