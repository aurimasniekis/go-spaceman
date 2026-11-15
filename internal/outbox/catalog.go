package outbox

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Name identifies an event: subject evt.spaceman.<area>.<entity>.<action>, ce-type
// spaceman.<area>.<entity>_<action>.v<version> (Porch convention, ADR-0015). A payload change that is not additive
// gets a new version and a new type; the subject stays.
type Name struct {
	Area, Entity, Action string
	Version              int
}

// SubjectPrefix starts every subject SpaceMan publishes (the Pulse grant is publish:evt.spaceman.>).
const SubjectPrefix = "evt.spaceman."

// Subject returns the NATS subject.
func (n Name) Subject() string { return SubjectPrefix + n.Area + "." + n.Entity + "." + n.Action }

// Type returns the CloudEvents type.
func (n Name) Type() string {
	return "spaceman." + n.Area + "." + n.Entity + "_" + n.Action + ".v" + strconv.Itoa(n.Version)
}

var reSegment = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func (n Name) validate() error {
	for _, s := range []string{n.Area, n.Entity, n.Action} {
		if !reSegment.MatchString(s) {
			return fmt.Errorf("%w: name segment %q", ErrInvalid, s)
		}
	}
	if n.Version < 1 {
		return fmt.Errorf("%w: version %d", ErrInvalid, n.Version)
	}
	if len(n.Subject()) > 256 {
		return fmt.Errorf("%w: subject too long", ErrInvalid)
	}
	return nil
}

// Payload is a catalog payload. Only this package defines payloads, so every published event is in the catalog and
// carries identifiers, states and outcomes only (never titles, bodies, requests, responses, prompts or secrets).
type Payload interface {
	eventName() Name
	spaceID() string
}

// The event catalog (site/guide/publishing-events.md).
var (
	RunLaunchedName        = Name{"fleet", "run", "launched", 1}
	RunStartedName         = Name{"fleet", "run", "started", 1}
	RunEndedName           = Name{"fleet", "run", "ended", 1}
	UsageLimitReachedName  = Name{"fleet", "usage_limit", "reached", 1}
	UsageLimitClearedName  = Name{"fleet", "usage_limit", "cleared", 1}
	TaskCreatedName        = Name{"queue", "task", "created", 1}
	TaskStatusChangedName  = Name{"queue", "task", "status_changed", 1}
	InboxItemCreatedName   = Name{"inbox", "item", "created", 1}
	HumanCreatedName       = Name{"human", "action", "created", 1}
	HumanResolvedName      = Name{"human", "action", "resolved", 1}
	HumanCancelledName     = Name{"human", "action", "cancelled", 1}
	HumanExpiredName       = Name{"human", "action", "expired", 1}
	HumanRemindedName      = Name{"human", "action", "reminded", 1}
	HumanEscalatedName     = Name{"human", "action", "escalated", 1}
	HumanReroutedName      = Name{"human", "action", "rerouted", 1}
	ApprovalDecidedName    = Name{"approvals", "approval", "decided", 1}
	ApprovalExecutedName   = Name{"approvals", "approval", "executed", 1}
	MemorySyncedName       = Name{"memory", "collection", "synced", 1}
	KBProposalAcceptedName = Name{"kb", "proposal", "accepted", 1}
	XSpaceCreatedName      = Name{"xspace", "request", "created", 1}
	XSpaceStateName        = Name{"xspace", "request", "state_changed", 1}
)

// Run describes a run in run lifecycle events.
type Run struct {
	SpaceID    string `json:"space_id"`
	RunID      string `json:"run_id"`
	NodeID     string `json:"node_id"`
	Kind       string `json:"kind"` // control, worker, kb, dreamer, diagnostic
	Role       string `json:"role"`
	Harness    string `json:"harness"`
	InstanceID string `json:"instance_id"`
	Generation int64  `json:"lease_generation"`
}

// RunLaunched: central accepted a launch and queued it for the space's node.
type RunLaunched struct {
	Run
}

