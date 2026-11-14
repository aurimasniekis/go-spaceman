package settings

import (
	"log/slog"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// Updates returns the per-link inbound limits for the dispatcher's limiter (the global updates-per-second limit goes
// to the Intake's rate).
func (l InboundLimits) Updates() updates.InboundLimits {
	return updates.InboundLimits{ActionsPerMinute: int(l.ActionsPerMinute), ActionsBurst: int(l.ActionsBurst),
		InboxItemsPerHour: int(l.InboxItemsPerHour), DownloadsPerHour: int(l.DownloadsPerHour)}
}

// LimitSetter is the part of *sender.Sender BindSender needs.
type LimitSetter interface {
	SetLimits(sender.Limits) error
}

// BindSender applies send_limits to s now and after every change, without a restart. The values were validated
// against the sender's bounds when they were stored; a refusal anyway (bounds changed in a later release) is logged and
// the sender keeps its previous limits.
func (svc *Service) BindSender(s LimitSetter) {
	svc.OnChange(func(st Settings) {
		if err := s.SetLimits(st.SendLimits.Sender()); err != nil {
			svc.log.Warn("telegram: the sender refused the send limits; keeping the previous ones", slog.String("error", err.Error()))
		}
	})
}
