// Package approvals enforces approvals for operations SpaceMan itself controls (SM-APPR-001…005, ADR-0024): gateway
// tools ("mcp.<server>.<tool>") and built-in agent tools ("spaceman.<tool>"). A policy says which operations need a
// person's approval. A gated call without a usable approval creates an approval request (a human action of type
// approval) and returns at once; nothing waits. Once approved, the same run calling the same operation with the same
// arguments consumes one use and runs. The approval binds the operation, its resource, the space, the requesting run
// and a digest of the canonical arguments, and has an expiry and a number of uses. Authorization is checked when a
// person decides (the API needs approval:decide) and again before every execution. The decision and each execution
// are recorded separately.
//
// Approvals of other kinds stay advisory: a human action of type approval that no policy created gates nothing.
package approvals

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Approval states (the decision) and execution states, kept apart (SM-APPR-004).
const (
	Pending  = "pending"
	Approved = "approved"
	Denied   = "denied"
	Expired  = "expired"
	Revoked  = "revoked"

	Executing   = "executing"
	Succeeded   = "succeeded"
	Failed      = "failed"
	Interrupted = "interrupted"
)

// Never gated: the tools an agent needs to ask for approval, read answers and talk to people. Gating them could leave a
// run with no way to ask.
var exempt = []string{"spaceman.whoami", "spaceman.ping", "spaceman.human_ask", "spaceman.human_types", "spaceman.human_cancel",
	"spaceman.human_get", "spaceman.human_list", "spaceman.messages", "spaceman.message_send", "spaceman.message_ack", "spaceman.message_reply",
	"spaceman.message_resolve"}

var (
	// SpaceMan MCP tools (ADR-0063) are mcp.<slug>.<class>.<tool>, so a policy can name a class: mcp.<slug>.write.*.
	reOperation = regexp.MustCompile(`^(spaceman\.[a-z0-9_]{1,64}|mcp\.[a-z][a-z0-9-]{0,30}\.[A-Za-z0-9_-]{1,64}|mcp\.[a-z][a-z0-9-]{0,30}\.\*|` +
		`mcp\.[a-z][a-z0-9-]{0,30}\.(read|write|dangerous|sensitive-read)\.([A-Za-z0-9_-]{1,64}|\*)|mcp\.\*|spaceman\.\*)$`)
	// Argument fields whose values never appear in previews.
	reSecretKey = regexp.MustCompile(`(?i)(secret|token|password|passwd|authorization|api[_-]?key|credential|private[_-]?key|cookie)`)
)

// Policy makes operations need approval.
type Policy struct {
	ID                string    `json:"id"`
	SpaceID           string    `json:"space_id" doc:"Empty: every space"`
	Operation         string    `json:"operation" doc:"spaceman.<tool>, mcp.<server>.<tool>, or a prefix ending in .*"`
	Uses              int64     `json:"uses" doc:"How many executions one approval allows"`
	ValidSeconds      int64     `json:"valid_seconds" doc:"How long an approval can be used after the decision"`
	RequestTTLSeconds int64     `json:"request_ttl_seconds" doc:"How long a request waits for a decision"`
	Note              string    `json:"note,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

// Approval is one request and its decision.
type Approval struct {
	ID            string      `json:"id"`
	SpaceID       string      `json:"space_id"`
	PolicyID      string      `json:"policy_id"`
	Operation     string      `json:"operation"`
	Resource      string      `json:"resource"`
	ArgsDigest    string      `json:"args_digest" doc:"SHA-256 of the operation, resource and canonical arguments"`
	ArgsPreview   string      `json:"args_preview" doc:"The arguments with secret-looking fields redacted"`
	RequesterRun  string      `json:"requester_run"`
	RequesterRole string      `json:"requester_role"`
	HumanActionID string      `json:"human_action_id"`
	State         string      `json:"state" enum:"pending,approved,denied,expired,revoked"`
	StateReason   string      `json:"state_reason,omitempty"`
	UsesAllowed   int64       `json:"uses_allowed"`
	UsesLeft      int64       `json:"uses_left"`
	DecidedBy     string      `json:"decided_by,omitempty"`
	DecidedAt     *time.Time  `json:"decided_at,omitempty"`
	ValidUntil    *time.Time  `json:"valid_until,omitempty"`
	RequestExpiry time.Time   `json:"request_expires_at"`
	CreatedAt     time.Time   `json:"created_at"`
	Executions    []Execution `json:"executions"`
}

// Execution is one use of an approval.
type Execution struct {
	ID         string     `json:"id"`
	State      string     `json:"state" enum:"executing,succeeded,failed,interrupted"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Service manages policies and approvals.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	human   *human.Service
	log     *slog.Logger
	// StillAuthorized reports whether the person who approved may still decide approvals in the space; checked before
	// every execution (SM-APPR-003). Nil allows.
	StillAuthorized func(ctx context.Context, by audit.Actor, spaceID string) bool
	// Outbox records decision and execution events for publication to Pulse (SM-OBS-004). Nil: nothing is published.
	Outbox *outbox.Outbox
}

