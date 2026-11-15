// Package audit writes the append-only, hash-chained audit log (SM-OBS-003).
//
// Derived from github.com/aurimasniekis/go-hallpass internal/audit (Apache-2.0, same author).
//
// Every security-relevant action appends an event inside the same write transaction as the change it describes,
// so the audit trail and the state can never disagree. Each row stores hash = SHA-256(prev_hash || canonical row),
// which makes deletion, reordering or modification inside the database detectable by Verify. A party able to rewrite
// the whole chain (with database write access) is out of scope here; see docs/THREAT_MODEL.md.
package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"
	"strings"
	"sync"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Actor types.
const (
	ActorUser   = "user"   // a signed-in operator (browser session)
	ActorToken  = "token"  // an API token acting on behalf of its owner
	ActorRoot   = "root"   // the bootstrap root token
	ActorNode   = "node"   // a node agent
	ActorAgent  = "agent"  // a run credential (an agent inside a harness)
	ActorSystem = "system" // automatic actions (expiry, reconciliation, jobs)
	ActorPolicy = "policy" // policy-driven automatic decisions
	ActorCLI    = "cli"    // offline CLI commands on the central host
)

// Outcomes.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeDenied  = "denied"
)

// Actor identifies who performed an action.
type Actor struct {
	Type string
	ID   string
	// Via names how a user acted when it was not a browser session, e.g. "api_token:atk_…". It is recorded as
	// the "via" detail, so the event stays attributed to the user.
	Via string
	// Run is the agent run the user's authority was exercised for (a SpaceMan MCP call, ADR-0063). It is recorded as
	// the "run" detail.
	Run string
	// Channel names the entry point a token's call came through when it is not the plain API: ChannelMCPEndpoint for
	// the external MCP endpoint (ADR-0063 §External endpoint). It is recorded as the "channel" detail.
	Channel string
}

// ChannelMCPEndpoint is the Channel of a call made through /api/v1/mcp by an operator's own MCP client.
const ChannelMCPEndpoint = "mcp_endpoint"

// ViaSpacemanMCP prefixes Via for calls made through a SpaceMan MCP server's internal token (ADR-0063).
const ViaSpacemanMCP = "spaceman_mcp:"

// Internal reports whether the actor acted through a SpaceMan MCP server's internal token: an agent using its owner's
// authority, never a person at the keyboard.
func (a Actor) Internal() bool { return strings.HasPrefix(a.Via, ViaSpacemanMCP) }

// MCP reports whether the actor acted through one of SpaceMan's MCP entry points (a SpaceMan MCP server's internal
// token, or the external MCP endpoint): a model choosing the call, never a person deciding a request.
func (a Actor) MCP() bool { return a.Internal() || a.Channel == ChannelMCPEndpoint }

// System is the actor for automatic actions (expiry, reconciliation, jobs).
var System = Actor{Type: ActorSystem, ID: "spaceman"}

// Target identifies what an action applied to.
type Target struct {
	Type string
	ID   string
}

// Event is one audit record.
type Event struct {
	Actor         Actor
	Action        string // dotted, e.g. "agent.key.revoked"
	Target        Target
	Outcome       string
	SourceIP      string
	CorrelationID string
	// Details are non-sensitive structured fields stored in plaintext.
	Details map[string]any
	// Sensitive details (free text such as revocation reasons) are sealed with the field key.
	Sensitive map[string]any
}

