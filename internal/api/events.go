package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/events"
	"github.com/aurimasniekis/go-spaceman/internal/events/template"
)

// EventRules are the authorization rules of connections, templates and subscriptions (SM-EVT-018).
var EventRules = map[string]Rule{
	// Any signed-in person can see which connections exist (never their credentials) and read the catalog.
	"list-event-connections":  {Session: true},
	"get-event-connection":    {Session: true},
	"create-event-connection": StepUp(authz.ConnectionWrite),
	"update-event-connection": StepUp(authz.ConnectionWrite),
	"delete-event-connection": StepUp(authz.ConnectionWrite),
	"list-event-templates":    {Session: true},
	"get-event-template":      {Session: true},
	"preview-event-template":  {Session: true},
	"create-event-template":   Global(authz.TemplateWrite),
	"save-template-draft":     Global(authz.TemplateWrite),
	"publish-template":        Global(authz.TemplateWrite),

	"list-subscriptions":         InSpace(authz.SubscriptionRead),
	"get-subscription":           InSpace(authz.SubscriptionRead),
	"create-subscription":        InSpace(authz.SubscriptionWrite),
	"update-subscription":        InSpace(authz.SubscriptionWrite),
	"delete-subscription":        InSpace(authz.SubscriptionWrite),
	"list-subscription-delivery": InSpace(authz.SubscriptionRead),
	"list-quarantine":            InSpace(authz.SubscriptionRead),
	"replay-subscription":        InSpace(authz.ReplayExecute),
	"list-subscription-replays":  InSpace(authz.SubscriptionRead),
	"resolve-quarantine":         InSpace(authz.SubscriptionWrite),
}

// ConnectionPath identifies a connection.
type ConnectionPath struct {
	ConnectionID string `path:"connection_id" maxLength:"64"`
}

// SubscriptionPath identifies a subscription of a space.
type SubscriptionPath struct {
	SpaceID        string `path:"space_id" maxLength:"64"`
	SubscriptionID string `path:"subscription_id" maxLength:"64"`
}

// ConnectionBody creates or updates a connection. Creds are write-only: an update without them keeps the stored ones.
type ConnectionBody struct {
	Name    string `json:"name" maxLength:"63"`
	URL     string `json:"url" maxLength:"500" doc:"nats://host:port or tls://host:port"`
	Creds   string `json:"creds,omitempty" maxLength:"16384" doc:"The .creds file content issued by Pulse (write-only)"`
	CAPEM   string `json:"ca_pem,omitempty" maxLength:"16384"`
	Stream  string `json:"stream,omitempty" maxLength:"64" doc:"Default EVENTS"`
	Enabled bool   `json:"enabled"`
}

func (b ConnectionBody) input() events.ConnectionInput {
	return events.ConnectionInput{Name: b.Name, URL: b.URL, Creds: b.Creds, CAPEM: b.CAPEM, Stream: b.Stream, Enabled: b.Enabled}
}

// PreviewInput renders a definition against a sample event without storing anything.
type PreviewInput struct {
	Definition template.Definition `json:"definition"`
	Vars       map[string]string   `json:"vars,omitempty"`
	Event      SampleEvent         `json:"event"`
}

// SampleEvent is a test event.
type SampleEvent struct {
	ID      string          `json:"id,omitempty" maxLength:"64"`
	Subject string          `json:"subject" maxLength:"256"`
	Type    string          `json:"type,omitempty" maxLength:"256"`
	Source  string          `json:"source,omitempty" maxLength:"256"`
	Time    *time.Time      `json:"time,omitempty"`
	Data    json.RawMessage `json:"data"`
}

