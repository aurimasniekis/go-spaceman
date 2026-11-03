package events

import (
	"context"

	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Source is an inbound event source (SM-ARCH-003): the transport that receives one subscription's events on a
// connection and hands each to the manager's processing path (loop guard, rate limit, filtering, deduplication,
// quarantine, the inbox). The manager picks the source for a subscription's delivery mode. Two are compiled in, both
// over the connection's go-pulse client (docs/contracts/pulse.md):
//
//   - DeliveryDurable: a JetStream durable consumer through the SDK; processing decides the acknowledgement
//     (ack, terminate, or negative acknowledgement and redelivery);
//   - DeliveryCore: a plain core NATS subscription (ADR-0042); nothing acknowledges, nothing redelivers.
//
// There is no other inbound source (no direct webhooks, SM-EVT-019). Sources are compiled in, never loaded.
type Source interface {
	// Delivery is the subscription delivery mode the source serves.
	Delivery() string
	// Start begins receiving the subscription's events on connection c and returns the running subscription; the
	// manager stops it (halt) when the subscription, its connection or its cursor changes. key identifies the
	// configuration the run was started with.
	Start(ctx context.Context, c *connRun, s db.EventSubscription, key string) (*subRun, error)
}

// durableSource is the JetStream durable consumer source (Manager.start).
type durableSource struct{ m *Manager }

func (d durableSource) Delivery() string { return DeliveryDurable }

func (d durableSource) Start(ctx context.Context, c *connRun, s db.EventSubscription, key string) (*subRun, error) {
	return d.m.start(ctx, c, s, key)
}

// coreSource is the core NATS source (Manager.startCore).
type coreSource struct{ m *Manager }

func (c coreSource) Delivery() string { return DeliveryCore }

func (c coreSource) Start(ctx context.Context, cr *connRun, s db.EventSubscription, key string) (*subRun, error) {
	return c.m.startCore(ctx, cr, s, key)
}
