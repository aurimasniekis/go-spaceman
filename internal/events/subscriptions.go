package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/events/template"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Cursor policies (SM-EVT-007).
const (
	StartNew  = "new"  // only events published after the consumer is (re)created
	StartAll  = "all"  // everything the stream still holds
	StartTime = "time" // from a time
	StartSeq  = "seq"  // from a stream sequence
)

// Subscription binds a template version (or a custom definition) to a space (SM-EVT-005).
type Subscription struct {
	ID           string               `json:"id"`
	SpaceID      string               `json:"space_id"`
	Name         string               `json:"name"`
	ConnectionID string               `json:"connection_id"`
	Template     *TemplateRef         `json:"template,omitempty"`
	Follow       string               `json:"follow" enum:"pinned,latest" doc:"latest moves to newly published versions that keep the same subject"`
	Custom       *template.Definition `json:"custom,omitempty"`
	Vars         map[string]string    `json:"vars"`
	Subject      string               `json:"subject" doc:"The effective Pulse filter"`
	Durable      string               `json:"durable" doc:"The durable name the Pulse operator must grant"`
	Consumer     string               `json:"consumer" doc:"The JetStream consumer: <client_id>_<durable>"`
	Grant        string               `json:"grant" doc:"The Pulse grant this subscription needs (durable:<subject>:<durable>)"`
	Enabled      bool                 `json:"enabled"`
	Cursor       Cursor               `json:"cursor"`
	LastReplay   *Replay              `json:"last_replay,omitempty" doc:"The most recent explicit replay"`
	Pending      bool                 `json:"cursor_pending" doc:"A cursor change waits to be applied by the ingestion manager"`
	LastEventAt  *time.Time           `json:"last_event_at,omitempty"`
	Quarantined  int64                `json:"quarantined" doc:"Open quarantine entries"`
	State        string               `json:"state" enum:"running,stopped,error,disabled" doc:"Runtime state reported by the ingestion manager"`
	LastError    string               `json:"last_error,omitempty"`
	Version      int64                `json:"version"`
	CreatedAt    time.Time            `json:"created_at"`
}

// Cursor is where a (re)created consumer starts.
type Cursor struct {
	Policy     string     `json:"policy" enum:"new,all,time,seq"`
	Time       *time.Time `json:"time,omitempty"`
	Seq        uint64     `json:"seq,omitempty"`
	Replay     bool       `json:"replay" doc:"Events delivered again are recorded as replayed, not as retries"`
	Generation int64      `json:"generation"`
}

// SubscriptionInput creates a subscription.
type SubscriptionInput struct {
	Name              string
	ConnectionID      string
	TemplateVersionID string
	Follow            string
	Custom            *template.Definition
	Vars              map[string]string
	Start             Cursor
}

// SubscriptionUpdate edits a subscription. Changing the effective subject needs an explicit cursor (Start): the
// consumer must be recreated, so "continue" is impossible.
type SubscriptionUpdate struct {
	Name              string
	Enabled           bool
	TemplateVersionID string
	Follow            string
	Custom            *template.Definition
	Vars              map[string]string
	Start             *Cursor
}

func durableFor(id string) string { return "sm-" + strings.ToLower(strings.TrimPrefix(id, "sub_")) }

func (svc *Service) definitionOf(ctx context.Context, q *db.Queries, r db.EventSubscription) (*template.Definition, *TemplateRef, error) {
	if r.CustomJson.Valid {
		var d template.Definition
		if err := json.Unmarshal([]byte(r.CustomJson.String), &d); err != nil {
			return nil, nil, err
		}
		return &d, nil, nil
	}
	vr, err := q.GetEventTemplateVersion(ctx, r.TemplateVersionID.String)
	if err != nil {
		return nil, nil, err
	}
	v, err := versionFromRow(vr)
	if err != nil {
		return nil, nil, err
	}
	ref := &TemplateRef{TemplateID: v.TemplateID, VersionID: v.ID, Version: v.Version}
	if t, err := q.GetEventTemplate(ctx, v.TemplateID); err == nil {
		ref.TemplateSlug = t.Slug
	}
	return &v.Definition, ref, nil
}

