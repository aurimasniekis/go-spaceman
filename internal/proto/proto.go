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
	MaxMinor = 0
	// ConnectPath is the WebSocket endpoint on central.
	ConnectPath = "/agent/v1/connect"
	// MaxMessage bounds one envelope (recording chunks travel in their own bounded messages).
	MaxMessage = 4 << 20
)

// Message types.
const (
	// node → central
	TypeHello     = "hello"
	TypeHeartbeat = "heartbeat"
	TypeAck       = "ack"
	TypeRunReport = "run.report"
	TypeRecording = "recording.chunk"

	// central → node
	TypeWelcome     = "welcome"
	TypeLeaseGrant  = "lease.grant"
	TypeLeaseRevoke = "lease.revoke"
	TypeCommand     = "command"
	TypeReconcile   = "reconcile" // central's desired state for every run assigned to the node
	TypeError       = "error"
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
)

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
	// AccountRef names the account whose secrets are attached (non-secret; recorded in the run manifest).
	AccountRef string `json:"account_ref,omitempty"`
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
	case CmdPrepare:
	default:
		return fmt.Errorf("unknown command kind %q", c.Kind)
	}
	return nil
}

// Negotiate picks the minor version both sides support, or fails.
func Negotiate(nodeMin, nodeMax int) (int, error) {
	lo, hi := max(nodeMin, MinMinor), min(nodeMax, MaxMinor)
	if lo > hi {
		return 0, fmt.Errorf("incompatible protocol: node speaks %d.%d-%d.%d, central %d.%d-%d.%d", Major, nodeMin, Major, nodeMax, Major, MinMinor, Major, MaxMinor)
	}
	return hi, nil
}
