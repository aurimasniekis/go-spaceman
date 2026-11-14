package updates

import (
	"context"
	"time"
)

// Options configure a Service.
type Options struct {
	Intake     IntakeOptions
	Webhook    WebhookOptions
	Dispatcher DispatcherOptions
}

// Service is the inbound side as central wires it: one Intake fed by the Webhook (webhook mode) or a Poller (polling
// mode), drained by the Dispatcher.
type Service struct {
	Intake     *Intake
	Webhook    *Webhook
	Dispatcher *Dispatcher
}

// New builds the service; call Run to start the workers.
func New(o Options) *Service {
	if o.Webhook.Metrics == nil {
		o.Webhook.Metrics = o.Intake.Metrics
	}
	in := NewIntake(o.Intake)
	return &Service{Intake: in, Webhook: NewWebhook(in, o.Webhook), Dispatcher: NewDispatcher(o.Dispatcher)}
}

// Run dispatches queued updates until ctx ends. The webhook answers 503 while Run isn't running. It also records the
// webhook's pending refused-secret counts on a timer.
func (s *Service) Run(ctx context.Context) {
	s.Webhook.setRunning(true)
	defer s.Webhook.setRunning(false)
	done := make(chan struct{})
	defer func() { <-done }()
	go func() {
		defer close(done)
		s.Webhook.auditLoop(ctx, auditFlushEvery)
	}()
	s.Intake.Run(ctx, s.Dispatcher.Dispatch)
}

// auditFlushEvery is how often Run looks for refused-secret counts nobody recorded because the refusals stopped.
const auditFlushEvery = rejectAuditEvery / 4

// auditLoop calls flushUnauthorized every interval until ctx ends.
func (w *Webhook) auditLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.flushUnauthorized(ctx)
		}
	}
}

// Poll runs the long-polling mode with bot until ctx ends (the webhook must be off: Webhook.Set(nil), deleteWebhook).
func (s *Service) Poll(ctx context.Context, bot Getter, o PollerOptions) error {
	if o.Metrics == nil {
		o.Metrics = s.Intake.metrics
	}
	if o.Log == nil {
		o.Log = s.Intake.log
	}
	return NewPoller(bot, s.Intake, o).Run(ctx)
}
