// Package proto defines the versioned node protocol `spaceman.node/v1` between central and spaceman-agent
// (SM-NODE-001…009, ADR-0006).
//
// Transport: the node dials wss://<central>/agent/v1/connect with `Authorization: Bearer <node credential>` and
// the subprotocol `spaceman.node.v1`. Every WebSocket text message is one Envelope. Central is the only authority:
// the node executes commands and reports what it observes; it never decides placement or starts work on its own.
//
// Execution authority: each space has a lease (space, node, generation, expiry). Every command and every run report
// carries the generation; anything with a stale generation is fenced (rejected and counted). Leases are renewed in
// heartbeats; a node that loses central keeps running within the lease plus the disconnect grace, then stops runs
// (and a lease-guard terminal enforces the same bound even if the daemon dies, ADR-0017).
package proto

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Version identifiers.
const (
	Subprotocol = "spaceman.node.v1"
	// Major is the protocol major version; MinMinor..MaxMinor is the range this build accepts.
	Major    = 1
	MinMinor = 0
	MaxMinor = 2 // 1: agent tool calls (tool.call / tool.result); 2: role memory sync (memory.*), KB acceptance (kb.*)
	// ConnectPath is the WebSocket endpoint on central.
	ConnectPath = "/agent/v1/connect"
	// MaxMessage bounds one envelope (recording chunks travel in their own bounded messages).
	MaxMessage = 4 << 20
)

// Message types.
const (
	// node → central
	TypeHello        = "hello"
	TypeHeartbeat    = "heartbeat"
	TypeAck          = "ack"
	TypeRunReport    = "run.report"
	TypeRecording    = "recording.chunk"
	TypeToolCall     = "tool.call"     // minor ≥ 1: an agent's SpaceMan tool call, forwarded by the MCP bridge
	TypeMemorySync   = "memory.sync"   // minor ≥ 2: new role-memory history, as a git bundle
	TypeMemoryStatus = "memory.status" // minor ≥ 2: the node's view of its memory collections
	TypeKBResult     = "kb.result"     // minor ≥ 2: the outcome of a kb.accept command

	// central → node
	TypeWelcome      = "welcome"
	TypeLeaseGrant   = "lease.grant"
	TypeLeaseRevoke  = "lease.revoke"
	TypeCommand      = "command"
	TypeReconcile    = "reconcile" // central's desired state for every run assigned to the node
	TypeRecordingAck = "recording.ack"
	TypeToolResult   = "tool.result" // minor ≥ 1
	TypeMemoryAck    = "memory.ack"  // minor ≥ 2
	TypeError        = "error"
)

// Envelope is one protocol message.
type Envelope struct {
	V    int             `json:"v"`
	ID   string          `json:"id"`
	Type string          `json:"type"`
	TS   time.Time       `json:"ts"`
	Body json.RawMessage `json:"body,omitempty"`
}

// Encode builds an envelope around body.
func Encode(id, typ string, now time.Time, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{V: Major, ID: id, Type: typ, TS: now.UTC(), Body: b})
}

// Decode parses an envelope and checks the major version.
func Decode(data []byte) (Envelope, error) {
	if len(data) > MaxMessage {
		return Envelope{}, errors.New("proto: message too large")
	}
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return Envelope{}, fmt.Errorf("proto: %w", err)
	}
	if e.V != Major {
		return Envelope{}, fmt.Errorf("proto: unsupported major version %d", e.V)
	}
	if e.Type == "" || e.ID == "" {
		return Envelope{}, errors.New("proto: message without type or id")
	}
	return e, nil
}

// Body decodes an envelope body into v.
func (e Envelope) Decode(v any) error {
	if len(e.Body) == 0 {
		return errors.New("proto: empty body")
	}
	return json.Unmarshal(e.Body, v)
}

// Capabilities is the node's inventory, reported in hello (SM-NODE-002).
type Capabilities struct {
	OS        string          `json:"os"`
	Arch      string          `json:"arch"`
	Runner    RunnerInfo      `json:"runner"`
	Harnesses []HarnessInfo   `json:"harnesses"`
	Git       *ToolInfo       `json:"git,omitempty"`
	Features  []string        `json:"features"`
	Extra     json.RawMessage `json:"extra,omitempty"`
}