// RunStarted: the node reported the run's process running for the first time.
type RunStarted struct {
	Run
	Adopted bool `json:"adopted"` // an existing process was adopted instead of launched
}

// RunEnded: the run ended (exited, failed or lost). Error text and terminal output are never included.
type RunEnded struct {
	Run
	State     string `json:"state"` // exited, failed, lost
	EndReason string `json:"end_reason,omitempty"`
	// NodeReason is the node's own end reason when central recorded another (usage_limit, ADR-0079).
	NodeReason string `json:"node_reason,omitempty"`
	ExitCode   *int64 `json:"exit_code,omitempty"`
}

// UsageLimit describes an account hold (ADR-0079): one exhausted account (or gateway) window as one space saw it.
// It carries no transcript text.
type UsageLimit struct {
	SpaceID    string     `json:"space_id"`
	LimitID    string     `json:"limit_id"`
	Scope      string     `json:"scope"` // account, gateway
	AccountID  string     `json:"account_id,omitempty"`
	GatewayRef string     `json:"gateway_ref,omitempty"`
	Kind       string     `json:"kind"`
	ResetsAt   *time.Time `json:"resets_at,omitempty"` // absent: unknown
	RunID      string     `json:"run_id,omitempty"`    // the run whose report opened it
}

// UsageLimitReached: a run's report opened a hold on its account (or gateway reference) in the run's space. Published
// once per hold, not per report.
type UsageLimitReached struct {
	UsageLimit
}

// UsageLimitCleared: a hold was cleared (a run on the account reported the limit over).
type UsageLimitCleared struct {
	UsageLimit
	ClearedBy string `json:"cleared_by"` // run:<id> (a run of this space), evidence (a run of another space), or who cleared it
}

// Task describes a queue task (never its title, objective, acceptance criteria, reason or result).
type Task struct {
	SpaceID      string `json:"space_id"`
	TaskID       string `json:"task_id"`
	ParentID     string `json:"parent_id,omitempty"`
	Kind         string `json:"kind"` // ordinary, incident
	Priority     string `json:"priority"`
	AssigneeRole string `json:"assignee_role,omitempty"`
	OriginKind   string `json:"origin_kind,omitempty"`
	OriginID     string `json:"origin_id,omitempty"`
}

// TaskCreated: a task was added to a space's queue.
type TaskCreated struct {
	Task
	Status string `json:"status"` // backlog or ready
}

// TaskStatusChanged: a task moved between queue states.
type TaskStatusChanged struct {
	Task
	From      string `json:"from"`
	To        string `json:"to"`
	Preempted bool   `json:"preempted"`
}

// InboxItemCreated: an item arrived in a space's inbox (never its title, body or payload).
type InboxItemCreated struct {
	SpaceID        string `json:"space_id"`
	ItemID         string `json:"item_id"`
	SourceKind     string `json:"source_kind"`
	SourceID       string `json:"source_id,omitempty"`
	SubscriptionID string `json:"subscription_id,omitempty"`
	Urgency        string `json:"urgency,omitempty"`
}

// HumanAction is the safe metadata of a human action (SM-OBS-004, SM-HUI-007): identifiers, type, status, outcome
// and priority. Titles, bodies, requests, responses, comments and reasons are never published.
type HumanAction struct {
	SpaceID       string `json:"space_id"`
	ActionID      string `json:"action_id"`
	Type          string `json:"type"` // question, approval, todo, message, custom
	TypeSlug      string `json:"type_slug,omitempty"`
	TypeVersion   int64  `json:"type_version,omitempty"`
	Status        string `json:"status"`
	Outcome       string `json:"outcome,omitempty"`
	Priority      string `json:"priority"`
	RequesterKind string `json:"requester_kind"`
	TaskID        string `json:"task_id,omitempty"`
	BlocksTask    bool   `json:"blocks_task"`
	FollowUpOf    string `json:"follow_up_of,omitempty"`
}

// HumanActionCreated: a request for a person was created.
type HumanActionCreated struct{ HumanAction }

