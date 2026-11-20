package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// MessageRules are the authorization rules of the message operations.
var MessageRules = map[string]Rule{
	"list-messages":   InSpace(authz.MessageRead),
	"get-message":     InSpace(authz.MessageRead),
	"send-message":    InSpace(authz.MessageWrite),
	"ack-message":     InSpace(authz.MessageWrite),
	"reply-message":   InSpace(authz.MessageWrite),
	"resolve-message": InSpace(authz.MessageWrite),
	"cancel-message":  InSpace(authz.MessageWrite),
	// The operator inbox across spaces: only messages to operators in spaces where the caller has message:read.
	"list-operator-messages": FilteredRead(),
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

// OperatorMessage is a message to operators with the space it is in, so a client can link to it and act through the
// per-space ack, reply and resolve operations.
type OperatorMessage struct {
	messages.Message
	SpaceSlug string `json:"space_slug"`
	SpaceName string `json:"space_name"`
}

// OperatorMessageList is a page of the operator inbox.
type OperatorMessageList struct {
	Items       []OperatorMessage `json:"items" doc:"Newest first"`
	NextCursor  string            `json:"next_cursor,omitempty"`
	UnreadCount int64             `json:"unread_count" doc:"Open messages to operators (not acknowledged or resolved) in all spaces you can read, whatever the filters"`
}

func (d *Deps) listOperatorMessages(ctx context.Context, svc *messages.Service, spaceFilter, state, kind string, p Page) (*OperatorMessageList, error) {
	after, id, err := DecodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}
	var afterCreated int64
	if p.Cursor != "" {
		if afterCreated, err = strconv.ParseInt(after, 10, 64); err != nil || afterCreated <= 0 {
			return nil, apperr.New(apperr.CodeInvalid, "invalid cursor")
		}
	}
	spaces, err := d.Store.R().ListSpaces(ctx)
	if err != nil {
		return nil, err
	}
	pr := PrincipalFrom(ctx)
	byID := map[string]db.Space{}
	var readable, selected []string
	for _, sp := range spaces {
		if d.Authz == nil || pr == nil || !d.Authz.AllowedAll(pr.Subjects(), sp.ID, authz.MessageRead) {
			continue
		}
		byID[sp.ID] = sp
		readable = append(readable, sp.ID)
		if spaceFilter == "" || spaceFilter == sp.ID || spaceFilter == sp.Slug {
			selected = append(selected, sp.ID)
		}
	}
	unread, err := svc.OperatorUnread(ctx, readable)
	if err != nil {
		return nil, err
	}
	l, more, err := svc.ListOperator(ctx, messages.OperatorQuery{Spaces: selected, All: state == "all", Kind: kind,
		AfterCreated: afterCreated, AfterID: id, Limit: p.limit()})
	if err != nil {
		return nil, err
	}
	out := &OperatorMessageList{Items: make([]OperatorMessage, 0, len(l)), UnreadCount: unread}
	for _, m := range l {
		sp := byID[m.SpaceID]
		out.Items = append(out.Items, OperatorMessage{Message: *m, SpaceSlug: sp.Slug, SpaceName: sp.Name})
	}
	if more && len(l) > 0 {
		last := l[len(l)-1]
		out.NextCursor = EncodeCursor(strconv.FormatInt(last.CreatedAt.UnixMicro(), 10), last.ID)
	}
	return out, nil
}

func operatorFrom(ctx context.Context) messages.From {
	a := Actor(ctx)
	return messages.From{Kind: "operator", ID: a.ID, Label: actorLabel(ctx)}
}

