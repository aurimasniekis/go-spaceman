package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// StoreFeed reads the change feed from the database.
type StoreFeed struct{ S *store.Store }

// Latest is the newest change's position.
func (f StoreFeed) Latest(ctx context.Context) (int64, error) { return f.S.R().LatestChangeSeq(ctx) }

// After returns up to limit changes after a position.
func (f StoreFeed) After(ctx context.Context, after int64, limit int) ([]changefeed.Change, error) {
	rows, err := f.S.R().ChangesAfter(ctx, db.ChangesAfterParams{After: after, Lim: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]changefeed.Change, len(rows))
	for i, r := range rows {
		out[i] = changefeed.FromRow(r)
	}
	return out, nil
}

// StoreMappings keeps card mappings in telegram_messages.
type StoreMappings struct{ S *store.Store }

// Save upserts the rows in one transaction.
func (m StoreMappings) Save(ctx context.Context, rows []Mapping) error {
	return m.S.W().TxNamed(ctx, "telegram.messages", func(tx *store.Tx) error {
		for _, r := range rows {
			if r.Delete {
				if err := tx.Q.DeleteTelegramMessage(ctx, db.DeleteTelegramMessageParams{ChatID: r.ChatID, MessageID: int64(r.MessageID)}); err != nil {
					return err
				}
				continue
			}
			if err := tx.Q.UpsertTelegramMessage(ctx, db.UpsertTelegramMessageParams{ChatID: r.ChatID, MessageID: int64(r.MessageID),
				LinkID: sql.NullString{String: r.LinkID, Valid: r.LinkID != ""}, SpaceID: sql.NullString{String: r.SpaceID, Valid: r.SpaceID != ""},
				Kind: kindOf(r), ResourceType: r.Key.Type, ResourceID: r.Key.ID, ResourceVersion: r.Version, CreatedAt: r.CreatedAt.UnixMicro()}); err != nil {
				return err
			}
		}
		return nil
	})
}

func kindOf(m Mapping) string {
	if m.Cont {
		return "cont"
	}
	return "card"
}

// Lookup returns a resource's cards and their continuation messages.
func (m StoreMappings) Lookup(ctx context.Context, k Key) ([]Mapping, error) {
	rows, err := m.S.R().ListTelegramMessagesByResource(ctx, db.ListTelegramMessagesByResourceParams{ResourceType: k.Type, ResourceID: k.ID})
	if err != nil {
		return nil, err
	}
	out := make([]Mapping, len(rows))
	for i, r := range rows {
		out[i] = Mapping{ChatID: r.ChatID, MessageID: int(r.MessageID), Key: k, Version: r.ResourceVersion, CreatedAt: time.UnixMicro(r.CreatedAt), Cont: r.Kind == "cont"}
	}
	return out, nil
}

// StoreReader reads resources with one lean row read each, as the system (no principal: recipients are checked
// afterwards, in memory).
type StoreReader struct {
	S     *store.Store
	Human *human.Service
}

var errUnknownType = errors.New("notify: unknown resource type")