// decided records the decision event of an approval (never its arguments, preview or resource); the approval is the
// correlation and the human action that decided it the causation.
func (svc *Service) decided(ctx context.Context, tx *store.Tx, a db.Approval, state string, by audit.Actor) error {
	_, err := svc.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: outbox.ApprovalDecided{SpaceID: a.SpaceID, ApprovalID: a.ID, HumanActionID: a.HumanActionID,
		Operation: outbox.CleanID(a.Operation), State: state, RequesterRun: a.RequesterRun, DeciderType: by.Type}, Correlation: a.ID, Causation: a.HumanActionID})
	return err
}

// New returns the service and registers it to hear about decisions on its human actions.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, h *human.Service, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	svc := &Service{s: s, audit: aw, changes: ch, human: h, log: log}
	h.OnFinalized = svc.onFinalized
	return svc
}

// ---- Policies ----

// PolicyInput creates a policy.
type PolicyInput struct {
	SpaceID           string `json:"space_id,omitempty" maxLength:"64" doc:"Empty: every space"`
	Operation         string `json:"operation" maxLength:"120"`
	Uses              int64  `json:"uses,omitempty" doc:"1-100; default 1"`
	ValidSeconds      int64  `json:"valid_seconds,omitempty" doc:"60-604800; default 3600"`
	RequestTTLSeconds int64  `json:"request_ttl_seconds,omitempty" doc:"300-2592000; default 86400"`
	Note              string `json:"note,omitempty" maxLength:"2000"`
}

func policyOut(p db.ApprovalPolicy) *Policy {
	return &Policy{ID: p.ID, SpaceID: p.SpaceID, Operation: p.Operation, Uses: p.Uses, ValidSeconds: p.ValidSeconds, RequestTTLSeconds: p.RequestTtlSeconds,
		Note: p.Note, CreatedAt: time.UnixMicro(p.CreatedAt).UTC()}
}

// CreatePolicy adds a policy.
func (svc *Service) CreatePolicy(ctx context.Context, actor audit.Actor, in PolicyInput) (*Policy, error) {
	v := &apperr.Validation{}
	in.Operation = strings.TrimSpace(in.Operation)
	if !reOperation.MatchString(in.Operation) {
		v.Add("operation", "must be spaceman.<tool>, mcp.<server>.<tool>, mcp.<server>.*, mcp.* or spaceman.*")
	}
	if isExempt(in.Operation) {
		v.Add("operation", "%s is never gated: agents need it to ask for approval and talk to people", in.Operation)
	}
	if in.Uses == 0 {
		in.Uses = 1
	}
	if in.ValidSeconds == 0 {
		in.ValidSeconds = 3600
	}
	if in.RequestTTLSeconds == 0 {
		in.RequestTTLSeconds = 86400
	}
	if in.Uses < 1 || in.Uses > 100 {
		v.Add("uses", "must be between 1 and 100")
	}
	if in.ValidSeconds < 60 || in.ValidSeconds > 7*86400 {
		v.Add("valid_seconds", "must be between 60 and 604800")
	}
	if in.RequestTTLSeconds < 300 || in.RequestTTLSeconds > 30*86400 {
		v.Add("request_ttl_seconds", "must be between 300 and 2592000")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	id := ids.New("apl")
	err := svc.s.W().TxNamed(ctx, "approvals.policy", func(tx *store.Tx) error {
		if in.SpaceID != "" {
			if _, err := tx.Q.GetSpace(ctx, in.SpaceID); errors.Is(err, sql.ErrNoRows) {
				return apperr.New(apperr.CodeNotFound, "space not found")
			} else if err != nil {
				return err
			}
		}
		if _, err := tx.Q.GetApprovalPolicyByKey(ctx, db.GetApprovalPolicyByKeyParams{SpaceID: in.SpaceID, Operation: in.Operation}); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "a policy for %s exists", in.Operation)
		}
		if err := tx.Q.InsertApprovalPolicy(ctx, db.InsertApprovalPolicyParams{ID: id, SpaceID: in.SpaceID, Operation: in.Operation, Uses: in.Uses,
			ValidSeconds: in.ValidSeconds, RequestTtlSeconds: in.RequestTTLSeconds, Note: in.Note, CreatedAt: tx.NowMicros(), CreatedBy: actor.ID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, in.SpaceID, "approval_policy", id, "created", 1); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "approval_policy.created", Target: audit.Target{Type: "approval_policy", ID: id},
			Details: map[string]any{"space_id": in.SpaceID, "operation": in.Operation, "uses": in.Uses, "valid_seconds": in.ValidSeconds}})
		return err
	})
	if err != nil {
		return nil, err
	}
	p, err := svc.s.R().GetApprovalPolicy(ctx, id)
	if err != nil {
		return nil, err
	}
	return policyOut(p), nil
}

