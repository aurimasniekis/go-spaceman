package cli

import "sort"

// Excluded are the API operations without a command, with the reason (ADR-0040). Every other operation of the
// build's OpenAPI document must be reachable from a command; TestEveryOperationHasACommand fails otherwise, so a new
// operation needs a command or an entry here.
var Excluded = map[string]string{ //nolint:gosec // G101 false positive: operation IDs and reasons, not credentials
	"login":           "interactive sign-in: it creates a browser session (cookie and CSRF token); the CLI authenticates with an API token",
	"logout":          "ends the browser session it is called from; an API token has none (revoke tokens with `spaceman token revoke`)",
	"verify-mfa":      "second step of an interactive sign-in, on the session that login created",
	"reauth-password": "step-up of a browser session; API tokens are not subject to re-authentication",
	"reauth-totp":     "step-up of an SSO browser session; API tokens are not subject to re-authentication",
	"recovery-login":  "break-glass sign-in that creates a browser session; the recovery token is used on the sign-in page",
	"start-oidc":      "browser redirect to the identity provider (sign-in, step-up, link or test login)",
	"oidc-callback":   "the identity provider's browser redirect back to central",
	"reset-password": "public and token-less: central accepts an anonymous state-changing request only from its own web origin " +
		"(cross-site protection), so the user redeems the reset token on the sign-in page; `spaceman user reset-token` issues it",
}

// ExcludedIDs returns the excluded operation IDs, sorted.
func ExcludedIDs() []string {
	out := make([]string, 0, len(Excluded))
	for id := range Excluded {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