// RunnerInfo describes the node's runner.
type RunnerInfo struct {
	Kind    string `json:"kind"` // herdr-core
	Version string `json:"version"`
	CoreID  string `json:"core_id"`
	Error   string `json:"error,omitempty"`
}

// HarnessInfo describes one installed harness and its advertised capabilities (SM-HARN-003).
type HarnessInfo struct {
	Kind         string   `json:"kind"` // claude-code, codex
	Version      string   `json:"version,omitempty"`
	Path         string   `json:"path,omitempty"`
	Available    bool     `json:"available"`
	Error        string   `json:"error,omitempty"`
	Capabilities []string `json:"capabilities"`
}

// ToolInfo describes a tool binary.
type ToolInfo struct {
	Version string `json:"version"`
	Path    string `json:"path"`
}

// Hello is the node's first message.
type Hello struct {
	AgentVersion string       `json:"agent_version"`
	MinMinor     int          `json:"min_minor"`
	MaxMinor     int          `json:"max_minor"`
	Capabilities Capabilities `json:"capabilities"`
	Capacity     int          `json:"capacity"`
	// Runs is the node's journal view of every run it knows (observed state), for reconciliation.
	Runs []RunReport `json:"runs"`
	// Leases are the leases the node believes it holds.
	Leases []LeaseState `json:"leases"`
}

// Welcome is central's reply to hello.
type Welcome struct {
	NodeID            string        `json:"node_id"`
	Minor             int           `json:"minor"`
	ServerTime        time.Time     `json:"server_time"`
	HeartbeatInterval time.Duration `json:"heartbeat_interval_ns"`
	LeaseTTL          time.Duration `json:"lease_ttl_ns"`
	DisconnectGrace   time.Duration `json:"disconnect_grace_ns"`
}