// PreviewOut is what a subscription would do with the sample.
type PreviewOut struct {
	Subject      string `json:"subject" doc:"The effective filter with these variables"`
	SubjectMatch bool   `json:"subject_match" doc:"Whether the sample's subject matches the filter"`
	Outcome      string `json:"outcome" enum:"accepted,filtered,quarantined"`
	Reason       string `json:"reason,omitempty"`
	Title        string `json:"title,omitempty"`
	Body         string `json:"body,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	Correlation  string `json:"correlation,omitempty"`
	Urgency      string `json:"urgency,omitempty"`
}

// SubscriptionBody creates a subscription.
type SubscriptionBody struct {
	Name              string               `json:"name" maxLength:"80"`
	ConnectionID      string               `json:"connection_id" maxLength:"64"`
	TemplateVersionID string               `json:"template_version_id,omitempty" maxLength:"64"`
	Follow            string               `json:"follow,omitempty" enum:"pinned,latest"`
	Custom            *template.Definition `json:"custom,omitempty"`
	Vars              map[string]string    `json:"vars,omitempty"`
	Start             *events.Cursor       `json:"start,omitempty" doc:"Where the consumer starts; default new (no history). Core subscriptions accept only new."`
	Delivery          string               `json:"delivery,omitempty" enum:"durable,core" doc:"durable (default): a JetStream durable consumer, acknowledged after the inbox commit, redelivered and replayable. core: a plain NATS Core subscription for sources that publish without JetStream; NON-DURABLE: no replay, no redelivery, no cursor, and messages published while SpaceMan is disconnected are lost. Fixed at creation."`
}

// SubscriptionPatch updates a subscription.
type SubscriptionPatch struct {
	Name              string               `json:"name,omitempty" maxLength:"80"`
	Enabled           bool                 `json:"enabled"`
	TemplateVersionID string               `json:"template_version_id,omitempty" maxLength:"64"`
	Follow            string               `json:"follow,omitempty" enum:"pinned,latest"`
	Custom            *template.Definition `json:"custom,omitempty"`
	Vars              map[string]string    `json:"vars,omitempty"`
	Start             *events.Cursor       `json:"start,omitempty" doc:"Recreate the consumer at this position (required when the subject changes). Anything but new is a replay. Refused for core subscriptions, which have no cursor."`
}

type subOut struct {
	ETag string `header:"ETag"`
	Body *events.Subscription
}

func matchSubject(filter, subject string) bool {
	f, s := splitDots(filter), splitDots(subject)
	for i, t := range f {
		if t == ">" {
			return len(s) > i
		}
		if i >= len(s) || (t != "*" && t != s[i]) {
			return false
		}
	}
	return len(f) == len(s)
}

func splitDots(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// RegisterEvents registers the event operations.
func RegisterEvents(svc *events.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"events"}
		huma.Register(api, huma.Operation{OperationID: "list-event-connections", Method: http.MethodGet, Path: "/event-connections", Summary: "List Pulse connections", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct {
				Body struct {
					Items []*events.Connection `json:"items"`
				}
			}, error) {
				l, err := svc.ListConnections(ctx)
				if err != nil {
					return nil, d.fail(ctx, "events.connections", err)
				}
				out := &struct {
					Body struct {
						Items []*events.Connection `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-event-connection", Method: http.MethodGet, Path: "/event-connections/{connection_id}",
			Summary: "A Pulse connection (never its credentials)", Tags: tags},
			func(ctx context.Context, in *ConnectionPath) (*struct{ Body *events.Connection }, error) {
				c, err := svc.GetConnection(ctx, in.ConnectionID)
				if err != nil {
					return nil, d.fail(ctx, "events.connection_get", err)
				}
				return &struct{ Body *events.Connection }{Body: c}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-event-connection", Method: http.MethodPost, Path: "/event-connections", Summary: "Add a Pulse connection",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct{ Body ConnectionBody }) (*struct{ Body *events.Connection }, error) {
				c, err := svc.CreateConnection(ctx, Actor(ctx), in.Body.input())
				if err != nil {
					return nil, d.fail(ctx, "events.connection_create", err)
				}
				return &struct{ Body *events.Connection }{Body: c}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "update-event-connection", Method: http.MethodPut, Path: "/event-connections/{connection_id}",
			Summary: "Update a Pulse connection", Tags: tags},
			func(ctx context.Context, in *struct {
				ConnectionPath
				IfMatch
				Body ConnectionBody
			}) (*struct{ Body *events.Connection }, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "events.connection_update", err)
				}
				c, err := svc.UpdateConnection(ctx, Actor(ctx), in.ConnectionID, v, in.Body.input())
				if err != nil {
					return nil, d.fail(ctx, "events.connection_update", err)
				}
				return &struct{ Body *events.Connection }{Body: c}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-event-connection", Method: http.MethodDelete, Path: "/event-connections/{connection_id}",
			Summary: "Delete an unused Pulse connection", Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *ConnectionPath) (*struct{}, error) {
				if err := svc.DeleteConnection(ctx, Actor(ctx), in.ConnectionID); err != nil {
					return nil, d.fail(ctx, "events.connection_delete", err)
				}
				return &struct{}{}, nil
			})

		huma.Register(api, huma.Operation{OperationID: "list-event-templates", Method: http.MethodGet, Path: "/event-templates", Summary: "The event template catalog",
			Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct {
				Body struct {
					Items []*events.Template `json:"items"`
				}
			}, error) {
				l, err := svc.ListTemplates(ctx)
				if err != nil {
					return nil, d.fail(ctx, "events.templates", err)
				}
				out := &struct {
					Body struct {
						Items []*events.Template `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-event-template", Method: http.MethodGet, Path: "/event-templates/{template_id}", Summary: "A template with its versions",
			Tags: tags},
			func(ctx context.Context, in *struct {
				TemplateID string `path:"template_id" maxLength:"64"`
			}) (*struct{ Body *events.Template }, error) {
				t, err := svc.GetTemplate(ctx, in.TemplateID)
				if err != nil {
					return nil, d.fail(ctx, "events.template", err)
				}
				return &struct{ Body *events.Template }{Body: t}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-event-template", Method: http.MethodPost, Path: "/event-templates", Summary: "Add a template (as a draft)",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				Body struct {
					Slug        string              `json:"slug" maxLength:"63"`
					Name        string              `json:"name" maxLength:"120"`
					Description string              `json:"description,omitempty" maxLength:"2000"`
					Definition  template.Definition `json:"definition"`
				}
			}) (*struct{ Body *events.Template }, error) {
				t, err := svc.CreateTemplate(ctx, Actor(ctx), in.Body.Slug, in.Body.Name, in.Body.Description, in.Body.Definition)
				if err != nil {
					return nil, d.fail(ctx, "events.template_create", err)
				}
				return &struct{ Body *events.Template }{Body: t}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "save-template-draft", Method: http.MethodPut, Path: "/event-templates/{template_id}/draft",
			Summary: "Save the template's draft version", Tags: tags},
			func(ctx context.Context, in *struct {
				TemplateID string `path:"template_id" maxLength:"64"`
				Body       struct {
					Definition template.Definition `json:"definition"`
					Notes      string              `json:"notes,omitempty" maxLength:"2000"`
				}
			}) (*struct{ Body *events.TemplateVersion }, error) {
				v, err := svc.SaveDraft(ctx, Actor(ctx), in.TemplateID, in.Body.Definition, in.Body.Notes)
				if err != nil {
					return nil, d.fail(ctx, "events.template_draft", err)
				}
				return &struct{ Body *events.TemplateVersion }{Body: v}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "publish-template", Method: http.MethodPost, Path: "/event-template-versions/{version_id}/publish",
			Summary: "Publish a draft (it becomes immutable)", Description: "Subscriptions following the template move to it only when it keeps their subject; the others are listed as held.",
			Tags: tags},
			func(ctx context.Context, in *struct {
				VersionID string `path:"version_id" maxLength:"64"`
			}) (*struct {
				Body struct {
					Version *events.TemplateVersion `json:"version"`
					Held    []string                `json:"held_subscriptions"`
				}
			}, error) {
				v, held, err := svc.Publish(ctx, Actor(ctx), in.VersionID)
				if err != nil {
					return nil, d.fail(ctx, "events.template_publish", err)
				}
				out := &struct {
					Body struct {
						Version *events.TemplateVersion `json:"version"`
						Held    []string                `json:"held_subscriptions"`
					}
				}{}
				out.Body.Version, out.Body.Held = v, append([]string{}, held...)
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "preview-event-template", Method: http.MethodPost, Path: "/event-templates/preview",
			Summary: "Test a definition against a sample event", Description: "Nothing is stored or subscribed.", Tags: tags},
			func(ctx context.Context, in *struct{ Body PreviewInput }) (*struct{ Body PreviewOut }, error) {
				b, err := in.Body.Definition.Bind(in.Body.Vars)
				if err != nil {
					return nil, d.fail(ctx, "events.preview", apperr.New(apperr.CodeInvalid, "%v", err))
				}
				ev := template.Event{ID: in.Body.Event.ID, Subject: in.Body.Event.Subject, Type: in.Body.Event.Type, Source: in.Body.Event.Source,
					ContentType: "application/json", Data: in.Body.Event.Data}
				if in.Body.Event.Time != nil {
					ev.Time = *in.Body.Event.Time
				}
				out := PreviewOut{Subject: b.Subject, SubjectMatch: matchSubject(b.Subject, ev.Subject)}
				r, err := b.Apply(ev)
				switch {
				case err != nil:
					out.Outcome, out.Reason = "quarantined", err.Error()
				case !r.Accepted:
					out.Outcome, out.Reason = "filtered", r.SkipReason
				default:
					out.Outcome, out.Title, out.Body, out.Instructions, out.Correlation, out.Urgency = "accepted", r.Title, r.Body, r.Instructions, r.Correlation, r.Urgency
				}
				return &struct{ Body PreviewOut }{Body: out}, nil
			})

		subTags := []string{"subscriptions"}
		huma.Register(api, huma.Operation{OperationID: "list-subscriptions", Method: http.MethodGet, Path: "/spaces/{space_id}/subscriptions",
			Summary: "A space's event subscriptions", Tags: subTags},
			func(ctx context.Context, in *SpacePath) (*struct {
				Body struct {
					Items []*events.Subscription `json:"items"`
				}
			}, error) {
				l, err := svc.ListSubscriptions(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "events.subscriptions", err)
				}
				out := &struct {
					Body struct {
						Items []*events.Subscription `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-subscription", Method: http.MethodGet, Path: "/spaces/{space_id}/subscriptions/{subscription_id}",
			Summary: "A subscription", Tags: subTags},
			func(ctx context.Context, in *SubscriptionPath) (*subOut, error) {
				s, err := svc.GetSubscription(ctx, in.SpaceID, in.SubscriptionID)
				if err != nil {
					return nil, d.fail(ctx, "events.subscription", err)
				}
				return &subOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-subscription", Method: http.MethodPost, Path: "/spaces/{space_id}/subscriptions",
			Summary: "Subscribe a space to events", Description: "Validated and bound before activation. The response shows the Pulse grant the subscription needs.",
			Tags: subTags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				SpacePath
				Body SubscriptionBody
			}) (*subOut, error) {
				start := events.Cursor{}
				if in.Body.Start != nil {
					start = *in.Body.Start
					if err := d.requireReplay(ctx, in.SpaceID, start.Policy); err != nil {
						return nil, err
					}
				}
				s, err := svc.CreateSubscription(ctx, Actor(ctx), in.SpaceID, events.SubscriptionInput{Name: in.Body.Name, ConnectionID: in.Body.ConnectionID,
					TemplateVersionID: in.Body.TemplateVersionID, Follow: in.Body.Follow, Custom: in.Body.Custom, Vars: in.Body.Vars, Start: start,
					Delivery: in.Body.Delivery})
				if err != nil {
					return nil, d.fail(ctx, "events.subscription_create", err)
				}
				return &subOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "update-subscription", Method: http.MethodPut, Path: "/spaces/{space_id}/subscriptions/{subscription_id}",
			Summary: "Update a subscription", Description: "A changed subject requires start; any start other than new is an audited replay (needs replay:execute).",
			Tags: subTags},
			func(ctx context.Context, in *struct {
				SubscriptionPath
				IfMatch
				Body SubscriptionPatch
			}) (*subOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "events.subscription_update", err)
				}
				if in.Body.Start != nil {
					if err := d.requireReplay(ctx, in.SpaceID, in.Body.Start.Policy); err != nil {
						return nil, err
					}
				}
				s, err := svc.UpdateSubscription(ctx, Actor(ctx), in.SpaceID, in.SubscriptionID, v, events.SubscriptionUpdate{Name: in.Body.Name, Enabled: in.Body.Enabled,
					TemplateVersionID: in.Body.TemplateVersionID, Follow: in.Body.Follow, Custom: in.Body.Custom, Vars: in.Body.Vars, Start: in.Body.Start})
				if err != nil {
					return nil, d.fail(ctx, "events.subscription_update", err)
				}
				return &subOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-subscription", Method: http.MethodDelete, Path: "/spaces/{space_id}/subscriptions/{subscription_id}",
			Summary: "Delete a subscription", Tags: subTags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *SubscriptionPath) (*struct{}, error) {
				if err := svc.DeleteSubscription(ctx, Actor(ctx), in.SpaceID, in.SubscriptionID); err != nil {
					return nil, d.fail(ctx, "events.subscription_delete", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-subscription-delivery", Method: http.MethodGet, Path: "/spaces/{space_id}/subscriptions/{subscription_id}/deliveries",
			Summary: "Recent deliveries of a subscription", Tags: subTags},
			func(ctx context.Context, in *struct {
				SubscriptionPath
				Limit int `query:"limit" minimum:"1" maximum:"500" default:"100"`
			}) (*struct {
				Body struct {
					Items []*events.Delivery `json:"items"`
				}
			}, error) {
				l, err := svc.Deliveries(ctx, in.SpaceID, in.SubscriptionID, in.Limit)
				if err != nil {
					return nil, d.fail(ctx, "events.deliveries", err)
				}
				out := &struct {
					Body struct {
						Items []*events.Delivery `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "replay-subscription", Method: http.MethodPost, Path: "/spaces/{space_id}/subscriptions/{subscription_id}/replay",
			Summary: "Replay a subscription from a position",
			Description: "An explicit, audited replay (needs replay:execute): the consumer is recreated at the position and events stored before that moment are " +
				"delivered with origin replay, never as retries. Inbox deduplication still applies unless fresh_items is set. Refused (precondition_failed) for core subscriptions, which keep no history.",
			Tags: subTags, DefaultStatus: http.StatusAccepted},
			func(ctx context.Context, in *struct {
				SubscriptionPath
				IfMatch
				Body events.ReplayInput
			}) (*struct{ Body *events.Replay }, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "events.replay", err)
				}
				r, err := svc.Replay(ctx, Actor(ctx), in.SpaceID, in.SubscriptionID, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "events.replay", err)
				}
				return &struct{ Body *events.Replay }{Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-subscription-replays", Method: http.MethodGet,
			Path: "/spaces/{space_id}/subscriptions/{subscription_id}/replays", Summary: "A subscription's replay history, newest first", Tags: subTags},
			func(ctx context.Context, in *SubscriptionPath) (*struct {
				Body struct {
					Items []*events.Replay `json:"items"`
				}
			}, error) {
				l, err := svc.Replays(ctx, in.SpaceID, in.SubscriptionID, 50)
				if err != nil {
					return nil, d.fail(ctx, "events.replays", err)
				}
				out := &struct {
					Body struct {
						Items []*events.Replay `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-quarantine", Method: http.MethodGet, Path: "/spaces/{space_id}/subscriptions/{subscription_id}/quarantine",
			Summary: "Quarantined events of a subscription", Tags: subTags},
			func(ctx context.Context, in *struct {
				SubscriptionPath
				Status string `query:"status" enum:"open,reprocessed,discarded" required:"false"`
			}) (*struct {
				Body struct {
					Items []*events.QuarantineEntry `json:"items"`
				}
			}, error) {
				l, err := svc.Quarantine(ctx, in.SpaceID, in.SubscriptionID, in.Status)
				if err != nil {
					return nil, d.fail(ctx, "events.quarantine", err)
				}
				out := &struct {
					Body struct {
						Items []*events.QuarantineEntry `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "resolve-quarantine", Method: http.MethodPost,
			Path: "/spaces/{space_id}/subscriptions/{subscription_id}/quarantine/{entry_id}", Summary: "Reprocess or discard a quarantined event", Tags: subTags},
			func(ctx context.Context, in *struct {
				SubscriptionPath
				EntryID string `path:"entry_id" maxLength:"64"`
				Body    struct {
					Action string `json:"action" enum:"reprocess,discard"`
				}
			}) (*struct{ Body *events.QuarantineEntry }, error) {
				q, err := svc.ResolveQuarantine(ctx, Actor(ctx), in.SpaceID, in.SubscriptionID, in.EntryID, in.Body.Action == "reprocess")
				if err != nil {
					return nil, d.fail(ctx, "events.quarantine_resolve", err)
				}
				return &struct{ Body *events.QuarantineEntry }{Body: q}, nil
			})
	}
}

// requireReplay enforces replay:execute for any cursor other than "new" (SM-EVT-015: replay is a separate permission).
func (d *Deps) requireReplay(ctx context.Context, spaceID, policy string) error {
	if policy == "" || policy == events.StartNew {
		return nil
	}
	p := PrincipalFrom(ctx)
	if d.Authz == nil || p == nil || !d.Authz.AllowedAll(p.Subjects(), spaceID, authz.ReplayExecute) {
		return errForbidden()
	}
	return nil
}
