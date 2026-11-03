// Package xspace is cross-space collaboration (SM-XSPACE-001…003, SM-INV-008, ADR-0038). One space's control asks
// another space a question, delegates work to it, or asks one of its knowledge bases, only through SpaceMan and only
// as far as both spaces allow:
//
//   - Permissions are per ordered pair and separate: the destination says what it accepts from the source (discovery,
//     questions, delegation, knowledge, and what its replies may disclose, with quotas); the source says what its own
//     control may send to the destination. Nothing is allowed without a row on both sides.
//   - A question or delegation lands in the destination's inbox for its control, which accepts, declines and replies.
//     The destination owns everything that happens in it: no tool of the source creates tasks or runs there.
//   - A knowledge request is routed to the destination KB's scheduler as a support session whose requester is the
//     source run ("space:<id>/run:<id>"); the source never reads the KB repository.
//   - Replies reach the source reduced to the destination's disclosure level. Requests are correlated, have deadlines,
//     can be cancelled by the source, carry their depth and the spaces their chain visited (loops are refused), and
//     are bounded by quotas. Everything is durable; notifications are messages, and deadlines share the human-action
//     timer, so nothing polls.
package xspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/kb"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Request kinds.
const (
	Question   = "question"
	Delegation = "delegation"
	Knowledge  = "knowledge"
)

// Request states.
const (
	Pending   = "pending"
	Accepted  = "accepted"
	Completed = "completed"
	Failed    = "failed"
	Declined  = "declined"
	Cancelled = "cancelled"
	Expired   = "expired"
)

// Disclosure levels: what a destination's replies to a peer may contain.
const (
	DiscloseNone     = "none"     // the outcome only: no reply text, no reasons
	DiscloseSummary  = "summary"  // reply text up to SummaryChars, no evidence
	DiscloseDetailed = "detailed" // reply text up to DetailedChars and the KB's evidence (paths and revisions)
)

// Bounds.
const (
	SummaryChars    = 2000
	DetailedChars   = 20000
	MaxTitle        = 200
	MaxBody         = 20000
	MaxReason       = 500
	DefaultDeadline = 24 * time.Hour
	MinDeadline     = 5 * time.Minute
	MaxDeadline     = 7 * 24 * time.Hour
	// DefaultMaxDepth is how deep a chain of requests a space accepts unless its settings say otherwise.
	DefaultMaxDepth = 3
	// CeilingDepth bounds every space's max_depth setting.
	CeilingDepth       = 8
	DefaultMaxOutgoing = 60
	DefaultMaxPerHour  = 20
	DefaultMaxPending  = 5
	defaultLimit       = 100
	maxDueBatch        = 200
	actorID            = "xspace"
	requestIDPrefix    = "xrq"
	requestResource    = "xspace_request"
	peerResource       = "xspace_peer"
	settingsResource   = "xspace_settings"
	originKind         = "xspace_request"
	inboxSourceKind    = "cross_space"
	messageFromKind    = "system"
)

var (
	system  = audit.Actor{Type: audit.ActorSystem, ID: actorID}
	reKB    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	kinds   = []string{Question, Delegation, Knowledge}
	levels  = []string{DiscloseNone, DiscloseSummary, DiscloseDetailed}
	errPeer = apperr.New(apperr.CodeNotFound, "no such peer configuration")
)