func validCursor(c *Cursor) error {
	switch c.Policy {
	case "":
		c.Policy = StartNew
	case StartNew, StartAll:
	case StartTime:
		if c.Time == nil {
			return apperr.New(apperr.CodeInvalid, "start.time is required for policy time")
		}
	case StartSeq:
		if c.Seq == 0 {
			return apperr.New(apperr.CodeInvalid, "start.seq is required for policy seq")
		}
	default:
		return apperr.New(apperr.CodeInvalid, "start.policy must be new, all, time or seq")
	}
	return nil
}

func cursorCols(c Cursor) (string, sql.NullInt64, sql.NullInt64) {
	var t, s sql.NullInt64
	if c.Time != nil {
		t = sql.NullInt64{Int64: c.Time.UnixMicro(), Valid: true}
	}
	if c.Seq > 0 {
		s = sql.NullInt64{Int64: int64(c.Seq), Valid: true} //nolint:gosec // stream sequences fit int64
	}
	return c.Policy, t, s
}

// resolve picks the definition of a create/update: a published template version, or a custom definition.
func (svc *Service) resolve(ctx context.Context, q *db.Queries, versionID string, custom *template.Definition) (*template.Definition, sql.NullString, sql.NullString, sql.NullString, error) {
	var none sql.NullString
	if (versionID == "") == (custom == nil) {
		return nil, none, none, none, apperr.New(apperr.CodeInvalid, "give exactly one of template_version_id or custom")
	}
	if custom != nil {
		if err := custom.Validate(); err != nil {
			return nil, none, none, none, apperr.New(apperr.CodeInvalid, "custom: %v", err)
		}
		return custom, none, none, sql.NullString{String: mustJSON(custom), Valid: true}, nil
	}
	vr, err := q.GetEventTemplateVersion(ctx, versionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, none, none, none, apperr.New(apperr.CodeInvalid, "template version %q not found", versionID)
	} else if err != nil {
		return nil, none, none, none, err
	}
	if vr.Status != "published" {
		return nil, none, none, none, apperr.New(apperr.CodeInvalid, "subscriptions use published template versions only")
	}
	v, err := versionFromRow(vr)
	if err != nil {
		return nil, none, none, none, err
	}
	return &v.Definition, sql.NullString{String: v.TemplateID, Valid: true}, sql.NullString{String: v.ID, Valid: true}, none, nil
}