// HumanActionResolved: a person responded (Outcome says how).
type HumanActionResolved struct{ HumanAction }

// HumanActionCancelled: the request was withdrawn.
type HumanActionCancelled struct{ HumanAction }

// HumanActionExpired: nobody responded before the expiry.
type HumanActionExpired struct{ HumanAction }

// HumanActionReminded: people were reminded of a pending action (ReminderCount says how often so far).
type HumanActionReminded struct {
	HumanAction
	ReminderCount int64 `json:"reminder_count"`
}

// HumanActionEscalated: a pending action escalated per policy (Priority is the new one).
type HumanActionEscalated struct {
	HumanAction
	FromPriority string `json:"from_priority"`
	Reassigned   bool   `json:"reassigned"`
}

// HumanActionRerouted: the notification of an outcome was routed away from the requester's run (To is run, instance,
// control or operators; Reason a fixed code). Never the response.
type HumanActionRerouted struct {
	HumanAction
	NotificationID string `json:"notification_id"`
	To             string `json:"to"`
	RouteReason    string `json:"route_reason"`
}

// ApprovalDecided: an enforced approval was approved, denied, expired or withdrawn (never its arguments or resource).
type ApprovalDecided struct {
	SpaceID       string `json:"space_id"`
	ApprovalID    string `json:"approval_id"`
	HumanActionID string `json:"human_action_id,omitempty"`
	Operation     string `json:"operation"`
	State         string `json:"state"` // approved, denied, expired, revoked
	RequesterRun  string `json:"requester_run,omitempty"`
	DeciderType   string `json:"decider_type,omitempty"`
}

// ApprovalExecuted: an approved operation ran (one use of an approval).
type ApprovalExecuted struct {
	SpaceID     string `json:"space_id"`
	ApprovalID  string `json:"approval_id"`
	ExecutionID string `json:"execution_id"`
	Operation   string `json:"operation"`
	Result      string `json:"result"`               // succeeded, failed
	ErrorCode   string `json:"error_code,omitempty"` // the error class, never its message
}

// MemorySynced: central preserved a new head of a role's memory (fast-forward).
type MemorySynced struct {
	SpaceID      string `json:"space_id"`
	CollectionID string `json:"collection_id"`
	Role         string `json:"role"`
	NodeID       string `json:"node_id"`
	FromCommit   string `json:"from_commit,omitempty"`
	ToCommit     string `json:"to_commit"`
}

// KBProposalAccepted: a knowledge-base proposal was merged into the KB's base branch.
type KBProposalAccepted struct {
	SpaceID      string `json:"space_id"`
	KBID         string `json:"kb_id"`
	ProposalID   string `json:"proposal_id"`
	SessionID    string `json:"session_id,omitempty"`
	Commit       string `json:"commit"`
	MergedCommit string `json:"merged_commit,omitempty"`
}

// XSpaceRequest is the safe metadata of a cross-space request (ADR-0038), published once for each of the two spaces
// (SpaceID with Side source or destination). Titles, bodies, replies and reasons are never published.
type XSpaceRequest struct {
	SpaceID       string `json:"space_id"`
	Side          string `json:"side"` // source or destination
	RequestID     string `json:"request_id"`
	SourceSpaceID string `json:"source_space_id"`
	DestSpaceID   string `json:"dest_space_id"`
	Kind          string `json:"kind"` // question, delegation, knowledge
	State         string `json:"state"`
	Depth         int64  `json:"depth"`
	CorrelationID string `json:"correlation_id"`
}

// XSpaceRequestCreated: a space's control asked another space.
type XSpaceRequestCreated struct{ XSpaceRequest }

// XSpaceRequestStateChanged: a cross-space request was accepted, declined, answered, failed, cancelled or expired.
type XSpaceRequestStateChanged struct {
	XSpaceRequest
	From string `json:"from"`
}

