package client

import (
	"context"
)

// ---- cross-space settings and peers (ADR-0038) ----

// XSpaceSettings are a space's cross-space settings; Version is 0 until they are first saved.
type XSpaceSettings struct {
	SpaceID            string `json:"space_id"`
	Profile            string `json:"profile"`
	MaxDepth           int64  `json:"max_depth"`
	MaxOutgoingPerHour int64  `json:"max_outgoing_per_hour"`
	Version            int64  `json:"version"`
}

// XSpaceSettingsInput replaces a space's settings (zero values take SpaceMan's defaults).
type XSpaceSettingsInput struct {
	Profile            string `json:"profile,omitempty"`
	MaxDepth           int64  `json:"max_depth,omitempty"`
	MaxOutgoingPerHour int64  `json:"max_outgoing_per_hour,omitempty"`
}

// GetXSpaceSettings is GET /spaces/{id}/xspace/settings (SpaceMan's defaults when never saved).
func (c *Client) GetXSpaceSettings(ctx context.Context, spaceID string) (*XSpaceSettings, error) {
	var out XSpaceSettings
	return &out, c.get(ctx, "/spaces/"+esc(spaceID)+"/xspace/settings", &out)
}

// SetXSpaceSettings is PUT /spaces/{id}/xspace/settings; version 0 (never saved) sends no If-Match.
func (c *Client) SetXSpaceSettings(ctx context.Context, spaceID string, version int64, in XSpaceSettingsInput) (*XSpaceSettings, error) {
	var out XSpaceSettings
	return &out, c.put(ctx, "/spaces/"+esc(spaceID)+"/xspace/settings", version, in, &out)
}

// XSpaceInbound is what a space accepts from a peer.
type XSpaceInbound struct {
	Discover   bool     `json:"discover"`
	Questions  bool     `json:"questions"`
	Delegation bool     `json:"delegation"`
	Knowledge  bool     `json:"knowledge"`
	KBs        []string `json:"kbs"`
	Disclosure string   `json:"disclosure"`
	MaxPerHour int64    `json:"max_per_hour"`
	MaxPending int64    `json:"max_pending"`
}

// XSpaceOutbound is what a space's control may send to a peer.
type XSpaceOutbound struct {
	Questions  bool `json:"questions"`
	Delegation bool `json:"delegation"`
	Knowledge  bool `json:"knowledge"`
}

// XSpacePeer is a space's configuration for one peer space.
type XSpacePeer struct {
	SpaceID string `json:"space_id"`
	Peer    struct {
		ID string `json:"id"`
	} `json:"peer"`
	Inbound  XSpaceInbound  `json:"inbound"`
	Outbound XSpaceOutbound `json:"outbound"`
	Version  int64          `json:"version"`
}

// XSpacePeerInput is PUT /spaces/{id}/xspace/peers/{peer}.
type XSpacePeerInput struct {
	Inbound  XSpaceInbound  `json:"inbound"`
	Outbound XSpaceOutbound `json:"outbound"`
}

// GetXSpacePeer finds a space's configuration for a peer (the API lists them; it has no single read).
func (c *Client) GetXSpacePeer(ctx context.Context, spaceID, peerID string) (*XSpacePeer, error) {
	var out items[*XSpacePeer]
	if err := c.get(ctx, "/spaces/"+esc(spaceID)+"/xspace/peers", &out); err != nil {
		return nil, err
	}
	for _, p := range out.Items {
		if p.Peer.ID == peerID {
			return p, nil
		}
	}
	return nil, notFound("GET /spaces/{space_id}/xspace/peers", "configuration for peer "+peerID)
}

