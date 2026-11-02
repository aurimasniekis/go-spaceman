package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
)

// InboxRules are the authorization rules of the inbox operations.
var InboxRules = map[string]Rule{
	"list-inbox":        InSpace(authz.InboxRead),
	"get-inbox-item":    InSpace(authz.InboxRead),
	"create-inbox-item": InSpace(authz.InboxWrite),
	"set-inbox-status":  InSpace(authz.InboxWrite),
}

// InboxItemPath identifies an inbox item.
type InboxItemPath struct {
	SpaceID string `path:"space_id" maxLength:"64"`
	ItemID  string `path:"item_id" maxLength:"64"`
}

// CreateInboxInput is an operator's new inbox item. The source is always the signed-in operator.
type CreateInboxInput struct {
	Title         string   `json:"title,omitempty" maxLength:"200" doc:"Defaults to the first line of the body"`
	ContentType   string   `json:"content_type,omitempty" enum:"text/markdown,application/json" doc:"Default text/markdown"`
	Body          string   `json:"body" maxLength:"200000" doc:"Markdown (reference attachments as spaceman://attachment/<id>) or JSON"`
	Urgency       string   `json:"urgency,omitempty" enum:"low,normal,high,urgent"`
	AttachmentIDs []string `json:"attachment_ids,omitempty" maxItems:"20" doc:"Attachments uploaded to this space"`
	CorrelationID string   `json:"correlation_id,omitempty" maxLength:"256"`
}

// InboxPage is a page of a space's inbox with the number of items per status.
type InboxPage struct {
	Items      []*inbox.Item    `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
	Counts     map[string]int64 `json:"counts"`
}

type inboxItemOut struct {
	ETag string `header:"ETag"`
	Body *inbox.Item
}

// RegisterInbox registers the inbox operations.
func RegisterInbox(svc *inbox.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"inbox"}
		huma.Register(api, huma.Operation{OperationID: "list-inbox", Method: http.MethodGet, Path: "/spaces/{space_id}/inbox", Summary: "List a space's inbox",
			Description: "Newest first. Bodies are untrusted content.", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Status string `query:"status" enum:"new,in_progress,deferred,rejected,done" required:"false"`
				Cursor string `query:"cursor" maxLength:"200"`
				Limit  int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
			}) (*struct{ Body InboxPage }, error) {
				p, err := svc.List(ctx, in.SpaceID, in.Status, in.Cursor, in.Limit)
				if err != nil {
					return nil, d.fail(ctx, "inbox.list", err)
				}
				counts, err := svc.Counts(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "inbox.counts", err)
				}
				return &struct{ Body InboxPage }{Body: InboxPage{Items: p.Items, NextCursor: p.NextCursor, Counts: counts}}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-inbox-item", Method: http.MethodGet, Path: "/spaces/{space_id}/inbox/{item_id}",
			Summary: "Get an inbox item with its attachments", Tags: tags},
			func(ctx context.Context, in *InboxItemPath) (*inboxItemOut, error) {
				it, err := svc.Get(ctx, in.SpaceID, in.ItemID)
				if err != nil {
					return nil, d.fail(ctx, "inbox.get", err)
				}
				return &inboxItemOut{ETag: ETag(it.Version), Body: it}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-inbox-item", Method: http.MethodPost, Path: "/spaces/{space_id}/inbox",
			Summary: "Add an item to a space's inbox", Description: "The item comes from the signed-in operator. It is not a task: control decides what it becomes.",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				SpacePath
				IdempotencyKey
				Body CreateInboxInput
			}) (*inboxItemOut, error) {
				p := PrincipalFrom(ctx)
				label := "root token"
				if p.User != nil {
					label = p.User.DisplayName
				}
				it, _, err := Idempotent(ctx, d, "create-inbox-item", in.IdempotencyKey, map[string]any{"space": in.SpaceID, "body": in.Body}, false,
					func() (*inbox.Item, error) {
						it, _, err := svc.Create(ctx, Actor(ctx), in.SpaceID, inbox.CreateInput{SourceKind: "operator", SourceID: Actor(ctx).ID, SourceLabel: label,
							Title: in.Body.Title, ContentType: in.Body.ContentType, Body: in.Body.Body, Urgency: in.Body.Urgency,
							AttachmentIDs: in.Body.AttachmentIDs, CorrelationID: in.Body.CorrelationID,
							Trusted: map[string]any{"via": p.Method}})
						return it, err
					})
				if err != nil {
					return nil, d.fail(ctx, "inbox.create", err)
				}
				return &inboxItemOut{ETag: ETag(it.Version), Body: it}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "set-inbox-status", Method: http.MethodPost, Path: "/spaces/{space_id}/inbox/{item_id}/status",
			Summary: "Change an inbox item's processing status", Description: "Defer (until a time), reject, mark done or associate with existing work.",
			Tags: tags},
			func(ctx context.Context, in *struct {
				InboxItemPath
				IfMatch
				Body struct {
					Status     string     `json:"status" enum:"new,in_progress,deferred,rejected,done"`
					Reason     string     `json:"reason,omitempty" maxLength:"2000"`
					DeferUntil *time.Time `json:"defer_until,omitempty"`
					LinkedKind string     `json:"linked_kind,omitempty" maxLength:"32"`
					LinkedID   string     `json:"linked_id,omitempty" maxLength:"64"`
				}
			}) (*inboxItemOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "inbox.status", err)
				}
				it, err := svc.SetStatus(ctx, Actor(ctx), in.SpaceID, in.ItemID, v, inbox.StatusInput{Status: in.Body.Status, Reason: in.Body.Reason,
					DeferUntil: in.Body.DeferUntil, LinkedKind: in.Body.LinkedKind, LinkedID: in.Body.LinkedID})
				if err != nil {
					return nil, d.fail(ctx, "inbox.status", err)
				}
				return &inboxItemOut{ETag: ETag(it.Version), Body: it}, nil
			})
	}
}
