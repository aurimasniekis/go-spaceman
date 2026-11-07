package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/schedule"
)

// LimitRules are the authorization rules of the limits model and space schedules (SM-LIMIT-*).
var LimitRules = map[string]Rule{
	"list-limit-definitions": FilteredRead(),
	"get-space-limits":       InSpace(authz.SpaceConfigRead),
	"set-space-limits":       InSpace(authz.SpaceConfigWrite),
	"list-schedules":         InSpace(authz.SpaceConfigRead),
	"get-schedule":           InSpace(authz.SpaceConfigRead),
	"create-schedule":        InSpace(authz.SpaceConfigWrite),
	"update-schedule":        InSpace(authz.SpaceConfigWrite),
	"delete-schedule":        InSpace(authz.SpaceConfigWrite),
	"preview-schedule":       InSpace(authz.SpaceConfigRead),
}

type limitsOut struct {
	ETag string `header:"ETag"`
	Body *limits.Effective
}

// SchedulePath identifies a schedule of a space.
type SchedulePath struct {
	SpaceID    string `path:"space_id" pattern:"^spc_[0-9a-z]{26}$"`
	ScheduleID string `path:"schedule_id" maxLength:"64"`
}

// ScheduleDetail is a schedule with its recent occurrences.
type ScheduleDetail struct {
	*schedule.Schedule
	Occurrences []schedule.Occurrence `json:"occurrences"`
}

type scheduleOut struct {
	ETag string `header:"ETag"`
	Body *schedule.Schedule
}

// RegisterLimits registers the limits and schedule operations.
func RegisterLimits(svc *limits.Service, sched *schedule.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"limits"}
		huma.Register(api, huma.Operation{OperationID: "list-limit-definitions", Method: http.MethodGet, Path: "/limits/definitions",
			Summary: "Every limit: unit, bounds, installation default source, enforcement point and what lowering it does", Tags: tags},
			func(_ context.Context, _ *struct{}) (*struct {
				Body struct {
					Items []limits.Def `json:"items"`
				}
			}, error) {
				out := &struct {
					Body struct {
						Items []limits.Def `json:"items"`
					}
				}{}
				out.Body.Items = limits.Defs
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-space-limits", Method: http.MethodGet, Path: "/spaces/{space_id}/limits",
			Summary:     "A space's effective limits, each with its value and source, and its budget state",
			Description: "Budgets are usage-based: usage arrives late or not at all, so they warn and stop new runs but are never a hard currency limit.", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*limitsOut, error) {
				e, err := svc.Effective(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "limits.get", err)
				}
				return &limitsOut{ETag: ETag(e.Version), Body: e}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "set-space-limits", Method: http.MethodPut, Path: "/spaces/{space_id}/limits",
			Summary: "Replace a space's limit overrides",
			Description: "Keys left out inherit the installation default. If-Match is required once overrides are stored. Lowering a limit never stops " +
				"running work, except the concurrent-runs limit with on_lower=stop, which stops the newest runs beyond it.", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				IfMatch
				Body limits.Input
			}) (*limitsOut, error) {
				v, err := optionalVersion(in.IfMatch)
				if err != nil {
					return nil, d.fail(ctx, "limits.set", err)
				}
				// A retention period deletes data when it is shortened: changing one needs retention:write (admins).
				changed, err := svc.RetentionChanged(ctx, in.SpaceID, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "limits.set", err)
				}
				if p := PrincipalFrom(ctx); changed && (d.Authz == nil || p == nil || !d.Authz.AllowedAll(p.Subjects(), in.SpaceID, authz.RetentionWrite)) {
					return nil, newProblem(http.StatusForbidden, apperr.CodeForbidden, "changing a retention period needs retention:write (space admins)", nil)
				}
				e, err := svc.Set(ctx, Actor(ctx), in.SpaceID, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "limits.set", err)
				}
				return &limitsOut{ETag: ETag(e.Version), Body: e}, nil
			})

		stags := []string{"schedules"}
		huma.Register(api, huma.Operation{OperationID: "list-schedules", Method: http.MethodGet, Path: "/spaces/{space_id}/schedules",
			Summary: "A space's schedules", Tags: stags},
			func(ctx context.Context, in *SpacePath) (*struct {
				Body struct {
					Items []*schedule.Schedule `json:"items"`
				}
			}, error) {
				l, err := sched.List(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "schedules.list", err)
				}
				out := &struct {
					Body struct {
						Items []*schedule.Schedule `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-schedule", Method: http.MethodPost, Path: "/spaces/{space_id}/schedules",
			Summary: "Create a schedule", Description: "It creates an inbox item or a task at each occurrence; the first occurrence is the first one after now.",
			Tags: stags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				SpacePath
				Body schedule.Input
			}) (*scheduleOut, error) {
				s, err := sched.Create(ctx, Actor(ctx), in.SpaceID, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "schedules.create", err)
				}
				return &scheduleOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "preview-schedule", Method: http.MethodPost, Path: "/spaces/{space_id}/schedules/preview",
			Summary: "Validate a schedule's timing and list its next occurrences", Tags: stags},
			func(ctx context.Context, in *struct {
				SpacePath
				Body schedule.Input
			}) (*struct {
				Body struct {
					Occurrences []time.Time `json:"occurrences"`
				}
			}, error) {
				l, err := sched.Preview(in.Body, 5)
				if err != nil {
					return nil, d.fail(ctx, "schedules.preview", err)
				}
				out := &struct {
					Body struct {
						Occurrences []time.Time `json:"occurrences"`
					}
				}{}
				out.Body.Occurrences = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-schedule", Method: http.MethodGet, Path: "/spaces/{space_id}/schedules/{schedule_id}",
			Summary: "A schedule with its recent occurrences", Tags: stags},
			func(ctx context.Context, in *SchedulePath) (*struct {
				ETag string `header:"ETag"`
				Body ScheduleDetail
			}, error) {
				s, err := sched.Get(ctx, in.SpaceID, in.ScheduleID)
				if err != nil {
					return nil, d.fail(ctx, "schedules.get", err)
				}
				occ, err := sched.Occurrences(ctx, in.SpaceID, in.ScheduleID, 50)
				if err != nil {
					return nil, d.fail(ctx, "schedules.get", err)
				}
				return &struct {
					ETag string `header:"ETag"`
					Body ScheduleDetail
				}{ETag: ETag(s.Version), Body: ScheduleDetail{Schedule: s, Occurrences: occ}}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "update-schedule", Method: http.MethodPut, Path: "/spaces/{space_id}/schedules/{schedule_id}",
			Summary: "Replace a schedule", Description: "A changed timing restarts it from now; occurrences before the change are not treated as missed.", Tags: stags},
			func(ctx context.Context, in *struct {
				SchedulePath
				IfMatch
				Body schedule.Input
			}) (*scheduleOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "schedules.update", err)
				}
				s, err := sched.Update(ctx, Actor(ctx), in.SpaceID, in.ScheduleID, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "schedules.update", err)
				}
				return &scheduleOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-schedule", Method: http.MethodDelete, Path: "/spaces/{space_id}/schedules/{schedule_id}",
			Summary: "Delete a schedule (what it created stays)", Tags: stags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *SchedulePath) (*struct{}, error) {
				if err := sched.Delete(ctx, Actor(ctx), in.SpaceID, in.ScheduleID); err != nil {
					return nil, d.fail(ctx, "schedules.delete", err)
				}
				return &struct{}{}, nil
			})
	}
}
