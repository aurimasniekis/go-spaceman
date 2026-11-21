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
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Scopes of ingest_own (ADR-0106, amending ADR-0104).
const (
	// ScopeSpace (the default) admits an own dialog event only when its data.space_id is the subscription's space.
	ScopeSpace = "space"
	// ScopeFleet also admits the dialog events of every other space, for a space that holds an operator's fleet-scope
	// grant. It changes nothing else: the subjects stay the two dialog events, the depth stays 0.
	ScopeFleet = "fleet"
)

// fleetDataFields are the only data fields a fleet-scoped subscription ingests (ADR-0106 decision 4): what
// identifies the space, the run and the dialog occurrence, the dialog's registry ID and signal, and when. A run
// dialog event carries more (node, role, harness, instance, lease generation, harness version); those describe how
// another space runs and stay out of the operations space's inbox.
var fleetDataFields = []string{"space_id", "run_id", "dialog_id", "dialog", "signal", "detected_at", "cleared_at"}

// fleetData returns data reduced to fleetDataFields. data was already read as a JSON object by admitOwn.
func fleetData(data json.RawMessage) (json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(fleetDataFields))
	for _, k := range fleetDataFields {
		if v, ok := all[k]; ok {
			out[k] = v
		}
	}
	return json.Marshal(out)
}

// FleetScopeGrant allows one space's subscriptions to use ingest_own_scope fleet (ADR-0106). Only an installation
// administrator grants or revokes it.
type FleetScopeGrant struct {
	SpaceID   string    `json:"space_id"`
	Reason    string    `json:"reason,omitempty" doc:"Why the space holds the grant, as the administrator gave it"`
	GrantedAt time.Time `json:"granted_at"`
	GrantedBy string    `json:"granted_by"`
	// FleetSubscriptions counts the space's subscriptions with ingest_own_scope fleet.
	FleetSubscriptions int64 `json:"fleet_subscriptions" doc:"The space's subscriptions with ingest_own_scope fleet"`
}

func grantFromRow(r db.EventFleetScopeGrant, n int64) *FleetScopeGrant {
	return &FleetScopeGrant{SpaceID: r.SpaceID, Reason: r.Reason, GrantedAt: fromMicros(r.GrantedAt), GrantedBy: r.GrantedBy, FleetSubscriptions: n}
}

// ListFleetScopeGrants returns every fleet-scope grant.
func (svc *Service) ListFleetScopeGrants(ctx context.Context) ([]*FleetScopeGrant, error) {
	q := svc.s.R()
	rows, err := q.ListEventFleetScopeGrants(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*FleetScopeGrant, 0, len(rows))
	for _, r := range rows {
		n, err := q.CountFleetScopeSubscriptions(ctx, r.SpaceID)
		if err != nil {
			return nil, err
		}
		out = append(out, grantFromRow(r, n))
	}
	return out, nil
}

// GetFleetScopeGrant returns a space's grant, or not_found when it holds none.
func (svc *Service) GetFleetScopeGrant(ctx context.Context, spaceID string) (*FleetScopeGrant, error) {
	q := svc.s.R()
	r, err := q.GetEventFleetScopeGrant(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("fleet-scope grant")
	} else if err != nil {
		return nil, err
	}
	n, err := q.CountFleetScopeSubscriptions(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	return grantFromRow(r, n), nil
}

// GrantFleetScope gives a space the fleet-scope grant, or replaces its reason (idempotent). The space must exist and
// not be archived.
func (svc *Service) GrantFleetScope(ctx context.Context, actor audit.Actor, spaceID, reason string) (*FleetScopeGrant, error) {
	reason = strings.TrimSpace(reason)
	err := svc.s.W().TxNamed(ctx, "events.fleet_scope_grant", func(tx *store.Tx) error {
		sp, err := tx.Q.GetSpace(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("space")
		} else if err != nil {
			return err
		}
		if sp.State == "archived" {
			return apperr.New(apperr.CodePrecondition, "the space is archived")
		}
		_, prev := tx.Q.GetEventFleetScopeGrant(ctx, spaceID)
		if err := tx.Q.UpsertEventFleetScopeGrant(ctx, db.UpsertEventFleetScopeGrantParams{SpaceID: spaceID, Reason: reason, GrantedAt: tx.NowMicros(),
			GrantedBy: actor.ID}); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "event_fleet_scope.granted", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"space_id": spaceID, "reason": reason, "replaced": prev == nil}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.GetFleetScopeGrant(ctx, spaceID)
}

// RevokeFleetScope removes a space's grant. Its fleet-scoped subscriptions keep their setting but stop admitting other
// spaces' events with the next event (admitOwn reads the grant per event); same-space dialog events are still
// ingested, as with scope space. Updating such a subscription is refused until its scope goes back to space.
func (svc *Service) RevokeFleetScope(ctx context.Context, actor audit.Actor, spaceID string) error {
	return svc.s.W().TxNamed(ctx, "events.fleet_scope_revoke", func(tx *store.Tx) error {
		n, err := tx.Q.DeleteEventFleetScopeGrant(ctx, spaceID)
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("fleet-scope grant")
		}
		subs, err := tx.Q.CountFleetScopeSubscriptions(ctx, spaceID)
		if err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "event_fleet_scope.revoked", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"space_id": spaceID, "fleet_subscriptions": subs}})
		return err
	})
}

// checkScope validates a subscription's ingest_own_scope against its ingest_own and its space's grant, inside the
// transaction that writes the subscription. The grant is never something the subscription can claim: it is read here
// from the operator-managed table.
func checkScope(ctx context.Context, q *db.Queries, spaceID, scope string, ingestOwn bool) error {
	switch scope {
	case ScopeSpace:
		return nil
	case ScopeFleet:
	default:
		return apperr.New(apperr.CodeInvalid, "ingest_own_scope must be space or fleet")
	}
	if !ingestOwn {
		return apperr.New(apperr.CodeInvalid, "ingest_own_scope fleet needs ingest_own")
	}
	if _, err := q.GetEventFleetScopeGrant(ctx, spaceID); errors.Is(err, sql.ErrNoRows) {
		return errNoFleetGrant()
	} else if err != nil {
		return err
	}
	return nil
}

func errNoFleetGrant() error {
	return apperr.New(apperr.CodeForbidden, "ingest_own_scope fleet is not allowed: this space holds no fleet-scope grant; "+
		"only an installation administrator can grant one (PUT /event-fleet-scope-grants/{space_id}), "+
		"or set ingest_own_scope to space")
}
