package app

import (
	"context"
	"net/http"
)

// PublicServer exposes the public listener's server (timeouts included) to the external tests.
func PublicServer(c *Central, ctx context.Context) *http.Server { return c.publicServer(ctx) }
