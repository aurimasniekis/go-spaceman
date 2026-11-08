package spacemanmcp

import (
	"net/http"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
)

// SetEndpointPrincipal replaces how the endpoint reads its caller (the API's guard sets it in production).
func SetEndpointPrincipal(e *Endpoint, f func(*http.Request) *auth.Principal) { e.principal = f }
