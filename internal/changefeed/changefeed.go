// Package changefeed records committed changes of watched resources in the durable `changes` table (SM-API-002
// durable watches). A row is written in the same transaction as the change; after commit the in-process bus wakes
// open watch streams, which then read from the table, so a slow or reconnecting client misses nothing that is
// still retained and sees an explicit gap for anything pruned.
package changefeed

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Actions.
const (
	Created = "created"
	Updated = "updated"
	Deleted = "deleted"
)

// Change is one feed entry. It carries identifiers only; clients re-read the resource through the API, which
// applies authorization again.
type Change struct {
	Seq          int64  `json:"seq"`
	At           int64  `json:"at" doc:"Unix microseconds"`
	SpaceID      string `json:"space_id,omitempty"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Action       string `json:"action"`
	Version      int64  `json:"version,omitempty"`
}

// Recorder appends changes.
type Recorder struct{ bus *eventbus.Bus }

// New returns a recorder publishing wake-ups on bus (nil disables wake-ups; streams still catch up on reconnect).
func New(bus *eventbus.Bus) *Recorder { return &Recorder{bus: bus} }

// Record appends a change inside tx. spaceID is "" for global resources.
func (r *Recorder) Record(ctx context.Context, tx *store.Tx, spaceID, resourceType, resourceID, action string, version int64) error {
	seq, err := tx.Q.InsertChange(ctx, db.InsertChangeParams{At: tx.NowMicros(), SpaceID: sql.NullString{String: spaceID, Valid: spaceID != ""},
		ResourceType: resourceType, ResourceID: resourceID, Action: action, Version: version})
	if err != nil {
		return err
	}
	if r != nil && r.bus != nil {
		tx.AfterCommit(func() { r.bus.Publish(eventbus.Event{Topic: eventbus.TopicChange, Key: strconv.FormatInt(seq, 10)}) })
	}
	return nil
}

// FromRow converts a stored row.
func FromRow(c db.Change) Change {
	return Change{Seq: c.Seq, At: c.At, SpaceID: c.SpaceID.String, ResourceType: c.ResourceType, ResourceID: c.ResourceID, Action: c.Action,
		Version: c.Version}
}
