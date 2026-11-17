package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/control"
)

// WatchdogRules is the authorization rule of the watchdog state: read-only, with the permission that reads runs.
var WatchdogRules = map[string]Rule{
	"get-space-watchdog": InSpace(authz.RunRead),
}

// SpaceWatchdog is a space's control watchdog state (ADR-0099) as its last pass saw it. It holds counts, states and
// times only: no titles, identifiers of work or message content.
type SpaceWatchdog struct {
	Observed     bool            `json:"observed" doc:"false until the watchdog has passed over the space since central started; the rest is then empty"`
	Guard        string          `json:"guard,omitempty" doc:"Why the last pass left the space alone (off, space_not_active, no_live_control, taken_over, usage_limit, waiting_for_person, busy, no_backlog, recently_active); empty while the watchdog is reminding"`
	Status       string          `json:"status" enum:"off,idle,nudging,escalated" doc:"off: the watchdog is off or the space not active; idle: nothing is being nudged; nudging: control was reminded and the backlog still waits; escalated: the reminders went unanswered and the space's people were told"`
	Backlog      control.Backlog `json:"backlog"`
	ControlRun   string          `json:"control_run,omitempty"`
	Nudges       int             `json:"nudges" doc:"Reminders sent in the current stretch of the same backlog"`
	LastActivity *time.Time      `json:"last_activity,omitempty"`
	LastNudge    *time.Time      `json:"last_nudge,omitempty"`
	NextNudge    *time.Time      `json:"next_nudge,omitempty"`
	Escalated    bool            `json:"escalated"`
	CheckedAt    *time.Time      `json:"checked_at,omitempty"`
}

// watchdogView turns the watchdog's state into the API's view.
func watchdogView(st control.WatchState, ok bool) SpaceWatchdog {
	if !ok {
		return SpaceWatchdog{Status: "off"}
	}
	opt := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		return &t
	}
	v := SpaceWatchdog{Observed: true, Guard: st.Guard, Backlog: st.Backlog, ControlRun: st.ControlRun, Nudges: st.Nudges,
		LastActivity: opt(st.LastActivity), LastNudge: opt(st.LastNudge), NextNudge: opt(st.NextNudge), Escalated: st.Escalated, CheckedAt: opt(st.CheckedAt)}
	switch {
	case st.Guard == "off" || st.Guard == "space_not_active":
		v.Status = "off"
	case st.Escalated:
		v.Status = "escalated"
	case st.Nudges > 0:
		v.Status = "nudging"
	default:
		v.Status = "idle"
	}
	return v
}

// RegisterWatchdog registers the read-only watchdog state (SM-CTRL-008).
func RegisterWatchdog(w *control.Watchdog) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		huma.Register(api, huma.Operation{OperationID: "get-space-watchdog", Method: http.MethodGet, Path: "/spaces/{space_id}/watchdog",
			Summary: "A space's control watchdog state",
			Description: "What the control watchdog (ADR-0099) saw at its last pass over the space: why it left control alone, or how many reminders it " +
				"sent and whether it escalated, with the waiting work as counts. Read-only; the watchdog is configured with the " +
				"control_watchdog_minutes, control_watchdog_nudges and control_watchdog_restart limits.", Tags: []string{"spaces"}},
			func(ctx context.Context, in *SpacePath) (*struct{ Body SpaceWatchdog }, error) {
				st, ok := control.WatchState{}, false
				if w != nil {
					st, ok = w.State(in.SpaceID)
				}
				return &struct{ Body SpaceWatchdog }{Body: watchdogView(st, ok)}, nil
			})
	}
}
