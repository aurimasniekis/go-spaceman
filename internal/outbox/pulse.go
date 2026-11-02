package outbox

import (
	"context"
	"log/slog"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"
	"github.com/nats-io/nats.go"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
)

// PulseConfig says how the relay reaches Pulse: the deployment URL (nats:// or tls://), the client .creds file Pulse
// issued (user JWT and nkey seed; read by the SDK at connect time, never stored by SpaceMan) and an optional CA file.
type PulseConfig struct {
	URL       string
	CredsFile string
	CAFile    string
	Source    string
}

// PulseDialer connects with the go-pulse SDK (docs/contracts/pulse.md). The client only publishes; its Pulse grant
// needs publish:evt.spaceman.>.
func PulseDialer(c PulseConfig, log *slog.Logger) Dialer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = obs.Component(log, "outbox")
	return func(context.Context) (Publisher, func(), error) {
		opts := []pulse.Option{pulse.WithCredsFile(c.CredsFile), pulse.WithSource(c.Source), pulse.WithName("spaceman-publisher"),
			pulse.WithNATSOptions(
				nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
					if err != nil { // nil: closed on shutdown
						log.Warn("event relay disconnected from Pulse; events wait in the outbox", obs.Err(err))
					}
				}),
				nats.ReconnectHandler(func(*nats.Conn) { log.Info("event relay reconnected to Pulse") }),
			)}
		if c.CAFile != "" {
			opts = append(opts, pulse.WithCAFile(c.CAFile))
		}
		cl, err := pulse.Connect(c.URL, opts...)
		if err != nil {
			return nil, nil, err
		}
		return cl, cl.Close, nil
	}
}
