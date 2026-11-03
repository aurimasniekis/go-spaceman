package client

import (
	"context"
	"net/http"
	"time"
)

// ---- limits (ADR-0036) ----

// Limit is one effective limit of a space (GET /spaces/{id}/limits).
type Limit struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Unit        string `json:"unit"`
	Min         int64  `json:"min"`
	Max         int64  `json:"max"`
	ZeroNone    bool   `json:"zero_means_none"`
	Overridable bool   `json:"overridable"`
	EditAt      string `json:"edit_at,omitempty"`
	Value       int64  `json:"value"`
	Default     int64  `json:"default"`
	Override    *int64 `json:"override,omitempty"`
	Source      string `json:"source"`
}

// SpaceLimits is a space's effective limits; Version is the version of its stored overrides (0: none stored).
type SpaceLimits struct {
	SpaceID string  `json:"space_id"`
	Limits  []Limit `json:"limits"`
	OnLower string  `json:"on_lower"`
	Version int64   `json:"version"`
}

// Overrides returns the space's own values by key.
func (l *SpaceLimits) Overrides() map[string]int64 {
	out := map[string]int64{}
	for _, x := range l.Limits {
		if x.Override != nil {
			out[x.Key] = *x.Override
		}
	}
	return out
}

// SpaceLimitsInput replaces a space's overrides as a whole (PUT /spaces/{id}/limits): a key left out inherits the
// installation default.
type SpaceLimitsInput struct {
	Values  map[string]int64 `json:"values"`
	OnLower string           `json:"on_lower,omitempty"`
}

// GetSpaceLimits is GET /spaces/{id}/limits.
func (c *Client) GetSpaceLimits(ctx context.Context, spaceID string) (*SpaceLimits, error) {
	var out SpaceLimits
	return &out, c.get(ctx, "/spaces/"+esc(spaceID)+"/limits", &out)
}

// SetSpaceLimits is PUT /spaces/{id}/limits. version is the stored overrides' version; 0 (none stored) sends no
// If-Match.
func (c *Client) SetSpaceLimits(ctx context.Context, spaceID string, version int64, in SpaceLimitsInput) (*SpaceLimits, error) {
	if in.Values == nil {
		in.Values = map[string]int64{}
	}
	var out SpaceLimits
	return &out, c.put(ctx, "/spaces/"+esc(spaceID)+"/limits", version, in, &out)
}

// ---- queue policy ----

// QueuePolicy is a space's queue policy (it holds the top-level task limit). The API has no If-Match for it.
type QueuePolicy struct {
	MaxActive          int64  `json:"max_active"`
	IncidentInsertion  string `json:"incident_insertion"`
	IncidentPreempts   bool   `json:"incident_preempts"`
	EscalationReorders bool   `json:"escalation_reorders"`
	Version            int64  `json:"version"`
}

// GetQueuePolicy reads the policy from GET /spaces/{id}/tasks (the API has no separate read; the tasks are ignored).
func (c *Client) GetQueuePolicy(ctx context.Context, spaceID string) (*QueuePolicy, error) {
	var out struct {
		Policy QueuePolicy `json:"policy"`
	}
	if err := c.get(ctx, "/spaces/"+esc(spaceID)+"/tasks", &out); err != nil {
		return nil, err
	}
	return &out.Policy, nil
}

// SetQueuePolicy is PUT /spaces/{id}/queue/policy.
func (c *Client) SetQueuePolicy(ctx context.Context, spaceID string, in QueuePolicy) (*QueuePolicy, error) {
	var out QueuePolicy
	return &out, c.put(ctx, "/spaces/"+esc(spaceID)+"/queue/policy", 0, in, &out)
}

// ---- schedules ----

// Schedule is a space schedule (GET /spaces/{id}/schedules/{schedule}).
type Schedule struct {
	ID              string    `json:"id"`
	SpaceID         string    `json:"space_id"`
	Name            string    `json:"name"`
	Description     string    `json:"description"`
	Enabled         bool      `json:"enabled"`
	Kind            string    `json:"kind"`
	Cron            string    `json:"cron"`
	IntervalSeconds int64     `json:"interval_seconds"`
	Anchor          time.Time `json:"anchor"`
	Timezone        string    `json:"timezone"`
	Missed          string    `json:"missed"`
	CatchUpMax      int64     `json:"catch_up_max"`
	SkipIfOpen      bool      `json:"skip_if_open"`
	Target          string    `json:"target"`
	Title           string    `json:"title"`
	Body            string    `json:"body"`
	Priority        string    `json:"priority"`
	AssigneeRole    string    `json:"assignee_role"`
	Version         int64     `json:"version"`
}

// ScheduleInput creates or replaces a schedule. A nil Anchor keeps the current one while the kind and interval stay
// the same (otherwise the anchor becomes now).
type ScheduleInput struct {
	Name            string     `json:"name"`
	Description     string     `json:"description,omitempty"`
	Enabled         *bool      `json:"enabled,omitempty"`
	Kind            string     `json:"kind"`
	Cron            string     `json:"cron,omitempty"`
	IntervalSeconds int64      `json:"interval_seconds,omitempty"`
	Anchor          *time.Time `json:"anchor,omitempty"`
	Timezone        string     `json:"timezone,omitempty"`
	Missed          string     `json:"missed,omitempty"`
	CatchUpMax      int64      `json:"catch_up_max,omitempty"`
	SkipIfOpen      *bool      `json:"skip_if_open,omitempty"`
	Target          string     `json:"target"`
	Title           string     `json:"title"`
	Body            string     `json:"body,omitempty"`
	Priority        string     `json:"priority,omitempty"`
	AssigneeRole    string     `json:"assignee_role,omitempty"`
}

// GetSchedule is GET /spaces/{id}/schedules/{schedule}.
func (c *Client) GetSchedule(ctx context.Context, spaceID, id string) (*Schedule, error) {
	var out Schedule
	return &out, c.get(ctx, "/spaces/"+esc(spaceID)+"/schedules/"+esc(id), &out)
}

// CreateSchedule is POST /spaces/{id}/schedules. The operation takes no Idempotency-Key, so it is never retried.
func (c *Client) CreateSchedule(ctx context.Context, spaceID string, in ScheduleInput) (*Schedule, error) {
	var out Schedule
	return &out, c.do2(ctx, http.MethodPost, "/spaces/"+esc(spaceID)+"/schedules", 0, in, &out)
}

// UpdateSchedule is PUT /spaces/{id}/schedules/{schedule} (If-Match required).
func (c *Client) UpdateSchedule(ctx context.Context, spaceID, id string, version int64, in ScheduleInput) (*Schedule, error) {
	var out Schedule
	return &out, c.put(ctx, "/spaces/"+esc(spaceID)+"/schedules/"+esc(id), version, in, &out)
}

// DeleteSchedule is DELETE /spaces/{id}/schedules/{schedule}.
func (c *Client) DeleteSchedule(ctx context.Context, spaceID, id string) error {
	return c.del(ctx, "/spaces/"+esc(spaceID)+"/schedules/"+esc(id))
}