// Service manages cross-space permissions and requests.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	inbox   *inbox.Service
	queue   *queue.Service
	kb      *kb.Service
	clock   clock.Clock
	log     *slog.Logger

	// Outbox records request events for publication (SM-OBS-004). Nil: nothing is published.
	Outbox *outbox.Outbox
	// Notified runs after a commit that stored a message (the messages notifier's Kick).
	Notified func()
	// Kick wakes the deadline timer after a commit that may move the earliest deadline (human.Service.Kick).
	Kick func()
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, in *inbox.Service, q *queue.Service, k *kb.Service, c clock.Clock, log *slog.Logger) *Service {
	if c == nil {
		c = clock.System{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{s: s, audit: aw, changes: ch, inbox: in, queue: q, kb: k, clock: c, log: log}
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func tp(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMicro(v.Int64).UTC()
	return &t
}

func micros(v int64) time.Time { return time.UnixMicro(v).UTC() }

func ignoreNoRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func clip(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	return string([]rune(s)[:n]), true
}

// ---- Settings ----

// Settings are a space's cross-space settings.
type Settings struct {
	SpaceID            string     `json:"space_id"`
	Profile            string     `json:"profile" doc:"What peers allowed to discover this space see about it (written for agents)"`
	MaxDepth           int64      `json:"max_depth" doc:"Requests deeper than this in a chain are refused here (1-8)"`
	MaxOutgoingPerHour int64      `json:"max_outgoing_per_hour" doc:"Requests this space's control may send per hour, to every peer together"`
	Version            int64      `json:"version" doc:"0 until the settings are first saved"`
	UpdatedAt          *time.Time `json:"updated_at,omitempty"`
	UpdatedBy          string     `json:"updated_by,omitempty"`
}

// SettingsInput edits a space's settings.
type SettingsInput struct {
	Profile            string `json:"profile,omitempty" maxLength:"2000"`
	MaxDepth           int64  `json:"max_depth,omitempty" doc:"1-8; default 3"`
	MaxOutgoingPerHour int64  `json:"max_outgoing_per_hour,omitempty" doc:"1-1000; default 60"`
}

func settingsOut(spaceID string, r db.XspaceSetting, found bool) *Settings {
	if !found {
		return &Settings{SpaceID: spaceID, MaxDepth: DefaultMaxDepth, MaxOutgoingPerHour: DefaultMaxOutgoing}
	}
	t := micros(r.UpdatedAt)
	return &Settings{SpaceID: spaceID, Profile: r.Profile, MaxDepth: r.MaxDepth, MaxOutgoingPerHour: r.MaxOutgoingPerHour, Version: r.Version,
		UpdatedAt: &t, UpdatedBy: r.UpdatedBy}
}

func settingsOf(ctx context.Context, q *db.Queries, spaceID string) (*Settings, error) {
	r, err := q.GetXSpaceSettings(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return settingsOut(spaceID, r, false), nil
	} else if err != nil {
		return nil, err
	}
	return settingsOut(spaceID, r, true), nil
}

// GetSettings returns a space's settings (defaults when never saved).
func (svc *Service) GetSettings(ctx context.Context, spaceID string) (*Settings, error) {
	if _, err := svc.s.R().GetSpace(ctx, spaceID); err != nil {
		return nil, apperr.New(apperr.CodeNotFound, "space not found")
	}
	return settingsOf(ctx, svc.s.R(), spaceID)
}

// SaveSettings saves a space's settings; version is the one read (0 when they were never saved).
func (svc *Service) SaveSettings(ctx context.Context, actor audit.Actor, spaceID string, version int64, in SettingsInput) (*Settings, error) {
	v := &apperr.Validation{}
	in.Profile = strings.TrimSpace(in.Profile)
	if utf8.RuneCountInString(in.Profile) > 2000 {
		v.Add("profile", "at most 2000 characters")
	}
	if in.MaxDepth == 0 {
		in.MaxDepth = DefaultMaxDepth
	}
	if in.MaxDepth < 1 || in.MaxDepth > CeilingDepth {
		v.Add("max_depth", "must be between 1 and %d", CeilingDepth)
	}
	if in.MaxOutgoingPerHour == 0 {
		in.MaxOutgoingPerHour = DefaultMaxOutgoing
	}
	if in.MaxOutgoingPerHour < 1 || in.MaxOutgoingPerHour > 1000 {
		v.Add("max_outgoing_per_hour", "must be between 1 and 1000")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	err := svc.s.W().TxNamed(ctx, "xspace.settings", func(tx *store.Tx) error {
		if _, err := tx.Q.GetSpace(ctx, spaceID); err != nil {
			return apperr.New(apperr.CodeNotFound, "space not found")
		}
		cur, err := tx.Q.GetXSpaceSettings(ctx, spaceID)
		action := changefeed.Updated
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if version != 0 {
				return apperr.New(apperr.CodePrecondition, "the settings were never saved; save them with version 0")
			}
			action = changefeed.Created
			if err := tx.Q.InsertXSpaceSettings(ctx, db.InsertXSpaceSettingsParams{SpaceID: spaceID, Profile: in.Profile, MaxDepth: in.MaxDepth,
				MaxOutgoingPerHour: in.MaxOutgoingPerHour, UpdatedAt: tx.NowMicros(), UpdatedBy: actor.ID}); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if version != cur.Version {
				return apperr.New(apperr.CodePrecondition, "the settings changed since version %d (now %d)", version, cur.Version)
			}
			if _, err := tx.Q.UpdateXSpaceSettings(ctx, db.UpdateXSpaceSettingsParams{Profile: in.Profile, MaxDepth: in.MaxDepth,
				MaxOutgoingPerHour: in.MaxOutgoingPerHour, Now: tx.NowMicros(), By: actor.ID, SpaceID: spaceID, Version: version}); err != nil {
				return err
			}
		}
		if err := svc.changes.Record(ctx, tx, spaceID, settingsResource, spaceID, action, version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "xspace.settings_updated", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"space_id": spaceID, "max_depth": in.MaxDepth, "max_outgoing_per_hour": in.MaxOutgoingPerHour, "profile_chars": utf8.RuneCountInString(in.Profile)}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.GetSettings(ctx, spaceID)
}

// ---- Peers ----

// Inbound is what a space accepts from a peer.
type Inbound struct {
	Discover   bool     `json:"discover" doc:"The peer's control may see this space and its profile"`
	Questions  bool     `json:"questions" doc:"The peer may ask this space's control questions"`
	Delegation bool     `json:"delegation" doc:"The peer may ask this space's control to do work (this space decides and runs it)"`
	Knowledge  bool     `json:"knowledge" doc:"The peer may ask this space's knowledge bases (through support sessions, never the repository)"`
	KBs        []string `json:"kbs" maxItems:"50" doc:"KB slugs the peer may ask; empty means every enabled KB"`
	Disclosure string   `json:"disclosure" enum:"none,summary,detailed" doc:"What replies to the peer may contain: none (the outcome only), summary (text up to 2,000 characters), detailed (text up to 20,000 characters and KB evidence)"`
	MaxPerHour int64    `json:"max_per_hour" doc:"Requests the peer may send here per hour (1-1000; default 20)"`
	MaxPending int64    `json:"max_pending" doc:"Requests from the peer open here at once (1-100; default 5)"`
}

// Outbound is what a space's control may send to a peer.
type Outbound struct {
	Questions  bool `json:"questions"`
	Delegation bool `json:"delegation"`
	Knowledge  bool `json:"knowledge"`
}

// SpaceRef names a space.
type SpaceRef struct {
	ID   string `json:"id"`
	Slug string `json:"slug,omitempty"`
	Name string `json:"name,omitempty"`
}

// Peer is a space's configuration for one peer space.
type Peer struct {
	SpaceID   string    `json:"space_id"`
	Peer      SpaceRef  `json:"peer"`
	Inbound   Inbound   `json:"inbound"`
	Outbound  Outbound  `json:"outbound"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

// PeerInput sets a space's configuration for a peer.
type PeerInput struct {
	Inbound  Inbound  `json:"inbound"`
	Outbound Outbound `json:"outbound"`
}

func parseList(s string) []string {
	out := []string{}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func listJSON(l []string) string {
	if l == nil {
		l = []string{}
	}
	b, _ := json.Marshal(l)
	return string(b)
}

func inboundOf(r db.XspacePeer) Inbound {
	return Inbound{Discover: r.InDiscover == 1, Questions: r.InQuestions == 1, Delegation: r.InDelegation == 1, Knowledge: r.InKnowledge == 1,
		KBs: parseList(r.InKbsJson), Disclosure: r.Disclosure, MaxPerHour: r.MaxPerHour, MaxPending: r.MaxPending}
}

func outboundOf(r db.XspacePeer) Outbound {
	return Outbound{Questions: r.OutQuestions == 1, Delegation: r.OutDelegation == 1, Knowledge: r.OutKnowledge == 1}
}

func (svc *Service) spaceRef(ctx context.Context, q *db.Queries, id string) SpaceRef {
	sp, err := q.GetSpace(ctx, id)
	if err != nil {
		return SpaceRef{ID: id}
	}
	return SpaceRef{ID: id, Slug: sp.Slug, Name: sp.Name}
}

func (svc *Service) peerOut(ctx context.Context, r db.XspacePeer) *Peer {
	return &Peer{SpaceID: r.SpaceID, Peer: svc.spaceRef(ctx, svc.s.R(), r.PeerSpaceID), Inbound: inboundOf(r), Outbound: outboundOf(r), Version: r.Version,
		CreatedAt: micros(r.CreatedAt), UpdatedAt: micros(r.UpdatedAt), UpdatedBy: r.UpdatedBy}
}

func (in *PeerInput) normalize() error {
	v := &apperr.Validation{}
	if in.Inbound.Disclosure == "" {
		in.Inbound.Disclosure = DiscloseNone
	}
	if !slices.Contains(levels, in.Inbound.Disclosure) {
		v.Add("inbound.disclosure", "must be none, summary or detailed")
	}
	if in.Inbound.MaxPerHour == 0 {
		in.Inbound.MaxPerHour = DefaultMaxPerHour
	}
	if in.Inbound.MaxPerHour < 1 || in.Inbound.MaxPerHour > 1000 {
		v.Add("inbound.max_per_hour", "must be between 1 and 1000")
	}
	if in.Inbound.MaxPending == 0 {
		in.Inbound.MaxPending = DefaultMaxPending
	}
	if in.Inbound.MaxPending < 1 || in.Inbound.MaxPending > 100 {
		v.Add("inbound.max_pending", "must be between 1 and 100")
	}
	if in.Inbound.KBs == nil {
		in.Inbound.KBs = []string{}
	}
	if len(in.Inbound.KBs) > 50 {
		v.Add("inbound.kbs", "at most 50")
	}
	for _, k := range in.Inbound.KBs {
		if !reKB.MatchString(k) {
			v.Add("inbound.kbs", "%q is not a KB slug", k)
		}
	}
	return v.Err()
}

// ListPeers returns a space's peer configurations.
func (svc *Service) ListPeers(ctx context.Context, spaceID string) ([]*Peer, error) {
	rows, err := svc.s.R().ListXSpacePeers(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]*Peer, 0, len(rows))
	for _, r := range rows {
		out = append(out, svc.peerOut(ctx, r))
	}
	return out, nil
}

// ListAllPeers returns every space's peer configurations (global administration).
func (svc *Service) ListAllPeers(ctx context.Context) ([]*Peer, error) {
	rows, err := svc.s.R().ListAllXSpacePeers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Peer, 0, len(rows))
	for _, r := range rows {
		out = append(out, svc.peerOut(ctx, r))
	}
	return out, nil
}

// GetPeer returns a space's configuration for one peer.
func (svc *Service) GetPeer(ctx context.Context, spaceID, peerID string) (*Peer, error) {
	r, err := svc.s.R().GetXSpacePeer(ctx, db.GetXSpacePeerParams{SpaceID: spaceID, PeerSpaceID: peerID})
	if err != nil {
		return nil, errPeer
	}
	return svc.peerOut(ctx, r), nil
}

// SavePeer creates (version 0) or updates a space's configuration for a peer. Each space edits only its own side:
// what it accepts and what its control may send.
func (svc *Service) SavePeer(ctx context.Context, actor audit.Actor, spaceID, peerID string, version int64, in PeerInput) (*Peer, error) {
	if err := in.normalize(); err != nil {
		return nil, err
	}
	if peerID == spaceID {
		return nil, apperr.New(apperr.CodeInvalid, "a space is not its own peer")
	}
	err := svc.s.W().TxNamed(ctx, "xspace.peer", func(tx *store.Tx) error {
		if _, err := tx.Q.GetSpace(ctx, spaceID); err != nil {
			return apperr.New(apperr.CodeNotFound, "space not found")
		}
		if _, err := tx.Q.GetSpace(ctx, peerID); err != nil {
			return apperr.New(apperr.CodeNotFound, "peer space %q not found", peerID)
		}
		now := tx.NowMicros()
		cur, err := tx.Q.GetXSpacePeer(ctx, db.GetXSpacePeerParams{SpaceID: spaceID, PeerSpaceID: peerID})
		action := changefeed.Updated
		ib, ob := in.Inbound, in.Outbound
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if version != 0 {
				return errPeer
			}
			action = changefeed.Created
			if err := tx.Q.InsertXSpacePeer(ctx, db.InsertXSpacePeerParams{SpaceID: spaceID, PeerSpaceID: peerID, InDiscover: b2i(ib.Discover),
				InQuestions: b2i(ib.Questions), InDelegation: b2i(ib.Delegation), InKnowledge: b2i(ib.Knowledge), InKbsJson: listJSON(ib.KBs),
				Disclosure: ib.Disclosure, MaxPerHour: ib.MaxPerHour, MaxPending: ib.MaxPending, OutQuestions: b2i(ob.Questions),
				OutDelegation: b2i(ob.Delegation), OutKnowledge: b2i(ob.Knowledge), CreatedAt: now, CreatedBy: actor.ID, UpdatedAt: now, UpdatedBy: actor.ID}); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if version == 0 {
				return apperr.New(apperr.CodeAlreadyExists, "this space already has a configuration for that peer (version %d)", cur.Version)
			}
			n, err := tx.Q.UpdateXSpacePeer(ctx, db.UpdateXSpacePeerParams{InDiscover: b2i(ib.Discover), InQuestions: b2i(ib.Questions),
				InDelegation: b2i(ib.Delegation), InKnowledge: b2i(ib.Knowledge), InKbsJson: listJSON(ib.KBs), Disclosure: ib.Disclosure,
				MaxPerHour: ib.MaxPerHour, MaxPending: ib.MaxPending, OutQuestions: b2i(ob.Questions), OutDelegation: b2i(ob.Delegation),
				OutKnowledge: b2i(ob.Knowledge), Now: now, By: actor.ID, SpaceID: spaceID, PeerSpaceID: peerID, Version: version})
			if err != nil {
				return err
			}
			if n == 0 {
				return apperr.New(apperr.CodePrecondition, "the configuration changed since version %d (now %d)", version, cur.Version)
			}
		}
		if err := svc.changes.Record(ctx, tx, spaceID, peerResource, peerID, action, version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "xspace.peer_" + action, Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"space_id": spaceID, "peer_space_id": peerID, "in_discover": ib.Discover, "in_questions": ib.Questions,
				"in_delegation": ib.Delegation, "in_knowledge": ib.Knowledge, "in_kbs": ib.KBs, "disclosure": ib.Disclosure, "max_per_hour": ib.MaxPerHour,
				"max_pending": ib.MaxPending, "out_questions": ob.Questions, "out_delegation": ob.Delegation, "out_knowledge": ob.Knowledge}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.GetPeer(ctx, spaceID, peerID)
}

// DeletePeer removes a space's configuration for a peer: nothing is allowed in either direction any more (requests
// already open stay open; replies to them then disclose nothing).
func (svc *Service) DeletePeer(ctx context.Context, actor audit.Actor, spaceID, peerID string) error {
	return svc.s.W().TxNamed(ctx, "xspace.peer_delete", func(tx *store.Tx) error {
		n, err := tx.Q.DeleteXSpacePeer(ctx, db.DeleteXSpacePeerParams{SpaceID: spaceID, PeerSpaceID: peerID})
		if err != nil {
			return err
		}
		if n == 0 {
			return errPeer
		}
		if err := svc.changes.Record(ctx, tx, spaceID, peerResource, peerID, changefeed.Deleted, 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "xspace.peer_deleted", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"space_id": spaceID, "peer_space_id": peerID}})
		return err
	})
}
