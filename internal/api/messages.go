package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
)

// MessageRules are the authorization rules of the message operations.
var MessageRules = map[string]Rule{
	"list-messages":   InSpace(authz.MessageRead),
	"get-message":     InSpace(authz.MessageRead),
	"send-message":    InSpace(authz.MessageWrite),
	"ack-message":     InSpace(authz.MessageWrite),
	"reply-message":   InSpace(authz.MessageWrite),
	"resolve-message": InSpace(authz.MessageWrite),
}

// MessagePath identifies a message.
type MessagePath struct {
	SpaceID   string `path:"space_id" maxLength:"64"`
	MessageID string `path:"message_id" maxLength:"64"`
}

// MessageList is a space's recent messages, with the number waiting for operators.
type MessageList struct {
	Items            []*messages.Message `json:"items"`
	OperatorsPending int64               `json:"operators_pending" doc:"Messages to operators not yet acknowledged or resolved"`
}

func operatorFrom(ctx context.Context) messages.From {
	a := Actor(ctx)
	return messages.From{Kind: "operator", ID: a.ID, Label: actorLabel(ctx)}
}

// RegisterMessages registers the message operations.
func RegisterMessages(svc *messages.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"messages"}
		huma.Register(api, huma.Operation{OperationID: "list-messages", Method: http.MethodGet, Path: "/spaces/{space_id}/messages",
			Summary: "A space's recent messages between agents, control and operators", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Limit int `query:"limit" minimum:"0" maximum:"500"`
			}) (*struct{ Body MessageList }, error) {
				l, err := svc.List(ctx, in.SpaceID, in.Limit)
				if err != nil {
					return nil, d.fail(ctx, "messages.list", err)
				}
				n, err := svc.OperatorPending(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "messages.list", err)
				}
				return &struct{ Body MessageList }{Body: MessageList{Items: l, OperatorsPending: n}}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-message", Method: http.MethodGet, Path: "/spaces/{space_id}/messages/{message_id}", Summary: "A message", Tags: tags},
			func(ctx context.Context, in *MessagePath) (*struct{ Body *messages.Message }, error) {
				m, err := svc.Get(ctx, in.SpaceID, in.MessageID)
				if err != nil {
					return nil, d.fail(ctx, "messages.get", err)
				}
				return &struct{ Body *messages.Message }{Body: m}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "send-message", Method: http.MethodPost, Path: "/spaces/{space_id}/messages",
			Summary: "Send a durable message to control, a role or a run", Description: "The recipient is notified when its program is idle and reads it through its tools.",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				SpacePath
				IdempotencyKey
				Body struct {
					To            string `json:"to" maxLength:"80" doc:"control, role:<name> or run:<run id>"`
					Subject       string `json:"subject" maxLength:"200"`
					Body          string `json:"body,omitempty" maxLength:"100000" doc:"Markdown"`
					Request       bool   `json:"request,omitempty" doc:"Expect a reply"`
					CorrelationID string `json:"correlation_id,omitempty" maxLength:"256"`
				}
			}) (*struct{ Body *messages.Message }, error) {
				m, _, err := Idempotent(ctx, d, "send-message", in.IdempotencyKey, map[string]any{"space": in.SpaceID, "body": in.Body}, false, func() (*messages.Message, error) {
					if in.Body.To == "operators" {
						return nil, errOperatorsToOperators
					}
					kind := "message"
					if in.Body.Request {
						kind = "request"
					}
					m, _, err := svc.Send(ctx, Actor(ctx), in.SpaceID, operatorFrom(ctx), messages.SendInput{To: in.Body.To, Kind: kind, Subject: in.Body.Subject,
						Body: in.Body.Body, CorrelationID: in.Body.CorrelationID})
					return m, err
				})
				if err != nil {
					return nil, d.fail(ctx, "messages.send", err)
				}
				return &struct{ Body *messages.Message }{Body: m}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "ack-message", Method: http.MethodPost, Path: "/spaces/{space_id}/messages/{message_id}/ack",
			Summary: "Acknowledge a message addressed to operators", Tags: tags},
			func(ctx context.Context, in *MessagePath) (*struct{ Body *messages.Message }, error) {
				m, err := svc.AckOperator(ctx, Actor(ctx), in.SpaceID, in.MessageID)
				if err != nil {
					return nil, d.fail(ctx, "messages.ack", err)
				}
				return &struct{ Body *messages.Message }{Body: m}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "reply-message", Method: http.MethodPost, Path: "/spaces/{space_id}/messages/{message_id}/reply",
			Summary: "Reply to an agent's message", Description: "A reply to a request resolves it.", Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				MessagePath
				Body struct {
					Body string `json:"body" maxLength:"100000"`
				}
			}) (*struct{ Body *messages.Message }, error) {
				orig, err := svc.Get(ctx, in.SpaceID, in.MessageID)
				if err != nil {
					return nil, d.fail(ctx, "messages.reply", err)
				}
				if orig.FromKind != "run" {
					return nil, d.fail(ctx, "messages.reply", errReplyOnlyRuns)
				}
				m, _, err := svc.Send(ctx, Actor(ctx), in.SpaceID, operatorFrom(ctx), messages.SendInput{To: "run:" + orig.FromID, Kind: "reply",
					Subject: "Re: " + orig.Subject, Body: in.Body.Body, ReplyTo: orig.ID})
				if err != nil {
					return nil, d.fail(ctx, "messages.reply", err)
				}
				return &struct{ Body *messages.Message }{Body: m}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "resolve-message", Method: http.MethodPost, Path: "/spaces/{space_id}/messages/{message_id}/resolve",
			Summary: "Close a message or request", Tags: tags},
			func(ctx context.Context, in *struct {
				MessagePath
				Body struct {
					Resolution string `json:"resolution,omitempty" maxLength:"2000"`
				}
			}) (*struct{ Body *messages.Message }, error) {
				m, err := svc.Resolve(ctx, Actor(ctx), in.SpaceID, in.MessageID, in.Body.Resolution)
				if err != nil {
					return nil, d.fail(ctx, "messages.resolve", err)
				}
				return &struct{ Body *messages.Message }{Body: m}, nil
			})
	}
}

var (
	errOperatorsToOperators = apperr.New(apperr.CodeInvalid, "operators send to control, a role or a run")
	errReplyOnlyRuns        = apperr.New(apperr.CodeInvalid, "only messages from agents can be replied to here")
)