// Record is a stored audit event as returned to readers.
type Record struct {
	ID            string         `json:"id"`
	Seq           int64          `json:"seq"`
	At            int64          `json:"at"`
	ActorType     string         `json:"actor_type"`
	ActorID       string         `json:"actor_id,omitempty"`
	Action        string         `json:"action"`
	TargetType    string         `json:"target_type,omitempty"`
	TargetID      string         `json:"target_id,omitempty"`
	Outcome       string         `json:"outcome"`
	SourceIP      string         `json:"source_ip,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	Details       map[string]any `json:"details,omitempty"`
	HasSensitive  bool           `json:"has_sensitive,omitempty"`
}

// Sink receives committed audit records (e.g. a JSON-lines file for SIEM ingestion).
type Sink interface {
	Write(Record) error
}

// Writer appends audit events.
type Writer struct {
	kr   *keyring.Keyring
	mu   sync.RWMutex
	sink Sink
	pub  Publisher
}

// Publisher records a committed audit record for outbound publication inside the same transaction
// (the transactional outbox, ADR-0015). It receives only the non-sensitive Record.
type Publisher interface {
	RecordAudit(ctx context.Context, tx *store.Tx, r Record) error
}

// NewWriter returns a writer. kr may be nil only if events never carry sensitive details.
func NewWriter(kr *keyring.Keyring) *Writer { return &Writer{kr: kr} }

// SetSink attaches a sink that receives every record after its transaction commits.
func (w *Writer) SetSink(s Sink) {
	w.mu.Lock()
	w.sink = s
	w.mu.Unlock()
}

// SetPublisher publishes every record through p; the published payload is the record as a sink receives it,
// never the sensitive details. nil turns publishing off.
func (w *Writer) SetPublisher(p Publisher) {
	w.mu.Lock()
	w.pub = p
	w.mu.Unlock()
}

// GenesisHash is the prev_hash of the first event.
var GenesisHash = make([]byte, sha256.Size)

// Append writes ev inside tx and returns its ID.
func (w *Writer) Append(ctx context.Context, tx *store.Tx, ev Event) (string, error) {
	if ev.Action == "" || ev.Actor.Type == "" {
		return "", errors.New("audit: action and actor type are required")
	}
	if ev.Outcome == "" {
		ev.Outcome = OutcomeSuccess
	}
	if m := MetaFrom(ctx); ev.SourceIP == "" || ev.CorrelationID == "" {
		if ev.SourceIP == "" {
			ev.SourceIP = m.SourceIP
		}
		if ev.CorrelationID == "" {
			ev.CorrelationID = m.CorrelationID
		}
	}
	ev = withCorrelation(ctx, ev)
	if ev.Actor.Via != "" || ev.Actor.Run != "" || ev.Actor.Channel != "" {
		d := make(map[string]any, len(ev.Details)+3)
		maps.Copy(d, ev.Details)
		if ev.Actor.Via != "" {
			d["via"] = ev.Actor.Via
		}
		if ev.Actor.Run != "" {
			d["run"] = ev.Actor.Run
		}
		if ev.Actor.Channel != "" {
			d["channel"] = ev.Actor.Channel
		}
		ev.Details = d
	}
	details, err := canonicalJSON(ev.Details)
	if err != nil {
		return "", fmt.Errorf("audit details: %w", err)
	}
	id := ids.New(ids.Audit)
	var enc []byte
	if len(ev.Sensitive) > 0 {
		if w.kr == nil {
			return "", errors.New("audit: sensitive details require a keyring")
		}
		pt, err := canonicalJSON(ev.Sensitive)
		if err != nil {
			return "", err
		}
		if enc, err = w.kr.Seal(keyring.PurposeField, keyring.FieldAAD("audit_events", "details_enc", id), []byte(pt)); err != nil {
			return "", err
		}
	}
	seq := int64(1)
	prev := GenesisHash
	last, err := tx.Q.LastAuditEvent(ctx)
	switch {
	case err == nil:
		seq, prev = last.Seq+1, last.Hash
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	}
	row := db.InsertAuditEventParams{
		ID: id, Seq: seq, At: tx.NowMicros(), ActorType: ev.Actor.Type, ActorID: nullStr(ev.Actor.ID), Action: ev.Action,
		TargetType: nullStr(ev.Target.Type), TargetID: nullStr(ev.Target.ID), Outcome: ev.Outcome,
		SourceIp: nullStr(ev.SourceIP), CorrelationID: nullStr(ev.CorrelationID), DetailsJson: details, DetailsEnc: enc,
		PrevHash: prev,
	}
	row.Hash = chainHash(prev, rowFields(row.ID, row.Seq, row.At, row.ActorType, row.ActorID.String, row.Action,
		row.TargetType.String, row.TargetID.String, row.Outcome, row.SourceIp.String, row.CorrelationID.String, row.DetailsJson, row.DetailsEnc))
	if err := tx.Q.InsertAuditEvent(ctx, row); err != nil {
		return "", err
	}
	w.mu.RLock()
	sink, pub := w.sink, w.pub
	w.mu.RUnlock()
	if sink == nil && pub == nil {
		return id, nil
	}
	r := Record{ID: id, Seq: seq, At: row.At, ActorType: row.ActorType, ActorID: ev.Actor.ID, Action: ev.Action,
		TargetType: ev.Target.Type, TargetID: ev.Target.ID, Outcome: ev.Outcome, SourceIP: ev.SourceIP,
		CorrelationID: ev.CorrelationID, Details: ev.Details, HasSensitive: enc != nil}
	if pub != nil {
		if err := pub.RecordAudit(ctx, tx, r); err != nil {
			return "", fmt.Errorf("audit event: %w", err)
		}
	}
	if sink != nil {
		tx.AfterCommit(func() { _ = sink.Write(r) })
	}
	return id, nil
}

// CorrelationKey is the details key holding the correlation IDs taken from the context (SM-OBS-001).
const CorrelationKey = "correlation"

// withCorrelation fills the correlation ID (HTTP request ID, else trace ID) and records the context's trace and
// lifecycle IDs under details["correlation"], unless the caller set them. The caller's map is not modified.
func withCorrelation(ctx context.Context, ev Event) Event {
	traceID, _ := obs.TraceIDs(ctx)
	ids := obs.IDs(ctx)
	if ev.CorrelationID == "" {
		ev.CorrelationID = obs.ID(ctx, obs.FieldHTTPRequestID)
		if ev.CorrelationID == "" {
			ev.CorrelationID = traceID
		}
	}
	if traceID == "" && len(ids) == 0 {
		return ev
	}
	if _, set := ev.Details[CorrelationKey]; set {
		return ev
	}
	corr := make(map[string]any, len(ids)+1)
	if traceID != "" {
		corr["trace_id"] = traceID
	}
	for _, id := range ids {
		corr[strings.ReplaceAll(strings.TrimPrefix(id.Key(), "spaceman."), ".", "_")] = id.Value()
	}
	details := make(map[string]any, len(ev.Details)+1)
	maps.Copy(details, ev.Details)
	details[CorrelationKey] = corr
	ev.Details = details
	return ev
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// canonicalJSON renders m with sorted keys (encoding/json sorts map keys); nil → "{}".
func canonicalJSON(m map[string]any) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// rowFields length-prefixes every field so that no two distinct rows serialize identically.
func rowFields(id string, seq, at int64, actorType, actorID, action, targetType, targetID, outcome, sourceIP,
	correlationID, details string, enc []byte) []byte {
	var buf bytes.Buffer
	writeField := func(b []byte) {
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(len(b)))
		buf.Write(l[:])
		buf.Write(b)
	}
	var n [8]byte
	writeField([]byte("spaceman/audit/v1"))
	writeField([]byte(id))
	binary.BigEndian.PutUint64(n[:], uint64(seq)) //nolint:gosec // positive by CHECK
	writeField(n[:])
	binary.BigEndian.PutUint64(n[:], uint64(at)) //nolint:gosec // timestamps are positive
	writeField(n[:])
	for _, s := range []string{actorType, actorID, action, targetType, targetID, outcome, sourceIP, correlationID, details} {
		writeField([]byte(s))
	}
	writeField(enc)
	return buf.Bytes()
}

func chainHash(prev, fields []byte) []byte {
	h := sha256.New()
	h.Write(prev)
	h.Write(fields)
	return h.Sum(nil)
}

// VerifyResult summarizes a chain verification.
type VerifyResult struct {
	Events   int64  `json:"events"`
	OK       bool   `json:"ok"`
	BrokenAt int64  `json:"broken_at,omitempty"`
	Problem  string `json:"problem,omitempty"`
	HeadHash []byte `json:"head_hash,omitempty"`
	// HeadSeq is the sequence of the newest event (0 when there is none).
	HeadSeq int64 `json:"head_seq,omitempty"`
	// PrunedThrough is the last sequence deleted by retention (0: nothing pruned); verification starts after it, from
	// the recorded hash of that last pruned event.
	PrunedThrough int64 `json:"pruned_through,omitempty"`
}

// Pruned is the audit log's pruned prefix: every event up to Through was deleted by retention.
type Pruned struct {
	Through int64  `json:"through_seq"`
	Hash    []byte `json:"-"`
	At      int64  `json:"through_at" doc:"Unix microseconds of the last pruned event"`
	Count   int64  `json:"pruned" doc:"Events deleted by retention so far"`
}

// PrunedPrefix returns the audit log's pruned prefix, or nil when nothing was pruned. A database from before audit
// retention (no audit_prune table: an older backup) has none.
func PrunedPrefix(ctx context.Context, q *db.Queries) (*Pruned, error) {
	p, err := q.GetAuditPrune(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil && strings.Contains(err.Error(), "no such table"):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &Pruned{Through: p.ThroughSeq, Hash: p.ThroughHash, At: p.ThroughAt, Count: p.Pruned}, nil
}

// Verify walks the whole chain in sequence order and reports the first inconsistency (SM-OBS-003). When retention
// pruned a prefix, the walk starts at the first retained event, linked to the recorded hash of the last pruned one.
func Verify(ctx context.Context, q *db.Queries) (VerifyResult, error) {
	const page = 1000
	res := VerifyResult{OK: true}
	prev := GenesisHash
	var after, expectSeq int64 = 0, 1
	pr, err := PrunedPrefix(ctx, q)
	if err != nil {
		return res, err
	}
	if pr != nil {
		prev, expectSeq, res.PrunedThrough = pr.Hash, pr.Through+1, pr.Through
	}
	for {
		rows, err := q.AuditEventsFrom(ctx, db.AuditEventsFromParams{Seq: after, Limit: page})
		if err != nil {
			return res, err
		}
		for _, r := range rows {
			res.Events++
			switch {
			case r.Seq != expectSeq:
				return fail(res, r.Seq, fmt.Sprintf("sequence gap: expected %d, found %d (events deleted)", expectSeq, r.Seq)), nil
			case !bytes.Equal(r.PrevHash, prev):
				return fail(res, r.Seq, "prev_hash does not match the previous event (events reordered or deleted)"), nil
			}
			want := chainHash(prev, rowFields(r.ID, r.Seq, r.At, r.ActorType, r.ActorID.String, r.Action, r.TargetType.String,
				r.TargetID.String, r.Outcome, r.SourceIp.String, r.CorrelationID.String, r.DetailsJson, r.DetailsEnc))
			if !bytes.Equal(want, r.Hash) {
				return fail(res, r.Seq, "hash mismatch (event modified)"), nil
			}
			prev, after, res.HeadSeq = r.Hash, r.Seq, r.Seq
			expectSeq++
		}
		if len(rows) < page {
			res.HeadHash = prev
			return res, nil
		}
	}
}

// PruneBatch bounds the events one Prune call deletes, so that one write transaction stays short.
const PruneBatch = 5000

// Prune deletes, inside tx, up to PruneBatch of the oldest events recorded before cutoff (Unix microseconds), always
// keeping the newest event (the chain's head), records the pruned prefix and appends an "audit.pruned" event. It
// returns how many events it deleted and the last deleted sequence; call it again while it deletes a full batch.
func (w *Writer) Prune(ctx context.Context, tx *store.Tx, cutoff int64, actor Actor) (deleted, through int64, err error) {
	last, err := tx.Q.LastAuditEvent(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	} else if err != nil {
		return 0, 0, err
	}
	pr, err := PrunedPrefix(ctx, tx.Q)
	if err != nil {
		return 0, 0, err
	}
	from := int64(0)
	if pr != nil {
		from = pr.Through
	}
	b, err := tx.Q.AuditPruneBoundary(ctx, db.AuditPruneBoundaryParams{Cutoff: cutoff, MaxSeq: min(last.Seq-1, from+PruneBatch)})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	} else if err != nil {
		return 0, 0, err
	}
	if b.Seq <= from {
		return 0, 0, nil
	}
	n, err := tx.Q.DeleteAuditThrough(ctx, b.Seq)
	if err != nil || n == 0 {
		return 0, 0, err
	}
	if err := tx.Q.UpsertAuditPrune(ctx, db.UpsertAuditPruneParams{ThroughSeq: b.Seq, ThroughHash: b.Hash, ThroughAt: b.At, Pruned: n,
		UpdatedAt: tx.NowMicros()}); err != nil {
		return 0, 0, err
	}
	if _, err = w.Append(ctx, tx, Event{Actor: actor, Action: "audit.pruned", Target: Target{Type: "audit", ID: strconv.FormatInt(b.Seq, 10)},
		Details: map[string]any{"through_seq": b.Seq, "through_at": b.At, "deleted": n, "reason": "retention"}}); err != nil {
		return 0, 0, err
	}
	return n, b.Seq, nil
}

func fail(res VerifyResult, seq int64, problem string) VerifyResult {
	res.OK, res.BrokenAt, res.Problem = false, seq, problem
	return res
}

// DecodeRecord converts a stored row to a Record (without decrypting sensitive details).
func DecodeRecord(r db.AuditEvent) Record {
	rec := Record{
		ID: r.ID, Seq: r.Seq, At: r.At, ActorType: r.ActorType, ActorID: r.ActorID.String, Action: r.Action,
		TargetType: r.TargetType.String, TargetID: r.TargetID.String, Outcome: r.Outcome, SourceIP: r.SourceIp.String,
		CorrelationID: r.CorrelationID.String, HasSensitive: len(r.DetailsEnc) > 0,
	}
	if r.DetailsJson != "" && r.DetailsJson != "{}" {
		_ = json.Unmarshal([]byte(r.DetailsJson), &rec.Details)
	}
	return rec
}

// OpenSensitive decrypts the sensitive details of a stored event.
func (w *Writer) OpenSensitive(r db.AuditEvent) (map[string]any, error) {
	if len(r.DetailsEnc) == 0 {
		return nil, nil
	}
	pt, err := w.kr.Open(keyring.PurposeField, keyring.FieldAAD("audit_events", "details_enc", r.ID), r.DetailsEnc)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(pt, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// JSONSink writes one JSON object per line.
type JSONSink struct {
	mu sync.Mutex
	w  io.Writer
}

// NewJSONSink returns a sink writing to w.
func NewJSONSink(w io.Writer) *JSONSink { return &JSONSink{w: w} }

// Write implements Sink.
func (s *JSONSink) Write(r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.w.Write(append(b, '\n'))
	return err
}