func (RunLaunched) eventName() Name          { return RunLaunchedName }
func (RunStarted) eventName() Name           { return RunStartedName }
func (RunEnded) eventName() Name             { return RunEndedName }
func (UsageLimitReached) eventName() Name    { return UsageLimitReachedName }
func (UsageLimitCleared) eventName() Name    { return UsageLimitClearedName }
func (TaskCreated) eventName() Name          { return TaskCreatedName }
func (TaskStatusChanged) eventName() Name    { return TaskStatusChangedName }
func (InboxItemCreated) eventName() Name     { return InboxItemCreatedName }
func (HumanActionCreated) eventName() Name   { return HumanCreatedName }
func (HumanActionResolved) eventName() Name  { return HumanResolvedName }
func (HumanActionCancelled) eventName() Name { return HumanCancelledName }
func (HumanActionExpired) eventName() Name   { return HumanExpiredName }
func (HumanActionReminded) eventName() Name  { return HumanRemindedName }
func (HumanActionEscalated) eventName() Name { return HumanEscalatedName }
func (HumanActionRerouted) eventName() Name  { return HumanReroutedName }
func (ApprovalDecided) eventName() Name      { return ApprovalDecidedName }
func (ApprovalExecuted) eventName() Name     { return ApprovalExecutedName }
func (MemorySynced) eventName() Name         { return MemorySyncedName }
func (KBProposalAccepted) eventName() Name   { return KBProposalAcceptedName }
func (XSpaceRequestCreated) eventName() Name { return XSpaceCreatedName }
func (XSpaceRequestStateChanged) eventName() Name {
	return XSpaceStateName
}

func (r Run) spaceID() string                { return r.SpaceID }
func (t Task) spaceID() string               { return t.SpaceID }
func (u UsageLimit) spaceID() string         { return u.SpaceID }
func (p InboxItemCreated) spaceID() string   { return p.SpaceID }
func (h HumanAction) spaceID() string        { return h.SpaceID }
func (p ApprovalDecided) spaceID() string    { return p.SpaceID }
func (p ApprovalExecuted) spaceID() string   { return p.SpaceID }
func (p MemorySynced) spaceID() string       { return p.SpaceID }
func (p KBProposalAccepted) spaceID() string { return p.SpaceID }
func (p XSpaceRequest) spaceID() string      { return p.SpaceID }

// HumanEvent returns the lifecycle event of a human action entering status (open/in_progress → created; resolved,
// cancelled, expired), or nil for a status that publishes nothing.
func HumanEvent(h HumanAction) Payload {
	switch h.Status {
	case "open", "in_progress":
		return HumanActionCreated{h}
	case "resolved":
		return HumanActionResolved{h}
	case "cancelled":
		return HumanActionCancelled{h}
	case "expired":
		return HumanActionExpired{h}
	}
	return nil
}

// CatalogEntry documents one event.
type CatalogEntry struct {
	Name    Name
	Example Payload
	Summary string
}

