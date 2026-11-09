package metrics

import (
	"regexp"
	"sync"
)

// Other is the fallback label value for anything outside a label's allowlist or over its cardinality cap.
const Other = "other"

// Typed label values. Methods accept these types so call sites use the constants below; every value is still
// normalized against the allowlist at runtime, so a converted free-form string can never create a new series.

// TxResult classifies the final outcome of a store write transaction.
type TxResult string

// Store transaction results (internal/store error classes).
const (
	TxOK         TxResult = "ok"
	TxBusy       TxResult = "busy"
	TxFull       TxResult = "full"
	TxIO         TxResult = "io"
	TxCorrupt    TxResult = "corrupt"
	TxConstraint TxResult = "constraint"
	TxConflict   TxResult = "conflict"
	TxNotFound   TxResult = "not_found"
	TxCanceled   TxResult = "canceled"
	TxError      TxResult = "error"
)

// EventOutcome is the outcome of publishing one event to Pulse (internal/outbox).
type EventOutcome string

// Event publish outcomes.
const (
	EventPublished EventOutcome = "published"
	EventRetry     EventOutcome = "retry"
	EventDead      EventOutcome = "dead"
)

// LoginMethod is an operator login method.
type LoginMethod string

// Login methods.
const (
	LoginPassword LoginMethod = "password"
	LoginOIDC     LoginMethod = "oidc"
	LoginRecovery LoginMethod = "recovery"
	LoginToken    LoginMethod = "token"
)

// LoginOutcome is the outcome of an operator or OIDC login.
type LoginOutcome string

// Login outcomes.
const (
	LoginSuccess   LoginOutcome = "success"
	LoginFailure   LoginOutcome = "failure"   // bad credentials / MFA failure
	LoginDenied    LoginOutcome = "denied"    // authenticated by the IdP but refused by policy (not provisioned, disabled)
	LoginThrottled LoginOutcome = "throttled" // rate limited
	LoginError     LoginOutcome = "error"     // IdP or internal error
)

// NodeConnectResult is the outcome of a node agent connection attempt.
type NodeConnectResult string

// Node connection results.
const (
	NodeAccepted      NodeConnectResult = "accepted"
	NodeAuthFailed    NodeConnectResult = "auth_failed"
	NodeRevoked       NodeConnectResult = "revoked"
	NodeIncompatible  NodeConnectResult = "incompatible" // protocol version outside the supported range
	NodeDuplicate     NodeConnectResult = "duplicate"    // replaced an older connection of the same node
	NodeShuttingDown  NodeConnectResult = "shutting_down"
	NodeProtocolError NodeConnectResult = "protocol_error"
)

// CommandType is a node protocol command type.
type CommandType string

// Command types (internal/proto).
const (
	CmdLaunch    CommandType = "run.launch"
	CmdStop      CommandType = "run.stop"
	CmdInterrupt CommandType = "run.interrupt"
	CmdInput     CommandType = "run.input"
	CmdSpace     CommandType = "space.prepare"
	CmdOther     CommandType = "other"
)

// CommandResult is a node's acknowledgement of a command.
type CommandResult string

// Command acknowledgement results.
const (
	CmdAccepted  CommandResult = "accepted"
	CmdDuplicate CommandResult = "duplicate"
	CmdRejected  CommandResult = "rejected"
	CmdTimeout   CommandResult = "timeout"
)

// Harness is a harness kind.
type Harness string

// Harness kinds.
const (
	HarnessClaude Harness = "claude-code"
	HarnessCodex  Harness = "codex"
)

// RunStartResult is the outcome of a launch.
type RunStartResult string

// Launch results.
const (
	RunLaunched RunStartResult = "launched"
	RunAdopted  RunStartResult = "adopted" // an existing process was found and adopted instead of launching
	RunFailed   RunStartResult = "failed"
)

// RunEndReason is why a run ended.
type RunEndReason string

