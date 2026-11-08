package api

import (
	"context"
	"net/http"

	"golang.org/x/time/rate"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
)

// MCPPath is the external MCP endpoint (ADR-0063 §External endpoint, SM-MCP-009): SpaceMan's tools for an operator's
// own MCP client, authenticated with the operator's API token as a bearer.
const MCPPath = Prefix + "/mcp"

// Calls per token through the MCP endpoint: 5 per second, bursts of 50. Failed bearer authentications are limited per
// address before this, as for every API request.
const (
	mcpTokenRate  = 5
	mcpTokenBurst = 50
)

// mcpEndpoint guards the external MCP endpoint, which runs behind the API's own middleware (security headers, request
// IDs, the body limit, bearer authentication and its failed-attempt limit). It serves only an operator's MCP client:
//
//   - a request a browser sends (Origin or Sec-Fetch-* headers) is refused, and no CORS headers are ever sent, so no
//     web page can call it with the operator's credentials;
//   - only a bearer API token: a session cookie and the root token are refused; internal tokens (purpose
//     spaceman_mcp) never authenticate as bearers, and are refused here as well;
//   - each token gets its own call rate.
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
		cp := *p
		cp.MCPEndpoint = true
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, &cp)))
	})
}