// SetXSpacePeer is PUT /spaces/{id}/xspace/peers/{peer}: version 0 creates (and fails when one exists), otherwise it
// must match.
func (c *Client) SetXSpacePeer(ctx context.Context, spaceID, peerID string, version int64, in XSpacePeerInput) (*XSpacePeer, error) {
	if in.Inbound.KBs == nil {
		in.Inbound.KBs = []string{}
	}
	var out XSpacePeer
	return &out, c.put(ctx, "/spaces/"+esc(spaceID)+"/xspace/peers/"+esc(peerID), version, in, &out)
}

// DeleteXSpacePeer is DELETE /spaces/{id}/xspace/peers/{peer}.
func (c *Client) DeleteXSpacePeer(ctx context.Context, spaceID, peerID string) error {
	return c.del(ctx, "/spaces/"+esc(spaceID)+"/xspace/peers/"+esc(peerID))
}

// ---- human-action policies (ADR-0032) ----

// HumanPolicy is a stored human-action policy, or an effective one with its source.
type HumanPolicy struct {
	HumanPolicyFields
	ID         string `json:"id,omitempty"`
	SpaceID    string `json:"space_id,omitempty"`
	ActionType string `json:"action_type"`
	Source     string `json:"source,omitempty"`
	Version    int64  `json:"version,omitempty"`
}

// HumanPolicyFields is what a policy decides; PUT replaces all of it.
type HumanPolicyFields struct {
	RemindAfterMinutes   int64  `json:"remind_after_minutes"`
	RemindEveryMinutes   int64  `json:"remind_every_minutes"`
	MaxReminders         int64  `json:"max_reminders"`
	EscalateAfterMinutes int64  `json:"escalate_after_minutes"`
	EscalatePriority     string `json:"escalate_priority"`
	EscalateAssignee     string `json:"escalate_assignee"`
	RequesterEnded       string `json:"requester_ended"`
	HoldMinutes          int64  `json:"hold_minutes"`
	SuccessorRole        string `json:"successor_role"`
	WakeIdle             bool   `json:"wake_idle"`
	RatePerRunHour       int64  `json:"rate_per_run_hour"`
	RatePerSpaceHour     int64  `json:"rate_per_space_hour"`
}

// HumanPolicies is a scope's stored policies and the policy in force for each type.
type HumanPolicies struct {
	Items     []*HumanPolicy `json:"items"`
	Effective []*HumanPolicy `json:"effective"`
}

func humanPolicyPath(spaceID string) string {
	if spaceID == "" {
		return "/human-policies"
	}
	return "/spaces/" + esc(spaceID) + "/human-policies"
}

// ListHumanPolicies is GET /human-policies (spaceID empty: installation defaults) or /spaces/{id}/human-policies.
func (c *Client) ListHumanPolicies(ctx context.Context, spaceID string) (*HumanPolicies, error) {
	var out HumanPolicies
	return &out, c.get(ctx, humanPolicyPath(spaceID), &out)
}

// GetHumanPolicy finds the stored policy of a type in a scope.
func (c *Client) GetHumanPolicy(ctx context.Context, spaceID, actionType string) (*HumanPolicy, error) {
	l, err := c.ListHumanPolicies(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	for _, p := range l.Items {
		if p.ActionType == actionType {
			return p, nil
		}
	}
	op := "GET /human-policies"
	if spaceID != "" {
		op = "GET /spaces/{space_id}/human-policies"
	}
	return nil, notFound(op, "policy for "+actionType)
}

// SetHumanPolicy is PUT …/human-policies/{type}: version 0 creates or overwrites, otherwise it must match.
func (c *Client) SetHumanPolicy(ctx context.Context, spaceID, actionType string, version int64, in HumanPolicyFields) (*HumanPolicy, error) {
	var out HumanPolicy
	return &out, c.put(ctx, humanPolicyPath(spaceID)+"/"+esc(actionType), version, in, &out)
}

// DeleteHumanPolicy is DELETE …/human-policies/{type}.
func (c *Client) DeleteHumanPolicy(ctx context.Context, spaceID, actionType string) error {
	return c.del(ctx, humanPolicyPath(spaceID)+"/"+esc(actionType))
}