// Run end reasons.
const (
	RunExited       RunEndReason = "exited"
	RunStopped      RunEndReason = "stopped"
	RunKilled       RunEndReason = "killed"
	RunLeaseExpired RunEndReason = "lease_expired"
	RunLost         RunEndReason = "lost"
	RunLaunchFailed RunEndReason = "launch_failed" // the launch could not be dispatched (node too old, account unusable)
)

// StaleReason is why central's stale-run sweeper ended a run (ADR-0074); the values are the runs' end reasons.
type StaleReason string

// Stale-run reasons.
const (
	StaleNotOnNode StaleReason = "stale_not_on_node" // the connected node's hello did not report the run
	StaleLeaseLost StaleReason = "stale_lease_lost"  // the disconnected node's lease and grace passed
)

// Volume names a filesystem whose capacity is exported.
type Volume string

// Volumes.
const (
	VolumeData      Volume = "data"
	VolumeArtifacts Volume = "artifacts"
	VolumeBackups   Volume = "backups"
)

// enum normalizes a label value against a fixed allowlist.
type enum map[string]struct{}

func newEnum[T ~string](vals ...T) enum {
	e := make(enum, len(vals))
	for _, v := range vals {
		e[string(v)] = struct{}{}
	}
	return e
}

func (e enum) norm(v string) string {
	if _, ok := e[v]; ok {
		return v
	}
	return Other
}

var (
	enumTxResult      = newEnum(TxOK, TxBusy, TxFull, TxIO, TxCorrupt, TxConstraint, TxConflict, TxNotFound, TxCanceled, TxError)
	enumEvent         = newEnum(EventPublished, EventRetry, EventDead)
	enumLoginMeth     = newEnum(LoginPassword, LoginOIDC, LoginRecovery, LoginToken)
	enumLoginOut      = newEnum(LoginSuccess, LoginFailure, LoginDenied, LoginThrottled, LoginError)
	enumVolume        = newEnum(VolumeData, VolumeArtifacts, VolumeBackups)
	enumJobResult     = newEnum("ok", "error")
	enumNodeConnect   = newEnum(NodeAccepted, NodeAuthFailed, NodeRevoked, NodeIncompatible, NodeDuplicate, NodeShuttingDown, NodeProtocolError)
	enumCommandType   = newEnum(CmdLaunch, CmdStop, CmdInterrupt, CmdInput, CmdSpace, CmdOther)
	enumCommandResult = newEnum(CmdAccepted, CmdDuplicate, CmdRejected, CmdTimeout)
	enumHarness       = newEnum(HarnessClaude, HarnessCodex)
	enumRunStart      = newEnum(RunLaunched, RunAdopted, RunFailed)
	enumRunEnd        = newEnum(RunExited, RunStopped, RunKilled, RunLeaseExpired, RunLost, RunLaunchFailed)
	enumStaleReason   = newEnum(StaleNotOnNode, StaleLeaseLost)
)

// bounded admits label values that match a pattern, up to a fixed number of distinct values; the rest map to
// Other. It guards labels whose values come from code or operator configuration (transaction names, job names,
// OIDC provider slugs, HTTP route templates) against unbounded series growth.
type bounded struct {
	mu     sync.Mutex
	re     *regexp.Regexp
	max    int
	maxLen int
	seen   map[string]struct{}
	empty  string
}

func newBounded(re *regexp.Regexp, maxValues, maxLen int, empty string) *bounded {
	return &bounded{re: re, max: maxValues, maxLen: maxLen, seen: map[string]struct{}{}, empty: empty}
}

func (b *bounded) norm(v string) string {
	if v == "" {
		return b.empty
	}
	if len(v) > b.maxLen || !b.re.MatchString(v) {
		return Other
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.seen[v]; ok {
		return v
	}
	if len(b.seen) >= b.max {
		return Other
	}
	b.seen[v] = struct{}{}
	return v
}

// Cardinality caps for code- or configuration-derived labels.
const (
	MaxTxNames      = 160
	MaxJobNames     = 64
	MaxOIDCProvider = 32
	MaxRoutes       = 128
)

var (
	reDotted = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$`)
	reSlug   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)
	reRoute  = regexp.MustCompile(`^/[A-Za-z0-9/_.{}$-]{0,127}$`)
)