// CreateSubscription validates and binds a definition before anything is activated (SM-EVT-006).
func (svc *Service) CreateSubscription(ctx context.Context, actor audit.Actor, spaceID string, in SubscriptionInput) (*Subscription, error) {
	if in.Name == "" || len(in.Name) > 80 {
		return nil, apperr.New(apperr.CodeInvalid, "name is required (at most 80 characters)")
	}
	if in.Follow == "" {
		in.Follow = "pinned"
	}
	if in.Follow != "pinned" && in.Follow != "latest" {
		return nil, apperr.New(apperr.CodeInvalid, "follow must be pinned or latest")
	}
	if err := validCursor(&in.Start); err != nil {
		return nil, err
	}
	id := ids.New(ids.Subscription)
	err := svc.s.W().TxNamed(ctx, "events.subscription_create", func(tx *store.Tx) error {
		sp, err := tx.Q.GetSpace(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("space")
		} else if err != nil {
			return err
		}
		if sp.State == "archived" {
			return apperr.New(apperr.CodePrecondition, "the space is archived")
		}
		if _, err := tx.Q.GetEventConnection(ctx, in.ConnectionID); errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeInvalid, "connection %q not found", in.ConnectionID)
		}
		def, tmplID, verID, custom, err := svc.resolve(ctx, tx.Q, in.TemplateVersionID, in.Custom)
		if err != nil {
			return err
		}
		if custom.Valid && in.Follow == "latest" {
			return apperr.New(apperr.CodeInvalid, "custom definitions cannot follow a template")
		}
		b, err := def.Bind(in.Vars)
		if err != nil {
			return apperr.New(apperr.CodeInvalid, "%v", err)
		}
		policy, st, ss := cursorCols(in.Start)
		if err := tx.Q.InsertEventSubscription(ctx, db.InsertEventSubscriptionParams{ID: id, SpaceID: spaceID, Name: in.Name, ConnectionID: in.ConnectionID,
			TemplateID: tmplID, TemplateVersionID: verID, Follow: in.Follow, CustomJson: custom, VarsJson: mustJSON(b.Vars), EffectiveSubject: b.Subject,
			Durable: durableFor(id), Enabled: 1, StartPolicy: policy, StartTime: st, StartSeq: ss, CreatedAt: tx.NowMicros(), CreatedBy: actor.ID,
			UpdatedAt: tx.NowMicros()}); err != nil {
			if isUnique(err) {
				return apperr.New(apperr.CodeAlreadyExists, "this space already has a subscription named %q", in.Name)
			}
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "event_subscription", id, "created", 1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "event_subscription.created", Target: audit.Target{Type: "event_subscription", ID: id},
			Details: map[string]any{"space_id": spaceID, "subject": b.Subject, "durable": durableFor(id), "start": policy, "custom": custom.Valid}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.notify()
	return svc.GetSubscription(ctx, spaceID, id)
}

// UpdateSubscription edits a subscription. A changed subject requires an explicit cursor; a cursor given without a
// subject change is applied as well (recreating the consumer).
func (svc *Service) UpdateSubscription(ctx context.Context, actor audit.Actor, spaceID, id string, version int64, in SubscriptionUpdate) (*Subscription, error) {
	if in.Start != nil {
		if err := validCursor(in.Start); err != nil {
			return nil, err
		}
	}
	err := svc.s.W().TxNamed(ctx, "events.subscription_update", func(tx *store.Tx) error {
		cur, err := tx.Q.GetEventSubscription(ctx, id)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && cur.SpaceID != spaceID) {
			return notFound("subscription")
		} else if err != nil {
			return err
		}
		if cur.Version != version {
			return apperr.New(apperr.CodePrecondition, "the subscription changed since version %d", version)
		}
		versionID := in.TemplateVersionID
		if versionID == "" && in.Custom == nil {
			versionID, in.Custom = cur.TemplateVersionID.String, nil
			if cur.CustomJson.Valid {
				var d template.Definition
				_ = json.Unmarshal([]byte(cur.CustomJson.String), &d)
				in.Custom, versionID = &d, ""
			}
		}
		def, _, verID, custom, err := svc.resolve(ctx, tx.Q, versionID, in.Custom)
		if err != nil {
			return err
		}
		follow := in.Follow
		if follow == "" {
			follow = cur.Follow
		}
		if custom.Valid {
			follow = "pinned"
		}
		vars := in.Vars
		if vars == nil {
			_ = json.Unmarshal([]byte(cur.VarsJson), &vars)
		}
		b, err := def.Bind(vars)
		if err != nil {
			return apperr.New(apperr.CodeInvalid, "%v", err)
		}
		gen, replay := cur.CursorGeneration, cur.Replay
		policy, st, ss := cur.StartPolicy, cur.StartTime, cur.StartSeq
		if b.Subject != cur.EffectiveSubject && in.Start == nil {
			return apperr.New(apperr.CodeInvalid, "the subject changes from %s to %s: choose where the new consumer starts (start.policy)", cur.EffectiveSubject, b.Subject)
		}
		if in.Start != nil {
			gen++
			policy, st, ss = cursorCols(*in.Start)
			replay = b2i(in.Start.Policy != StartNew)
		}
		name := in.Name
		if name == "" {
			name = cur.Name
		}
		if _, err := tx.Q.UpdateEventSubscription(ctx, db.UpdateEventSubscriptionParams{Name: name, TemplateVersionID: verID, Follow: follow, CustomJson: custom,
			VarsJson: mustJSON(b.Vars), EffectiveSubject: b.Subject, Enabled: b2i(in.Enabled), CursorGeneration: gen, StartPolicy: policy, StartTime: st,
			StartSeq: ss, Replay: replay, Now: tx.NowMicros(), ID: id, Version: version}); err != nil {
			if isUnique(err) {
				return apperr.New(apperr.CodeAlreadyExists, "this space already has a subscription named %q", name)
			}
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "event_subscription", id, "updated", version+1); err != nil {
			return err
		}
		details := map[string]any{"subject": b.Subject, "enabled": in.Enabled}
		if in.Start != nil {
			details["cursor"] = policy
			details["replay"] = replay == 1
			if replay == 1 {
				// A cursor change that re-delivers events is a replay like any other: it is in the replay history.
				rid, err := recordReplay(ctx, tx, actor, cur, gen, *in.Start, "cursor changed with the subscription", false)
				if err != nil {
					return err
				}
				details["replay_id"] = rid
			}
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "event_subscription.updated", Target: audit.Target{Type: "event_subscription", ID: id},
			Details: details})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.notify()
	return svc.GetSubscription(ctx, spaceID, id)
}

// DeleteSubscription removes a subscription; the manager stops its consumer (the consumer itself is left for the Pulse
// operator, who also holds its grant).
func (svc *Service) DeleteSubscription(ctx context.Context, actor audit.Actor, spaceID, id string) error {
	err := svc.s.W().TxNamed(ctx, "events.subscription_delete", func(tx *store.Tx) error {
		cur, err := tx.Q.GetEventSubscription(ctx, id)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && cur.SpaceID != spaceID) {
			return notFound("subscription")
		} else if err != nil {
			return err
		}
		if _, err := tx.Q.DeleteEventSubscription(ctx, id); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "event_subscription", id, "deleted", cur.Version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "event_subscription.deleted", Target: audit.Target{Type: "event_subscription", ID: id},
			Details: map[string]any{"space_id": spaceID, "durable": cur.Durable}})
		return err
	})
	if err == nil {
		svc.notify()
	}
	return err
}