// LeaseState is a lease as one side knows it.
type LeaseState struct {
	SpaceID    string    `json:"space_id"`
	Generation int64     `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// LeaseGrant grants or renews a lease. A higher generation replaces the old one; the node must stop runs of the
// old generation (they are fenced).
type LeaseGrant struct {
	LeaseState
	// Workdir is the space's managed folder name on the node (under the agent's spaces directory).
	Workdir string `json:"workdir"`
	// DisconnectPolicy is how the node stops the space's runs once the lease plus the disconnect grace has passed
	// without central: interrupt (Esc/Ctrl+C, then kill) or kill.
	DisconnectPolicy string `json:"disconnect_policy"`
	// Grace is the disconnect grace the node adds to ExpiresAt.
	Grace time.Duration `json:"grace_ns"`
}

// LeaseRevoke ends a lease: the node stops the space's runs per Mode (interrupt or kill).
type LeaseRevoke struct {
	SpaceID    string `json:"space_id"`
	Generation int64  `json:"generation"`
	Reason     string `json:"reason"`
	Mode       string `json:"mode"` // interrupt, kill
}

// Heartbeat renews leases and reports load.
type Heartbeat struct {
	Leases  []LeaseState `json:"leases"`
	Running int          `json:"running"`
	// CentralLost is how long the node was disconnected before this connection (0 on a fresh start).
	Load map[string]float64 `json:"load,omitempty"`
}

// Command kinds.
const (
	CmdLaunch    = "run.launch"
	CmdStop      = "run.stop"
	CmdInterrupt = "run.interrupt"
	CmdInput     = "run.input"
	CmdPrepare   = "space.prepare"
	CmdKBAccept  = "kb.accept"    // minor ≥ 2: merge an accepted KB proposal into the KB's base branch
	CmdDreamDone = "dream.commit" // minor ≥ 2: checkpoint a dream's memory, then advance its evidence positions
)

// Skill is one skill version's files (guidance only; never executed by SpaceMan).
type Skill struct {
	Name    string            `json:"name"`
	Version int64             `json:"version"`
	SHA256  string            `json:"sha256"`
	Files   map[string]string `json:"files"`
}

// Worktree is a git worktree for one writer: Repo is a repository folder relative to the space folder, Branch the
// branch to check out there (created from Base, default HEAD, when it does not exist). The node never resets,
// deletes or force-updates anything (SM-REPO-006).
type Worktree struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Base   string `json:"base,omitempty"`
}

// Command is an instruction from central (SM-NODE-003). CommandID is stable; delivering the same command twice
// is acknowledged "duplicate" and has no further effect (SM-NODE-004).
type Command struct {
	CommandID      string          `json:"command_id"`
	Kind           string          `json:"kind"`
	SpaceID        string          `json:"space_id"`
	RunID          string          `json:"run_id,omitempty"`
	Generation     int64           `json:"generation"`
	IdempotencyKey string          `json:"idempotency_key"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

// Ack results.
const (
	AckAccepted  = "accepted"
	AckDuplicate = "duplicate"
	AckRejected  = "rejected"
)

// Ack acknowledges a command (receipt, not completion: completion arrives as run reports).
type Ack struct {
	CommandID string `json:"command_id"`
	Result    string `json:"result"`
	Reason    string `json:"reason,omitempty"`
}

// Run states as observed by the node.
const (
	RunPending  = "pending"
	RunStarting = "starting"
	RunRunning  = "running"
	RunStopping = "stopping"
	RunExited   = "exited"
	RunFailed   = "failed"
	RunLost     = "lost"
)

// RunReport is the node's observation of a run (SM-NODE-005). It carries the lease generation the run belongs to.
type RunReport struct {
	RunID      string    `json:"run_id"`
	SpaceID    string    `json:"space_id"`
	Generation int64     `json:"generation"`
	State      string    `json:"state"`
	TerminalID string    `json:"terminal_id,omitempty"`
	CoreID     string    `json:"core_id,omitempty"`
	Identity   string    `json:"identity,omitempty"`
	PID        int       `json:"pid,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Error      string    `json:"error,omitempty"`
	Status     string    `json:"status,omitempty"` // program-reported (OSC 7501): idle, working, done, blocked, error
	StatusKind string    `json:"status_kind,omitempty"`
	Turn       uint64    `json:"turn,omitempty"`
	SessionID  string    `json:"session_id,omitempty"` // harness conversation id when known
	Adopted    bool      `json:"adopted,omitempty"`    // an existing process was adopted instead of launching
	ObservedAt time.Time `json:"observed_at"`
}

// Reconcile is central's desired state of the node's runs, sent after hello and on demand.
type Reconcile struct {
	Runs []DesiredRun `json:"runs"`
}

// DesiredRun is what central wants for one run.
type DesiredRun struct {
	RunID      string `json:"run_id"`
	SpaceID    string `json:"space_id"`
	Generation int64  `json:"generation"`
	Desired    string `json:"desired"` // running, stopped
}

// RecordingChunk ships recording records (internal/recording.Record JSON) of one run to central. Records carry their
// node-assigned sequence numbers; central stores each sequence once, so re-shipping after a reconnect is harmless.
type RecordingChunk struct {
	RunID   string            `json:"run_id"`
	SpaceID string            `json:"space_id"`
	Records []json.RawMessage `json:"records"`
}

// RecordingAck confirms the highest stored sequence of a run's recording.
type RecordingAck struct {
	RunID string `json:"run_id"`
	Seq   uint64 `json:"seq"`
}

// Error is a fatal protocol error; the receiver closes the connection.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// LaunchPayload is the body of run.launch. It never contains secrets: the node fetches the run's secrets with the
// run credential through its own authenticated channel when it materializes the launch (ADR-0009).
type LaunchPayload struct {
	Harness string `json:"harness"` // claude-code, codex
	Role    string `json:"role"`
	Kind    string `json:"kind"` // control, worker, kb, dreamer, diagnostic
	// InstanceID selects the harness config overlay; SessionID resumes an existing conversation when supported.
	InstanceID string            `json:"instance_id"`
	SessionID  string            `json:"session_id,omitempty"`
	Resume     bool              `json:"resume,omitempty"`
	Prompt     string            `json:"prompt,omitempty"` // initial instruction (non-secret)
	SystemText string            `json:"system,omitempty"` // appended system prompt (non-secret)
	Model      string            `json:"model,omitempty"`
	Settings   map[string]string `json:"settings,omitempty"`
	Workspace  string            `json:"workspace,omitempty"` // relative to the space folder
	// Worktree asks the node to run in a git worktree of a repository in the space folder (SM-REPO-002).
	Worktree *Worktree `json:"worktree,omitempty"`
	// Skills are materialized into the harness's own skills folder when the harness supports it (SM-MCP-004).
	Skills []Skill `json:"skills,omitempty"`
	// AccountRef names the account whose secrets are attached (non-secret; recorded in the run manifest).
	AccountRef string `json:"account_ref,omitempty"`
	// Dream asks the node to prepare a dream workspace (minor ≥ 2, ADR-0027).
	Dream *DreamSpec `json:"dream,omitempty"`
	// Secrets and RunToken are injected by central only into the message on the wire at dispatch time; they are
	// never stored with the command, logged or echoed in reports. The node writes them only to the run's 0600 secret
	// file, which the launcher shim reads and unlinks before exec (ADR-0009); they never reach the runner's env.
	Secrets  map[string]string `json:"secrets,omitempty"`
	RunToken string            `json:"run_token,omitempty"`
}

// Redacted returns a copy without secrets (for storage and logs).
func (p LaunchPayload) Redacted() LaunchPayload {
	p.Secrets, p.RunToken = nil, ""
	return p
}

// StopPayload is the body of run.stop.
type StopPayload struct {
	Mode   string `json:"mode"` // interrupt (polite, then kill after Grace), kill
	Grace  int    `json:"grace_seconds,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// InputPayload is the body of run.input (safe input: delivered only when the program reports idle unless Force).
type InputPayload struct {
	Text  string `json:"text"`
	Force bool   `json:"force,omitempty"`
}

var (
	reSpace = regexp.MustCompile(`^spc_[0-9a-z]{26}$`)
	reRun   = regexp.MustCompile(`^run_[0-9a-z]{26}$`)
	reCmd   = regexp.MustCompile(`^cmd_[0-9a-z]{26}$`)
)

// Validate checks a command's identifiers before any side effect.
func (c Command) Validate() error {
	switch {
	case !reCmd.MatchString(c.CommandID):
		return errors.New("invalid command_id")
	case !reSpace.MatchString(c.SpaceID):
		return errors.New("invalid space_id")
	case c.Generation <= 0:
		return errors.New("invalid generation")
	case c.IdempotencyKey == "" || len(c.IdempotencyKey) > 128:
		return errors.New("invalid idempotency_key")
	}
	switch c.Kind {
	case CmdLaunch, CmdStop, CmdInterrupt, CmdInput:
		if !reRun.MatchString(c.RunID) {
			return errors.New("invalid run_id")
		}
	case CmdPrepare, CmdKBAccept:
	case CmdDreamDone:
		if !reRun.MatchString(c.RunID) {
			return errors.New("invalid run_id")
		}
	default:
		return fmt.Errorf("unknown command kind %q", c.Kind)
	}
	return nil
}

// MinorTools is the first minor version that carries agent tool calls.
const MinorTools = 1

// ToolCall is one agent tool call (ADR-0012). Central authenticates RunToken itself; the node only forwards calls whose
// token it issued to that run. ToolCatalog asks for the tools the run may use.
type ToolCall struct {
	CallID   string          `json:"call_id"`
	RunID    string          `json:"run_id"`
	RunToken string          `json:"run_token"`
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args,omitempty"`
}

// ToolCatalog is the reserved tool name that lists a run's tools.
const ToolCatalog = "_catalog"

// ToolResult answers a ToolCall. Code is an apperr code when OK is false.
type ToolResult struct {
	CallID string          `json:"call_id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
}

// AgentTool describes one tool in a run's catalog.
type AgentTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	ReadOnly    bool            `json:"read_only,omitempty"`
	// Upstream marks a tool served by an external MCP server through the gateway (ADR-0023): its result is an MCP
	// CallToolResult that the bridge passes through.
	Upstream bool `json:"upstream,omitempty"`
}

// Negotiate picks the minor version both sides support, or fails.
func Negotiate(nodeMin, nodeMax int) (int, error) {
	lo, hi := max(nodeMin, MinMinor), min(nodeMax, MaxMinor)
	if lo > hi {
		return 0, fmt.Errorf("incompatible protocol: node speaks %d.%d-%d.%d, central %d.%d-%d.%d", Major, nodeMin, Major, nodeMax, Major, MinMinor, Major, MaxMinor)
	}
	return hi, nil
}

// MinorMemory is the first minor version that carries role-memory sync.
const MinorMemory = 2

// MemoryMaxBundle bounds one memory bundle (base64 inside a MaxMessage envelope).
const MemoryMaxBundle = 2 << 20

// MemorySync offers central new history of one role-memory collection (ADR-0025). Parent is the head central last
// acknowledged ("" for none); the bundle holds the commits from Parent to Head.
type MemorySync struct {
	SyncID  string `json:"sync_id"`
	SpaceID string `json:"space_id"`
	Role    string `json:"role"`
	Parent  string `json:"parent,omitempty"`
	Head    string `json:"head"`
	Bundle  []byte `json:"bundle"`
}

// Memory sync results.
const (
	MemoryOK          = "ok"           // central's head is now Head
	MemoryConflict    = "conflict"     // Head does not descend from central's head; kept under refs/conflicts
	MemoryMissingBase = "missing_base" // central lacks Parent: send the full history
	MemoryError       = "error"
)

// MemoryAck answers a MemorySync. Head is central's head after the sync.
type MemoryAck struct {
	SyncID  string `json:"sync_id"`
	SpaceID string `json:"space_id"`
	Role    string `json:"role"`
	Result  string `json:"result"`
	Head    string `json:"head,omitempty"`
	Message string `json:"message,omitempty"`
}

// MemoryStatus is the node's view of its collections (SM-MEM-004).
type MemoryStatus struct {
	Collections []MemoryCollectionStatus `json:"collections"`
}

// MemoryCollectionStatus is one collection as the node sees it.
type MemoryCollectionStatus struct {
	SpaceID        string     `json:"space_id"`
	Role           string     `json:"role"`
	Head           string     `json:"head,omitempty" doc:"The node's shared history"`
	Acked          string     `json:"acked,omitempty" doc:"The head central last acknowledged"`
	Instances      int        `json:"instances"`
	Uncheckpointed int        `json:"uncheckpointed" doc:"Changed files not yet checkpointed, across instances"`
	LocalCommits   int        `json:"local_commits" doc:"Commits not yet acknowledged by central"`
	Pending        bool       `json:"pending" doc:"A sync is due or in flight"`
	LastAckAt      *time.Time `json:"last_ack_at,omitempty"`
	LastCheckpoint *time.Time `json:"last_checkpoint_at,omitempty"`
	Conflicts      int        `json:"conflicts" doc:"Conflicts preserved on the node and not yet reconciled"`
	Retries        int        `json:"retries"`
	LastError      string     `json:"last_error,omitempty"`
}

// KBAcceptPayload asks the node to merge a KB proposal (ADR-0026): Commit must be on Branch (the session's branch); it
// is merged into Target by fast-forward or a clean merge, the validator runs on the result, and Target moves only if it
// still points where it did (compare-and-swap). Nothing is force-updated; a conflict leaves everything as it was.
type KBAcceptPayload struct {
	ProposalID string   `json:"proposal_id"`
	Repo       string   `json:"repo"`
	Branch     string   `json:"branch"`
	Commit     string   `json:"commit"`
	Target     string   `json:"target"`
	Validator  []string `json:"validator,omitempty"`
	Timeout    int      `json:"timeout_seconds,omitempty"`
	Message    string   `json:"message"`
}

// KB acceptance results.
const (
	KBMerged   = "merged"
	KBConflict = "conflict"
	KBFailed   = "failed"
)

// KBResult reports a kb.accept outcome.
type KBResult struct {
	CommandID  string `json:"command_id"`
	ProposalID string `json:"proposal_id"`
	Result     string `json:"result"`
	Merged     string `json:"merged,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// DreamSpec describes a dream's workspace (ADR-0027): the role memories it consolidates, whose transcripts it may read
// and the evidence budget. Transcripts are read from the space's instances on the node; only what is new since the
// dream configuration's last committed position is offered.
type DreamSpec struct {
	ConfigID        string   `json:"config_id"`
	MemoryRoles     []string `json:"memory_roles"`
	TranscriptRoles []string `json:"transcript_roles"` // "*" for every role
	ExcludeRoles    []string `json:"exclude_roles,omitempty"`
	MaxFiles        int      `json:"max_files"`
	MaxBytes        int64    `json:"max_bytes"`
}
