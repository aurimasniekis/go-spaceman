package app

import (
	"context"
	"net/http"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// PublicServer exposes the public listener's server (timeouts included) to the external tests.
func PublicServer(c *Central, ctx context.Context) *http.Server { return c.publicServer(ctx) }

// UploadSpooledArtifacts runs the startup pass over the spool synchronously.
func UploadSpooledArtifacts(c *Central, ctx context.Context) { c.uploadSpooledArtifacts(ctx) }

// NotifyRunDialog runs the notice a node's blocked dialog raises (ADR-0102).
func NotifyRunDialog(c *Central, ctx context.Context, r db.Run, d fleet.RunDialog) {
	c.notifyRunDialog(ctx, r, d)
}