// DeletePolicy removes a policy. Approvals it already produced stay usable until they expire or are revoked.
func (svc *Service) DeletePolicy(ctx context.Context, actor audit.Actor, id string) error {
	return svc.s.W().TxNamed(ctx, "approvals.policy_delete", func(tx *store.Tx) error {
		p, err := tx.Q.GetApprovalPolicy(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "policy not found")
		} else if err != nil {
			return err
		}
		if _, err := tx.Q.DeleteApprovalPolicy(ctx, id); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, p.SpaceID, "approval_policy", id, "deleted", 2); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "approval_policy.deleted", Target: audit.Target{Type: "approval_policy", ID: id},
			Details: map[string]any{"space_id": p.SpaceID, "operation": p.Operation}})
		return err
	})
}

// Policies lists every policy.
func (svc *Service) Policies(ctx context.Context) ([]*Policy, error) {
	rows, err := svc.s.R().ListApprovalPolicies(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Policy, 0, len(rows))
	for _, r := range rows {
		out = append(out, policyOut(r))
	}
	return out, nil
}

func isExempt(op string) bool {
	for _, e := range exempt {
		if op == e {
			return true
		}
	}
	return false
}

// matches reports whether a policy operation (exact, or a prefix ending in ".*") covers op.
func matches(pattern, op string) bool {
	if strings.HasSuffix(pattern, ".*") {
		return strings.HasPrefix(op, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == op
}

// policyFor returns the policy that gates op in a space: a space policy wins over a global one, an exact one over a
// prefix.
func (svc *Service) policyFor(ctx context.Context, spaceID, op string) (*db.ApprovalPolicy, error) {
	if isExempt(op) {
		return nil, nil
	}
	rows, err := svc.s.R().ListApprovalPoliciesFor(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	var best *db.ApprovalPolicy
	score := -1
	for i := range rows {
		r := rows[i]
		if !matches(r.Operation, op) {
			continue
		}
		s := 0
		if r.SpaceID != "" {
			s += 2
		}
		if r.Operation == op {
			s++
		}
		if s > score {
			best, score = &rows[i], s
		}
	}
	return best, nil
}

// ---- Binding ----

// Digest is the binding of an operation's arguments: SHA-256 over the operation, resource and canonical JSON (object
// keys sorted, numbers kept exactly). Any change to the arguments changes it.
func Digest(op, resource string, args json.RawMessage) (string, string, error) {
	canon, err := canonical(args)
	if err != nil {
		return "", "", err
	}
	h := sha256.Sum256([]byte(op + "\n" + resource + "\n" + canon))
	return hex.EncodeToString(h[:]), canon, nil
}

func canonical(args json.RawMessage) (string, error) {
	t := bytes.TrimSpace(args)
	if len(t) == 0 || string(t) == "null" {
		return "{}", nil
	}
	dec := json.NewDecoder(bytes.NewReader(t))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", apperr.New(apperr.CodeInvalid, "the arguments are not valid JSON")
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// preview renders arguments for the approver with secret-looking fields redacted.
func preview(canon string) string {
	var v any
	dec := json.NewDecoder(strings.NewReader(canon))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return ""
	}
	b, _ := json.MarshalIndent(redact(v), "", "  ")
	s := string(b)
	if len(s) > 8000 {
		s = s[:8000] + "\n… (truncated; the approval covers the full arguments)"
	}
	return s
}

func redact(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			if reSecretKey.MatchString(k) {
				out[k] = "[redacted]"
			} else {
				out[k] = redact(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = redact(val)
		}
		return out
	}
	return v
}

// ---- The gate ----

// ErrRequired is returned (wrapped in an apperr precondition) when a call needs approval first.
var ErrRequired = errors.New("approval required")

// Gate checks one call of a run. It returns finish (non-nil) when the call may run: the caller must call finish with
// the call's error once it is done. It returns (nil, nil) when no policy gates the operation, and an error when the
// call may not run now (approval requested, pending, denied or the request is invalid).
func (svc *Service) Gate(ctx context.Context, p *fleet.RunPrincipal, op, resource string, args json.RawMessage) (func(error), error) {
	pol, err := svc.policyFor(ctx, p.SpaceID, op)
	if err != nil || pol == nil {
		return nil, err
	}
	return svc.gate(ctx, runRequester(p), pol, op, resource, args)
}

// Requester is who asks for an approval: a run (Gate, Require), or a user's API token on the external MCP endpoint
// (RequireFor, ADR-0063).
type Requester struct {
	SpaceID string
	// Key binds an approval to its requester (approvals.requester_run): a run ID, or TokenRequesterPrefix + the token
	// ID. Only calls with the same key can use the approval.
	Key  string
	Role string
	// Actor records the requests and uses in the audit.
	Actor audit.Actor
	// Human is the requester the person deciding sees.
	Human human.Requester
	// SelfUserID is a user who must never decide the approval (the token's owner); "" for runs.
	SelfUserID string
}

// TokenRequesterPrefix starts the Key of an approval an API token asked for through the external MCP endpoint.
const TokenRequesterPrefix = "api_token:"

func runRequester(p *fleet.RunPrincipal) Requester {
	return Requester{SpaceID: p.SpaceID, Key: p.RunID, Role: p.Role, Actor: audit.Actor{Type: audit.ActorAgent, ID: p.RunID},
		Human: human.Requester{Kind: "agent", ID: p.RunID, Role: p.Role, Run: p.RunID, Label: p.Role + " (" + p.Kind + ")"}}
}

// RequiredPolicyID is the policy ID recorded on approvals that no stored policy asked for: the operation always needs
// one (Require).
const RequiredPolicyID = "required"

// requiredPolicy is the policy Require applies when no stored policy matches: one use, valid for an hour after the decision,
// a day to decide.
var requiredPolicy = db.ApprovalPolicy{ID: RequiredPolicyID, Operation: "", Uses: 1, ValidSeconds: 3600, RequestTtlSeconds: 86400}

// Require is Gate for an operation that always needs a person's approval, whether or not a policy names it: dangerous
// SpaceMan MCP tools, and writes of a server set to gate them (ADR-0063). A matching policy still sets the uses and
// lifetimes; without one, a synthetic one-use policy does. Exempt operations are never required. It never returns
// (nil, nil).
func (svc *Service) Require(ctx context.Context, p *fleet.RunPrincipal, op, resource string, args json.RawMessage) (func(error), error) {
	return svc.RequireFor(ctx, runRequester(p), op, resource, args)
}

// RequireFor is Require for any requester: the external MCP endpoint's dangerous calls (ADR-0063). An approval asked
// for by an API token is never decided by the token's owner (onFinalized), nor used if it was.
func (svc *Service) RequireFor(ctx context.Context, p Requester, op, resource string, args json.RawMessage) (func(error), error) {
	if p.SpaceID == "" || p.Key == "" {
		return nil, apperr.New(apperr.CodeInvalid, "%s: an approval needs a space and a requester", op)
	}
	if isExempt(op) {
		return nil, apperr.New(apperr.CodeInvalid, "%s cannot require an approval", op)
	}
	pol, err := svc.policyFor(ctx, p.SpaceID, op)
	if err != nil {
		return nil, err
	}
	if pol == nil {
		cp := requiredPolicy
		cp.Operation = op
		pol = &cp
	}
	return svc.gate(ctx, p, pol, op, resource, args)
}

func (svc *Service) gate(ctx context.Context, p Requester, pol *db.ApprovalPolicy, op, resource string, args json.RawMessage) (func(error), error) {
	digest, canon, err := Digest(op, resource, args)
	if err != nil {
		return nil, err
	}
	rows, err := svc.s.R().ListApprovalsByKey(ctx, db.ListApprovalsByKeyParams{SpaceID: p.SpaceID, Operation: op, ArgsDigest: digest, RequesterRun: p.Key})
	if err != nil {
		return nil, err
	}
	now := svc.s.Now()
	for _, a := range rows {
		if a.State != Approved || a.UsesLeft <= 0 || !a.ValidUntil.Valid || a.ValidUntil.Int64 <= now.UnixMicro() {
			continue
		}
		by := audit.Actor{Type: a.DecidedByType, ID: a.DecidedBy, Via: a.DecidedVia}
		if p.SelfUserID != "" && by.Type == audit.ActorUser && by.ID == p.SelfUserID {
			if err := svc.end(ctx, a, Revoked, "decided by the requesting token's own user"); err != nil {
				return nil, err
			}
			continue
		}
		if svc.StillAuthorized != nil && !svc.StillAuthorized(ctx, by, a.SpaceID) {
			if err := svc.end(ctx, a, Revoked, "the person who approved can no longer decide approvals in this space"); err != nil {
				return nil, err
			}
			continue
		}
		if finish, err := svc.use(ctx, p, a); err == nil {
			return finish, nil
		} else if !errors.Is(err, errUsedUp) {
			return nil, err
		}
	}
	for _, a := range rows {
		if a.State == Pending {
			if a.HumanActionID == "" {
				if err := svc.ask(ctx, p, a, pol); err != nil {
					return nil, err
				}
				a, _ = svc.s.R().GetApproval(ctx, a.ID)
			}
			return nil, required(op, a, "is waiting for a person")
		}
	}
	a, err := svc.request(ctx, p, pol, op, resource, digest, canon)
	if err != nil {
		return nil, err
	}
	return nil, required(op, a, "was sent to a person")
}

func required(op string, a db.Approval, what string) error {
	return fmt.Errorf("%w: %w", apperr.New(apperr.CodePrecondition,
		"%s needs a person's approval. Request %s (approval %s) %s. You will be notified when it is decided; then call the tool again "+
			"with exactly the same arguments. Changed arguments need a new approval.", op, a.HumanActionID, a.ID, what), ErrRequired)
}

var errUsedUp = errors.New("used up")

// use consumes one use of an approval and records the execution start, in one transaction: two calls racing for the
// last use cannot both run (SM-APPR-003).
func (svc *Service) use(ctx context.Context, p Requester, a db.Approval) (func(error), error) {
	execID := ids.New("apx")
	err := svc.s.W().TxNamed(ctx, "approvals.use", func(tx *store.Tx) error {
		n, err := tx.Q.UseApproval(ctx, db.UseApprovalParams{ID: a.ID, Now: tx.NowMicros()})
		if err != nil {
			return err
		}
		if n == 0 {
			return errUsedUp
		}
		if err := tx.Q.InsertApprovalExecution(ctx, db.InsertApprovalExecutionParams{ID: execID, ApprovalID: a.ID, StartedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, a.SpaceID, "approval", a.ID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: p.Actor, Action: "approval.used",
			Target: audit.Target{Type: "approval", ID: a.ID}, Details: map[string]any{"space_id": a.SpaceID, "operation": a.Operation, "execution": execID}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return func(callErr error) { svc.finish(context.WithoutCancel(ctx), a, execID, callErr) }, nil
}

func (svc *Service) finish(ctx context.Context, a db.Approval, execID string, callErr error) {
	state, msg := Succeeded, ""
	if callErr != nil {
		state, msg = Failed, string(apperr.CodeOf(callErr))
	}
	err := svc.s.W().TxNamed(ctx, "approvals.finish", func(tx *store.Tx) error {
		if _, err := tx.Q.FinishApprovalExecution(ctx, db.FinishApprovalExecutionParams{State: state, Error: msg,
			FinishedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: execID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, a.SpaceID, "approval", a.ID, "updated", 0); err != nil {
			return err
		}
		if _, err := svc.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: outbox.ApprovalExecuted{SpaceID: a.SpaceID, ApprovalID: a.ID, ExecutionID: execID,
			Operation: outbox.CleanID(a.Operation), Result: state, ErrorCode: outbox.CleanID(msg)}, Correlation: a.ID, Causation: a.RequesterRun}); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorSystem, ID: "approvals"}, Action: "approval.execution_" + state,
			Target: audit.Target{Type: "approval", ID: a.ID}, Details: map[string]any{"space_id": a.SpaceID, "execution": execID, "error": msg}})
		return err
	})
	if err != nil {
		svc.log.Error("record approval execution", slog.String("approval.id", a.ID), slog.Any("error", err))
	}
}

// request stores a new pending approval and asks a person.
func (svc *Service) request(ctx context.Context, p Requester, pol *db.ApprovalPolicy, op, resource, digest, canon string) (db.Approval, error) {
	id := ids.New(ids.Approval)
	err := svc.s.W().TxNamed(ctx, "approvals.request", func(tx *store.Tx) error {
		now := tx.NowMicros()
		if err := tx.Q.InsertApproval(ctx, db.InsertApprovalParams{ID: id, SpaceID: p.SpaceID, PolicyID: pol.ID, Operation: op, Resource: resource,
			ArgsDigest: digest, ArgsPreview: preview(canon), RequesterRun: p.Key, RequesterRole: p.Role, UsesAllowed: pol.Uses, UsesLeft: pol.Uses,
			ValidSeconds: pol.ValidSeconds, RequestExpiresAt: now + pol.RequestTtlSeconds*1_000_000, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, p.SpaceID, "approval", id, "created", 1); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: p.Actor, Action: "approval.requested",
			Target: audit.Target{Type: "approval", ID: id}, Details: map[string]any{"space_id": p.SpaceID, "operation": op, "digest": digest}})
		return err
	})
	if err != nil {
		return db.Approval{}, err
	}
	a, err := svc.s.R().GetApproval(ctx, id)
	if err != nil {
		return db.Approval{}, err
	}
	if err := svc.ask(ctx, p, a, pol); err != nil {
		return db.Approval{}, err
	}
	return svc.s.R().GetApproval(ctx, id)
}

// ask creates the human action for a pending approval. The dedup key makes it safe to repeat after a crash between
// storing the approval and asking.
func (svc *Service) ask(ctx context.Context, p Requester, a db.Approval, pol *db.ApprovalPolicy) error {
	exp := time.UnixMicro(a.RequestExpiresAt).UTC()
	who := fmt.Sprintf("run `%s` (%s)", a.RequesterRun, a.RequesterRole)
	if tok, ok := strings.CutPrefix(a.RequesterRun, TokenRequesterPrefix); ok {
		who = fmt.Sprintf("API token `%s` of %s. Another person must approve: the token's own user can only deny or withdraw it", tok, p.Human.Label)
	}
	body := fmt.Sprintf("SpaceMan will run this only after approval, and only as shown.\n\n- **Operation:** `%s`\n- **Resource:** `%s`\n- **Requested by:** %s\n- **Uses:** %d, within %s of the decision\n- **Argument digest:** `%s`\n\n```json\n%s\n```\n",
		a.Operation, a.Resource, who, a.UsesAllowed, time.Duration(a.ValidSeconds)*time.Second, a.ArgsDigest, a.ArgsPreview)
	req, _ := json.Marshal(human.ApprovalRequest{Action: truncate("Run "+a.Operation, 500), Details: "Enforced approval " + a.ID})
	act, _, err := svc.human.Create(ctx, p.Actor, a.SpaceID, human.CreateInput{Type: human.TypeApproval,
		Title: truncate("Approve "+a.Operation, 200), Body: body, Request: req, ExpiresAt: &exp, DedupKey: "approval:" + a.ID, Priority: "high",
		Requester: p.Human})
	if err != nil {
		return err
	}
	_ = pol
	return svc.s.W().TxNamed(ctx, "approvals.ask", func(tx *store.Tx) error {
		return tx.Q.SetApprovalAction(ctx, db.SetApprovalActionParams{HumanActionID: act.ID, UpdatedAt: tx.NowMicros(), ID: a.ID})
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// onFinalized runs inside the human action's finalizing transaction: the decision on an enforced approval's action
// becomes the approval's decision.
func (svc *Service) onFinalized(ctx context.Context, tx *store.Tx, r db.HumanAction, status, outcome string, actor audit.Actor) error {
	if r.Type != human.TypeApproval {
		return nil
	}
	if actor.MCP() {
		// Never an approver (ADR-0063): a model acting through a SpaceMan MCP server or the MCP endpoint cannot decide
		// an approval.
		return apperr.New(apperr.CodeForbidden, "an approval is decided by a person, not through the SpaceMan MCP")
	}
	a, err := tx.Q.GetApprovalByAction(ctx, r.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // an advisory approval
	} else if err != nil {
		return err
	}
	if tokID, ok := strings.CutPrefix(a.RequesterRun, TokenRequesterPrefix); ok && actor.Type == audit.ActorUser &&
		status == human.Resolved && outcome == "approved" {
		// An approval the MCP endpoint asked for is approved by someone else: not the token's own user, in any session
		// or with any token (ADR-0063 §External endpoint). That user may still deny or withdraw their own client's
		// request: it only stops the call.
		tok, err := tx.Q.GetAPIToken(ctx, tokID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err != nil || tok.UserID == actor.ID {
			return apperr.New(apperr.CodeForbidden, "this approval was asked for with your own API token: another person must approve it")
		}
	}
	now := tx.NowMicros()
	var state, reason string
	var until sql.NullInt64
	switch {
	case status == human.Resolved && outcome == "approved":
		state, until = Approved, sql.NullInt64{Int64: now + a.ValidSeconds*1_000_000, Valid: true}
	case status == human.Resolved:
		state, reason = Denied, "denied by a person"
	case status == human.Expired:
		state, reason = Expired, "nobody decided in time"
	default:
		state, reason = Revoked, "the request was withdrawn"
	}
	if _, err := tx.Q.DecideApproval(ctx, db.DecideApprovalParams{State: state, Reason: reason, By: actor.ID, ByType: actor.Type, Via: actor.Via,
		Now: sql.NullInt64{Int64: now, Valid: true}, ValidUntil: until, ID: a.ID}); err != nil {
		return err
	}
	if err := svc.changes.Record(ctx, tx, a.SpaceID, "approval", a.ID, "updated", 0); err != nil {
		return err
	}
	if err := svc.decided(ctx, tx, a, state, actor); err != nil {
		return err
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "approval." + state, Target: audit.Target{Type: "approval", ID: a.ID},
		Details: map[string]any{"space_id": a.SpaceID, "operation": a.Operation, "digest": a.ArgsDigest, "human_action_id": r.ID}})
	return err
}

func (svc *Service) end(ctx context.Context, a db.Approval, state, reason string) error {
	return svc.s.W().TxNamed(ctx, "approvals.end", func(tx *store.Tx) error {
		n, err := tx.Q.EndApproval(ctx, db.EndApprovalParams{State: state, Reason: reason, Now: tx.NowMicros(), ID: a.ID})
		if err != nil || n == 0 {
			return err
		}
		if err := svc.changes.Record(ctx, tx, a.SpaceID, "approval", a.ID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorSystem, ID: "approvals"}, Action: "approval." + state,
			Target: audit.Target{Type: "approval", ID: a.ID}, Details: map[string]any{"space_id": a.SpaceID, "reason": reason}})
		return err
	})
}

// Revoke ends an approval before (further) execution.
func (svc *Service) Revoke(ctx context.Context, actor audit.Actor, spaceID, id, reason string) (*Approval, error) {
	a, err := svc.s.R().GetApproval(ctx, id)
	if err != nil || a.SpaceID != spaceID {
		return nil, apperr.New(apperr.CodeNotFound, "approval not found")
	}
	if a.State != Approved && a.State != Pending {
		return nil, apperr.New(apperr.CodePrecondition, "the approval is %s", a.State)
	}
	if strings.TrimSpace(reason) == "" {
		reason = "revoked by " + actor.ID
	}
	err = svc.s.W().TxNamed(ctx, "approvals.revoke", func(tx *store.Tx) error {
		n, err := tx.Q.EndApproval(ctx, db.EndApprovalParams{State: Revoked, Reason: truncate(reason, 500), Now: tx.NowMicros(), ID: id})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeConflict, "the approval changed concurrently")
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "approval", id, "updated", 0); err != nil {
			return err
		}
		if err := svc.decided(ctx, tx, a, Revoked, actor); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "approval.revoked", Target: audit.Target{Type: "approval", ID: id},
			Details: map[string]any{"space_id": spaceID, "reason": reason}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, spaceID, id)
}

// ---- Reads and maintenance ----

func out(a db.Approval, ex []db.ApprovalExecution) *Approval {
	o := &Approval{ID: a.ID, SpaceID: a.SpaceID, PolicyID: a.PolicyID, Operation: a.Operation, Resource: a.Resource, ArgsDigest: a.ArgsDigest,
		ArgsPreview: a.ArgsPreview, RequesterRun: a.RequesterRun, RequesterRole: a.RequesterRole, HumanActionID: a.HumanActionID, State: a.State,
		StateReason: a.StateReason, UsesAllowed: a.UsesAllowed, UsesLeft: a.UsesLeft, DecidedBy: a.DecidedBy,
		RequestExpiry: time.UnixMicro(a.RequestExpiresAt).UTC(), CreatedAt: time.UnixMicro(a.CreatedAt).UTC(), Executions: []Execution{}}
	if a.DecidedAt.Valid {
		t := time.UnixMicro(a.DecidedAt.Int64).UTC()
		o.DecidedAt = &t
	}
	if a.ValidUntil.Valid {
		t := time.UnixMicro(a.ValidUntil.Int64).UTC()
		o.ValidUntil = &t
	}
	for _, e := range ex {
		x := Execution{ID: e.ID, State: e.State, Error: e.Error, StartedAt: time.UnixMicro(e.StartedAt).UTC()}
		if e.FinishedAt.Valid {
			t := time.UnixMicro(e.FinishedAt.Int64).UTC()
			x.FinishedAt = &t
		}
		o.Executions = append(o.Executions, x)
	}
	return o
}

// Get returns an approval with its executions.
func (svc *Service) Get(ctx context.Context, spaceID, id string) (*Approval, error) {
	a, err := svc.s.R().GetApproval(ctx, id)
	if err != nil || a.SpaceID != spaceID {
		return nil, apperr.New(apperr.CodeNotFound, "approval not found")
	}
	ex, err := svc.s.R().ListApprovalExecutions(ctx, id)
	if err != nil {
		return nil, err
	}
	return out(a, ex), nil
}

// ForAction returns the enforced approval behind a human action, or not found (the approval is advisory).
func (svc *Service) ForAction(ctx context.Context, spaceID, actionID string) (*Approval, error) {
	a, err := svc.s.R().GetApprovalByAction(ctx, actionID)
	if err != nil || a.SpaceID != spaceID {
		return nil, apperr.New(apperr.CodeNotFound, "no enforced approval for this action")
	}
	return svc.Get(ctx, spaceID, a.ID)
}

// List returns a space's most recent approvals.
func (svc *Service) List(ctx context.Context, spaceID string, limit int64) ([]*Approval, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := svc.s.R().ListApprovals(ctx, db.ListApprovalsParams{SpaceID: spaceID, Limit: limit})
	if err != nil {
		return nil, err
	}
	res := make([]*Approval, 0, len(rows))
	for _, a := range rows {
		ex, err := svc.s.R().ListApprovalExecutions(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		res = append(res, out(a, ex))
	}
	return res, nil
}

// Expire marks approved approvals whose time ran out (pending ones expire with their human action).
func (svc *Service) Expire(ctx context.Context) (int, error) {
	n := 0
	err := svc.s.W().TxNamed(ctx, "approvals.expire", func(tx *store.Tx) error {
		rows, err := tx.Q.ExpireApprovals(ctx, tx.NowMicros())
		if err != nil {
			return err
		}
		n = len(rows)
		for _, r := range rows {
			if err := svc.changes.Record(ctx, tx, r.SpaceID, "approval", r.ID, "updated", 0); err != nil {
				return err
			}
		}
		return nil
	})
	return n, err
}

// NextExpiry is when the earliest approved approval with uses left expires (zero: none), for the deadline timer.
func (svc *Service) NextExpiry(ctx context.Context) (time.Time, error) {
	t, err := svc.s.R().NextApprovalExpiry(ctx)
	if err != nil || t == 0 {
		return time.Time{}, err
	}
	return time.UnixMicro(t).UTC(), nil
}

// Recover marks executions that were running when central stopped as interrupted. Their outcome is unknown; they are
// never retried, so a restart cannot run an approved operation twice.
func (svc *Service) Recover(ctx context.Context) (int, error) {
	n := 0
	err := svc.s.W().TxNamed(ctx, "approvals.recover", func(tx *store.Tx) error {
		rows, err := tx.Q.InterruptApprovalExecutions(ctx, sql.NullInt64{Int64: tx.NowMicros(), Valid: true})
		n = len(rows)
		return err
	})
	return n, err
}
