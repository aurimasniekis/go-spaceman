package human

import (
	"context"
	"fmt"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Routed human responses as inbox items (SM-INBOX-001, ADR-0049).
//
// A response always reaches an agent requester as a durable, routed notification (ADR-0032). When the action type's
// policy says so (response_inbox), it also becomes an inbox item of the action's space, with source kind
// human_response, so control triages it like any other input (reply, create a task, defer, associate it with work):
//   - routed: when the response is routed to control or the operators because the requester's run had ended (the
//     work that asked is gone, so someone has to decide what the answer means now);
//   - always: whenever an action of the type is answered, whoever asked (operators, SpaceMan itself, a live agent).
//
// The item carries metadata only: the action, its type, outcome, who answered and why it was routed. The response
// text stays on the action, where only the runs it concerns can read it (spaceman_human_get, SM-HUM-009): every run
// of a space can read the inbox, so copying the response there would widen who sees it.

// responseItemKind is the only notification kind that produces an item: a person's actual response. Cancellations,
// expiries and clarification requests stay notifications.
const responseItemKind = "resolved"

// responseItem adds the inbox item of a's response inside tx (deduplicated per action, so routing it again, or the
// routed and the always rule together, never add a second one). routedTo and reason are empty for always.
func (svc *Service) responseItem(ctx context.Context, tx *store.Tx, a db.HumanAction, nid, routedTo, reason string) error {
	if svc.Inbox == nil {
		return nil
	}
	trusted := map[string]any{"action_id": a.ID, "action_type": a.Type, "status": a.Status, "outcome": a.Outcome, "requester_kind": a.RequesterKind,
		"responded_by": a.RespondedBy}
	if a.RequesterRun != "" {
		trusted["requester_run"] = a.RequesterRun
	}
	if a.TaskID != "" {
		trusted["task_id"] = a.TaskID
	}
	if nid != "" {
		trusted["notification_id"] = nid
	}
	routed := ""
	if routedTo != "" {
		trusted["routed_to"], trusted["route_reason"] = routedTo, reason
		routed = fmt.Sprintf(" It was routed to %s: %s.", routedTo, reasonText(reason))
	}
	outcome := ""
	if a.Outcome != "" {
		outcome = fmt.Sprintf(" with the outcome `%s`", a.Outcome)
	}
	body := fmt.Sprintf("A person responded to human action `%s` (%s)%s.%s\n\n"+
		"The response itself stays on the action: read it with `spaceman_human_get` (action_id `%s`). "+
		"Decide what it means for the work now: reply, create or update a task, or mark this item done.", a.ID, a.Type, outcome, routed, a.ID)
	corr := a.CorrelationID
	if corr == "" {
		corr = a.ID
	}
	_, _, err := svc.Inbox.CreateTx(ctx, tx, audit.System, a.SpaceID, inbox.CreateInput{SourceKind: inbox.SourceHumanResponse, SourceID: a.ID,
		SourceLabel: "Human actions", Title: "Response: " + a.Title, ContentType: inbox.Markdown, Body: body, CorrelationID: corr,
		DedupKey: "response:" + a.ID, Urgency: urgencyOf(a.Priority), Trusted: trusted, CausationID: a.ID})
	return err
}

func urgencyOf(priority string) string {
	switch priority {
	case "low", "high", "urgent":
		return priority
	}
	return "normal"
}

// responseRouted adds the item of a response routed to control or the operators, when the policy asks for it.
func (svc *Service) responseRouted(ctx context.Context, tx *store.Tx, a db.HumanAction, nid, kind string, rt route, reason string) error {
	if svc.Inbox == nil || kind != responseItemKind || (rt.kind != TargetControl && rt.kind != TargetOperators) {
		return nil
	}
	p, err := policyFor(ctx, tx.Q, a.SpaceID, a.Type)
	if err != nil {
		return err
	}
	if p.ResponseInbox != ResponseInboxRouted && p.ResponseInbox != ResponseInboxAlways {
		return nil
	}
	return svc.responseItem(ctx, tx, a, nid, rt.kind, reason)
}

// responseAlways adds the item of a response when the policy says always (after any routing, so a routed item keeps
// its routing details).
func (svc *Service) responseAlways(ctx context.Context, tx *store.Tx, a db.HumanAction) error {
	if svc.Inbox == nil || a.Status != Resolved {
		return nil
	}
	p, err := policyFor(ctx, tx.Q, a.SpaceID, a.Type)
	if err != nil || p.ResponseInbox != ResponseInboxAlways {
		return err
	}
	return svc.responseItem(ctx, tx, a, "", "", "")
}