// Catalog lists every event SpaceMan publishes, with an example payload (documentation and tests).
func Catalog() []CatalogEntry {
	run := Run{SpaceID: "spc_…", RunID: "run_…", NodeID: "nod_…", Kind: "worker", Role: "dev", Harness: "claude-code", InstanceID: "ins_…", Generation: 3}
	task := Task{SpaceID: "spc_…", TaskID: "tsk_…", Kind: "ordinary", Priority: "normal", AssigneeRole: "dev", OriginKind: "inbox", OriginID: "inb_…"}
	hum := HumanAction{SpaceID: "spc_…", ActionID: "hum_…", Type: "question", Status: "open", Priority: "normal", RequesterKind: "agent", TaskID: "tsk_…"}
	res, can, exp, esc := hum, hum, hum, hum
	esc.Status, esc.Priority = "in_progress", "high"
	res.Status, res.Outcome = "resolved", "answered"
	can.Status, exp.Status = "cancelled", "expired"
	code := int64(0)
	reset := time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)
	ul := UsageLimit{SpaceID: "spc_…", LimitID: "ulm_…", Scope: "account", AccountID: "acc_…", Kind: "five_hour", ResetsAt: &reset, RunID: "run_…"}
	xs := XSpaceRequest{SpaceID: "spc_…", Side: "destination", RequestID: "xrq_…", SourceSpaceID: "spc_…", DestSpaceID: "spc_…", Kind: "question",
		State: "pending", Depth: 1, CorrelationID: "xrq_…"}
	xsDone := xs
	xsDone.Side, xsDone.State = "source", "completed"
	return []CatalogEntry{
		{RunLaunchedName, RunLaunched{run}, "A launch was accepted and queued for the space's node."},
		{RunStartedName, RunStarted{Run: run}, "The node reported the run running for the first time."},
		{RunEndedName, RunEnded{Run: run, State: "exited", EndReason: "exited", ExitCode: &code}, "The run exited, failed or was lost."},
		{UsageLimitReachedName, UsageLimitReached{ul}, "A run's report opened a hold on its account in its space (once per hold)."},
		{UsageLimitClearedName, UsageLimitCleared{UsageLimit: ul, ClearedBy: "run:run_…"}, "A hold on an account was cleared."},
		{TaskCreatedName, TaskCreated{Task: task, Status: "ready"}, "A task was added to a queue."},
		{TaskStatusChangedName, TaskStatusChanged{Task: task, From: "ready", To: "running"}, "A task changed state."},
		{InboxItemCreatedName, InboxItemCreated{SpaceID: "spc_…", ItemID: "inb_…", SourceKind: "event", SourceID: "sub_…", SubscriptionID: "sub_…", Urgency: "normal"},
			"An item arrived in an inbox."},
		{HumanCreatedName, HumanActionCreated{hum}, "A request for a person was created."},
		{HumanResolvedName, HumanActionResolved{res}, "A person responded."},
		{HumanCancelledName, HumanActionCancelled{can}, "The request was withdrawn."},
		{HumanExpiredName, HumanActionExpired{exp}, "Nobody responded in time."},
		{HumanRemindedName, HumanActionReminded{HumanAction: hum, ReminderCount: 1}, "People were reminded of a pending action."},
		{HumanEscalatedName, HumanActionEscalated{HumanAction: esc, FromPriority: "normal", Reassigned: true}, "A pending action escalated per policy."},
		{HumanReroutedName, HumanActionRerouted{HumanAction: res, NotificationID: "hnt_…", To: "control", RouteReason: "requester_ended"},
			"A response was routed away from the requester's run, which had ended."},
		{ApprovalDecidedName, ApprovalDecided{SpaceID: "spc_…", ApprovalID: "apv_…", HumanActionID: "hum_…", Operation: "mcp.github.create_issue", State: "approved",
			RequesterRun: "run_…", DeciderType: "user"}, "An enforced approval was decided."},
		{ApprovalExecutedName, ApprovalExecuted{SpaceID: "spc_…", ApprovalID: "apv_…", ExecutionID: "apx_…", Operation: "mcp.github.create_issue", Result: "succeeded"},
			"An approved operation ran."},
		{MemorySyncedName, MemorySynced{SpaceID: "spc_…", CollectionID: "mem_…", Role: "dev", NodeID: "nod_…", FromCommit: "3f2a…", ToCommit: "9b1c…"},
			"Central preserved a new head of a role's memory."},
		{KBProposalAcceptedName, KBProposalAccepted{SpaceID: "spc_…", KBID: "kbr_…", ProposalID: "kbp_…", SessionID: "kbs_…", Commit: "5d4e…", MergedCommit: "7a8b…"},
			"A knowledge-base proposal was merged."},
		{XSpaceCreatedName, XSpaceRequestCreated{xs}, "A space's control asked another space (published for both spaces)."},
		{XSpaceStateName, XSpaceRequestStateChanged{XSpaceRequest: xsDone, From: "accepted"},
			"A cross-space request was accepted, declined, answered, failed, cancelled or expired (published for both spaces)."},
	}
}