// RegisterMessages registers the message operations.
func RegisterMessages(svc *messages.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"messages"}
		huma.Register(api, huma.Operation{OperationID: "list-operator-messages", Method: http.MethodGet, Path: "/messages",
			Summary: "Messages addressed to operators, across the spaces you can read",
			Description: "Only messages to operators in spaces where you have message:read (never messages to control, roles or runs), newest first. " +
				"By default only open messages (not acknowledged or resolved). Act on an item through the per-space ack, reply and resolve operations.",
			Tags: tags},
			func(ctx context.Context, in *struct {
				Page
				State string `query:"state" enum:"open,all" default:"open" doc:"open: not yet acknowledged or resolved"`
				Space string `query:"space" maxLength:"64" doc:"Only this space (ID or slug)"`
				Kind  string `query:"kind" enum:",message,request,reply,notification" maxLength:"16"`
			}) (*struct{ Body *OperatorMessageList }, error) {
				l, err := d.listOperatorMessages(ctx, svc, in.Space, in.State, in.Kind, in.Page)
				if err != nil {
					return nil, d.fail(ctx, "messages.list_operator", err)
				}
				return &struct{ Body *OperatorMessageList }{Body: l}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-messages", Method: http.MethodGet, Path: "/spaces/{space_id}/messages",
			Summary: "A space's recent messages between agents, control and operators", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Limit     int  `query:"limit" minimum:"0" maximum:"500"`
				Scheduled bool `query:"scheduled" doc:"Only scheduled messages to control that are not yet due, earliest first"`
			}) (*struct{ Body MessageList }, error) {
				var l []*messages.Message
				var err error
				if in.Scheduled {
					l, err = svc.Scheduled(ctx, in.SpaceID)
				} else {
					l, err = svc.List(ctx, in.SpaceID, in.Limit)
				}
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
					To            string   `json:"to" maxLength:"80" doc:"control, role:<name> or run:<run id>"`
					Subject       string   `json:"subject" maxLength:"200"`
					Body          string   `json:"body,omitempty" maxLength:"100000" doc:"Markdown"`
					Request       bool     `json:"request,omitempty" doc:"Expect a reply"`
					CorrelationID string   `json:"correlation_id,omitempty" maxLength:"256"`
					AttachmentIDs []string `json:"attachment_ids,omitempty" maxItems:"20" doc:"Attachments of this space the message references"`
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
						Body: in.Body.Body, CorrelationID: in.Body.CorrelationID, AttachmentIDs: in.Body.AttachmentIDs})
					return m, err
				})
				if err != nil {
					return nil, d.fail(ctx, "messages.send", err)
				}
				return &struct{ Body *messages.Message }{Body: m}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "ack-message", Method: http.MethodPost, Path: "/spaces/{space_id}/messages/{message_id}/ack",
			Summary: "Acknowledge a message addressed to operators", Description: "Optional If-Match: a message that changed since that version is refused as a conflict (409 precondition_failed).", Tags: tags},
			func(ctx context.Context, in *struct {
				MessagePath
				OptionalIfMatch
			}) (*struct{ Body *messages.Message }, error) {
				v, err := optionalVersion(in.OptionalIfMatch)
				if err != nil {
					return nil, d.fail(ctx, "messages.ack", err)
				}
				m, err := svc.AckOperator(ctx, Actor(ctx), in.SpaceID, in.MessageID, v)
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
					Body          string   `json:"body" maxLength:"100000"`
					AttachmentIDs []string `json:"attachment_ids,omitempty" maxItems:"20" doc:"Attachments of this space the reply references"`
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
					Subject: "Re: " + orig.Subject, Body: in.Body.Body, ReplyTo: orig.ID, AttachmentIDs: in.Body.AttachmentIDs})
				if err != nil {
					return nil, d.fail(ctx, "messages.reply", err)
				}
				return &struct{ Body *messages.Message }{Body: m}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "cancel-message", Method: http.MethodPost, Path: "/spaces/{space_id}/messages/{message_id}/cancel",
			Summary: "Cancel a scheduled message before it is due", Description: "It is resolved as cancelled and never delivered (ADR-0065).", Tags: tags},
			func(ctx context.Context, in *MessagePath) (*struct{ Body *messages.Message }, error) {
				m, err := svc.Cancel(ctx, Actor(ctx), in.SpaceID, in.MessageID)
				if err != nil {
					return nil, d.fail(ctx, "messages.cancel", err)
				}
				return &struct{ Body *messages.Message }{Body: m}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "resolve-message", Method: http.MethodPost, Path: "/spaces/{space_id}/messages/{message_id}/resolve",
			Summary: "Close a message or request", Description: "Optional If-Match: a message that changed since that version is refused as a conflict (409 precondition_failed).", Tags: tags},
			func(ctx context.Context, in *struct {
				MessagePath
				OptionalIfMatch
				Body struct {
					Resolution string `json:"resolution,omitempty" maxLength:"2000"`
				}
			}) (*struct{ Body *messages.Message }, error) {
				v, err := optionalVersion(in.OptionalIfMatch)
				if err != nil {
					return nil, d.fail(ctx, "messages.resolve", err)
				}
				m, err := svc.Resolve(ctx, Actor(ctx), in.SpaceID, in.MessageID, in.Body.Resolution, v)
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
