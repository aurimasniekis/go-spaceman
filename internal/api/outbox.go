package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
)

// OutboxRules are the authorization rules of outbound publication (SM-OBS-004). Its status is global
// administration: only global admins hold system:read.
var OutboxRules = map[string]Rule{
	"get-outbound-events": Global(authz.SystemRead),
}

// RegisterOutbox registers the outbound publication status.
func RegisterOutbox(ob *outbox.Outbox) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		huma.Register(api, huma.Operation{OperationID: "get-outbound-events", Method: http.MethodGet, Path: "/system/outbound-events",
			Summary: "Outbound event publication status",
			Description: "Whether SpaceMan publishes its own events to Pulse, the relay's connection, pending, retrying and dead-lettered events, " +
				"the publication lag (age of the oldest pending event) and recent failures.", Tags: []string{"system"}},
			func(ctx context.Context, _ *struct{}) (*struct{ Body outbox.Status }, error) {
				st, err := ob.Status(ctx)
				if err != nil {
					return nil, d.fail(ctx, "outbox.status", err)
				}
				return &struct{ Body outbox.Status }{Body: st}, nil
			})
	}
}