func (svc *Service) subFromRow(ctx context.Context, r db.EventSubscription) (*Subscription, error) {
	s := &Subscription{ID: r.ID, SpaceID: r.SpaceID, Name: r.Name, ConnectionID: r.ConnectionID, Follow: r.Follow, Subject: r.EffectiveSubject,
		Durable: r.Durable, Enabled: r.Enabled == 1, Version: r.Version, CreatedAt: fromMicros(r.CreatedAt), Vars: map[string]string{},
		Pending: r.AppliedGeneration < r.CursorGeneration, Cursor: Cursor{Policy: r.StartPolicy, Replay: r.Replay == 1, Generation: r.CursorGeneration}}
	_ = json.Unmarshal([]byte(r.VarsJson), &s.Vars)
	if r.StartTime.Valid {
		t := fromMicros(r.StartTime.Int64)
		s.Cursor.Time = &t
	}
	if r.StartSeq.Valid {
		s.Cursor.Seq = uint64(r.StartSeq.Int64) //nolint:gosec // stored from a uint64
	}
	if r.LastEventAt.Valid {
		t := fromMicros(r.LastEventAt.Int64)
		s.LastEventAt = &t
	}
	def, ref, err := svc.definitionOf(ctx, svc.s.R(), r)
	if err != nil {
		return nil, err
	}
	if ref != nil {
		s.Template = ref
	} else {
		s.Custom = def
	}
	if c, err := svc.s.R().GetEventConnection(ctx, r.ConnectionID); err == nil {
		s.Consumer = c.ClientID + "_" + r.Durable
	}
	s.Grant = "durable:" + r.EffectiveSubject + ":" + r.Durable
	if s.Quarantined, err = svc.s.R().CountOpenQuarantine(ctx, r.ID); err != nil {
		return nil, err
	}
	s.State, s.LastError = svc.runtimeState(r.ID, s.Enabled)
	if rows, err := svc.s.R().ListSubscriptionReplays(ctx, db.ListSubscriptionReplaysParams{SubscriptionID: r.ID, Limit: 1}); err == nil && len(rows) == 1 {
		s.LastReplay = replayFromRow(rows[0], r.CursorGeneration)
	}
	return s, nil
}

// GetSubscription returns a subscription of a space.
func (svc *Service) GetSubscription(ctx context.Context, spaceID, id string) (*Subscription, error) {
	r, err := svc.s.R().GetEventSubscription(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && r.SpaceID != spaceID) {
		return nil, notFound("subscription")
	} else if err != nil {
		return nil, err
	}
	return svc.subFromRow(ctx, r)
}

// ListSubscriptions returns a space's subscriptions.
func (svc *Service) ListSubscriptions(ctx context.Context, spaceID string) ([]*Subscription, error) {
	rows, err := svc.s.R().ListEventSubscriptionsBySpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]*Subscription, 0, len(rows))
	for _, r := range rows {
		s, err := svc.subFromRow(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
