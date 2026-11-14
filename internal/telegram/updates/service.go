package updates

import "context"

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

// Run dispatches queued updates until ctx ends. The webhook answers 503 while Run isn't running.
func (s *Service) Run(ctx context.Context) {
	s.Webhook.setRunning(true)
	defer s.Webhook.setRunning(false)
	s.Intake.Run(ctx, s.Dispatcher.Dispatch)
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