// Read implements Reader.
func (r StoreReader) Read(ctx context.Context, scope map[string]any, k Key) (*Resource, error) {
	q := r.S.R()
	var out *Resource
	var err error
	switch k.Type {
	case TypeHumanAction:
		out, err = r.action(ctx, scope, k)
	case TypeInboxItem:
		var it db.InboxItem
		if it, err = q.GetInboxItem(ctx, k.ID); err == nil {
			out = inboxResource(k, it)
		}
	case TypeTask:
		var t db.Task
		if t, err = q.GetTask(ctx, k.ID); err == nil {
			out = &Resource{Key: k, SpaceID: t.SpaceID, Version: t.Version, Kind: KindTaskReview, Perm: authz.QueueRead, Label: "Task",
				Title: t.Title, State: t.Status, Notify: t.Status == "review"}
		}
	case TypeMessage:
		var m db.Message
		if m, err = q.GetMessage(ctx, k.ID); err == nil {
			out = messageResource(k, m)
		}
	case TypeRun:
		var ru db.Run
		if ru, err = q.GetRun(ctx, k.ID); err == nil {
			out = runResource(k, ru)
		}
	default:
		return nil, errUnknownType
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return out, err
}

func (r StoreReader) action(ctx context.Context, scope map[string]any, k Key) (*Resource, error) {
	a, err := r.S.R().GetHumanAction(ctx, k.ID)
	if err != nil {
		return nil, err
	}
	pk := "perm:" + a.SpaceID
	ps, ok := scope[pk].(human.PermissionSet)
	if !ok {
		// The same permission set the API consults, loaded once per space and window.
		if ps, err = r.Human.Permissions(ctx, a.SpaceID); err != nil {
			return nil, err
		}
		scope[pk] = ps
	}
	role := ps.For(&human.Action{SpaceID: a.SpaceID, Type: a.Type, TypeSlug: a.TypeSlug}).ViewRole
	open := a.Status == human.Open || a.Status == human.InProgress
	state := a.Status
	if a.Outcome != "" {
		state = fmt.Sprintf("%s: %s", a.Status, a.Outcome)
	}
	detail := a.Body
	if a.Type == human.TypeApproval {
		// An enforced approval's body carries its (redacted) arguments: never sent to Telegram (SM-APPR-005, ADR-0083
		// decision 14). Enforced: an approvals row points at it, or the approvals package's dedup key (set before the
		// row is linked); a lookup error counts as enforced.
		if strings.HasPrefix(a.DedupKey, "approval:") {
			detail = ""
		} else if _, err := r.S.R().GetApprovalByAction(ctx, a.ID); !errors.Is(err, sql.ErrNoRows) {
			detail = ""
		}
	}
	return &Resource{Key: k, SpaceID: a.SpaceID, Version: a.Version, Kind: KindAction, Perm: authz.HumanRead, ViewRole: role,
		Label: humanLabel(a.Type, a.TypeSlug), Title: a.Title, Detail: detail, State: state, Priority: a.Priority, Assignee: a.Assignee,
		Notify: open, Final: !open, RemindedAt: a.RemindedAt.Int64, EscalatedAt: a.EscalatedAt.Int64}, nil
}

func humanLabel(t, slug string) string {
	switch t {
	case human.TypeQuestion:
		return "Question"
	case human.TypeApproval:
		return "Approval"
	case human.TypeTodo:
		return "To-do"
	case human.TypeMessage:
		return "Message"
	}
	if slug != "" {
		return slug
	}
	return "Action"
}

func inboxResource(k Key, it db.InboxItem) *Resource {
	fresh := it.Status == "new"
	return &Resource{Key: k, SpaceID: it.SpaceID, Version: it.Version, Kind: KindInbox, Perm: authz.InboxRead, Label: "Inbox",
		Title: it.Title, Detail: it.Body, State: it.Status, Notify: fresh, Final: it.Status == "done" || it.Status == "rejected"}
}

// messageResource: only messages to the operators are notified (to_kind operators); acknowledged or resolved ones are final.
func messageResource(k Key, m db.Message) *Resource {
	if m.ToKind != "operators" {
		return nil
	}
	open := m.State == "stored" || m.State == "available"
	return &Resource{Key: k, SpaceID: m.SpaceID, Version: m.Version, Kind: KindMessage, Perm: authz.MessageRead, Label: "Message",
		Title: m.Subject, Detail: m.Body, State: messageState(m.State), Notify: open, Final: !open}
}

// runResource: a run is notified only when it failed or was lost (runs.observed); other states are of no interest
// (plan 4.2 step 3).
func runResource(k Key, ru db.Run) *Resource {
	if ru.Observed != "failed" && ru.Observed != "lost" {
		return nil
	}
	return &Resource{Key: k, SpaceID: ru.SpaceID, Version: ru.Version, Kind: KindFailedRun, Perm: authz.RunRead, Label: "Run",
		Title: ru.Role, State: ru.Observed, Notify: true}
}

// ErrNoCard is returned by Card for a message that isn't a card the bot recorded.
var ErrNoCard = errors.New("notify: no such card")

// Card returns the card recorded for a message (the chat's message ID), with the link it was sent to.
func (m StoreMappings) Card(ctx context.Context, chatID int64, messageID int) (Mapping, error) {
	r, err := m.S.R().GetTelegramMessage(ctx, db.GetTelegramMessageParams{ChatID: chatID, MessageID: int64(messageID)})
	if errors.Is(err, sql.ErrNoRows) {
		return Mapping{}, ErrNoCard
	}
	if err != nil {
		return Mapping{}, err
	}
	return Mapping{ChatID: chatID, MessageID: messageID, LinkID: r.LinkID.String, SpaceID: r.SpaceID.String,
		Key: Key{Type: r.ResourceType, ID: r.ResourceID}, Version: r.ResourceVersion}, nil
}

// messageState is what a message card shows: the internal state ("stored", "available") means nobody read it yet.
func messageState(st string) string {
	switch st {
	case "stored", "available":
		return "Unread"
	case "acknowledged":
		return "Read"
	case "resolved":
		return "Resolved"
	}
	return ""
}
