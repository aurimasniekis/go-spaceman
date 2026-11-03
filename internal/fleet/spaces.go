package fleet

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

var reSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,46}[a-z0-9]$`)

// Space is a pinned project environment (SM-SPACE-001).
type Space struct {
	ID                   string     `json:"id"`
	Slug                 string     `json:"slug"`
	Name                 string     `json:"name"`
	Description          string     `json:"description"`
	Goals                string     `json:"goals"`
	StandingInstructions string     `json:"standing_instructions"`
	NodeID               string     `json:"node_id" doc:"The pinned node (effective assignment)"`
	DesiredNodeID        string     `json:"desired_node_id,omitempty" doc:"Relocation target while a relocation is in progress"`
	State                string     `json:"state" enum:"active,paused,archived"`
	PauseMode            string     `json:"pause_mode,omitempty" enum:",hold,interrupt,emergency"`
	DisconnectPolicy     string     `json:"disconnect_policy" enum:"interrupt,kill"`
	Workdir              string     `json:"workdir" doc:"Managed folder name on the node"`
	Lease                *Lease     `json:"lease,omitempty"`
	Version              int64      `json:"version"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	ArchivedAt           *time.Time `json:"archived_at,omitempty"`
}

// Lease is a space's execution authority.
type Lease struct {
	NodeID     string    `json:"node_id"`
	Generation int64     `json:"generation"`
	State      string    `json:"state"`
	ExpiresAt  time.Time `json:"expires_at"`
	Valid      bool      `json:"valid"`
}

func (svc *Service) spaceFromRow(ctx context.Context, q *db.Queries, r db.Space) *Space {
	s := &Space{ID: r.ID, Slug: r.Slug, Name: r.Name, Description: r.Description, Goals: r.Goals, StandingInstructions: r.StandingInstructions,
		NodeID: r.NodeID, DesiredNodeID: r.DesiredNodeID.String, State: r.State, PauseMode: r.PauseMode, DisconnectPolicy: r.DisconnectPolicy,
		Workdir: r.Workdir, Version: r.Version, CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(),
		ArchivedAt: microsPtr(r.ArchivedAt)}
	if l, err := q.GetLease(ctx, r.ID); err == nil {
		exp := time.UnixMicro(l.ExpiresAt).UTC()
		s.Lease = &Lease{NodeID: l.NodeID, Generation: l.Generation, State: l.State, ExpiresAt: exp, Valid: l.State == "active" && svc.now().Before(exp)}
	}
	return s
}

var errSpaceNotFound = apperr.New(apperr.CodeNotFound, "space not found")

// CreateSpaceInput creates a space pinned to a node.
type CreateSpaceInput struct {
	Slug                 string `json:"slug" doc:"Lowercase URL-safe identifier, unique (3-48 characters)"`
	Name                 string `json:"name" minLength:"1" maxLength:"128"`
	Description          string `json:"description,omitempty" maxLength:"2000"`
	Goals                string `json:"goals,omitempty" maxLength:"20000"`
	StandingInstructions string `json:"standing_instructions,omitempty" maxLength:"50000"`
	NodeID               string `json:"node_id" doc:"The node the space is pinned to"`
	DisconnectPolicy     string `json:"disconnect_policy,omitempty" enum:"interrupt,kill" doc:"What the node does with runs after losing central beyond the grace"`
}

// CreateSpace creates a space pinned to an existing, non-revoked node (SM-INV-002). If the node is connected, it is
// granted the space's first lease at once.
func (svc *Service) CreateSpace(ctx context.Context, actor audit.Actor, in CreateSpaceInput) (*Space, error) {
	v := &apperr.Validation{}
	in.Slug = strings.TrimSpace(in.Slug)
	if !reSlug.MatchString(in.Slug) {
		v.Add("slug", "must be 3-48 lowercase letters, digits or dashes, starting and ending with a letter or digit")
	}
	if strings.TrimSpace(in.Name) == "" {
		v.Add("name", "is required")
	}
	if in.DisconnectPolicy == "" {
		in.DisconnectPolicy = "interrupt"
	}
	if in.DisconnectPolicy != "interrupt" && in.DisconnectPolicy != "kill" {
		v.Add("disconnect_policy", "must be interrupt or kill")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	id := ids.New(ids.Space)
	workdir := in.Slug + "-" + id[len(id)-8:]
	err := svc.s.W().TxNamed(ctx, "fleet.create_space", func(tx *store.Tx) error {
		n, err := tx.Q.GetNode(ctx, in.NodeID)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeInvalid, "node_id: no such node")
		} else if err != nil {
			return err
		}
		if n.State == "revoked" || n.State == "disabled" {
			return apperr.New(apperr.CodePrecondition, "node %s is %s", n.Name, n.State)
		}
		if _, err := tx.Q.GetSpaceBySlug(ctx, in.Slug); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "a space with slug %q already exists", in.Slug)
		}
		if err := tx.Q.InsertSpace(ctx, db.InsertSpaceParams{ID: id, Slug: in.Slug, Name: strings.TrimSpace(in.Name), Description: in.Description,
			Goals: in.Goals, StandingInstructions: in.StandingInstructions, NodeID: in.NodeID, DisconnectPolicy: in.DisconnectPolicy, Workdir: workdir,
			CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor), UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := svc.record(ctx, tx, id, "space", id, "created", 1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space.created", Target: audit.Target{Type: "space", ID: id},
			Details: map[string]any{"slug": in.Slug, "node_id": in.NodeID}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.hub.Refresh(in.NodeID)
	return svc.GetSpace(ctx, id)
}

// GetSpace returns a space with its lease.
func (svc *Service) GetSpace(ctx context.Context, id string) (*Space, error) {
	r, err := svc.s.R().GetSpace(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errSpaceNotFound
	} else if err != nil {
		return nil, err
	}
	return svc.spaceFromRow(ctx, svc.s.R(), r), nil
}

// ListSpaces returns spaces; visible filters by the caller's authorization (nil: all).
func (svc *Service) ListSpaces(ctx context.Context, visible func(spaceID string) bool) ([]*Space, error) {
	rows, err := svc.s.R().ListSpaces(ctx)
	if err != nil {
		return nil, err
	}
	out := []*Space{}
	for _, r := range rows {
		if visible == nil || visible(r.ID) {
			out = append(out, svc.spaceFromRow(ctx, svc.s.R(), r))
		}
	}
	return out, nil
}

// UpdateSpaceInput edits a space's profile. Pinning changes go through relocation (SM-SPACE-005), never here.
type UpdateSpaceInput struct {
	Name                 string `json:"name" minLength:"1" maxLength:"128"`
	Description          string `json:"description" maxLength:"2000"`
	Goals                string `json:"goals" maxLength:"20000"`
	StandingInstructions string `json:"standing_instructions" maxLength:"50000"`
	DisconnectPolicy     string `json:"disconnect_policy" enum:"interrupt,kill"`
}

// UpdateSpace edits a space with optimistic concurrency.
func (svc *Service) UpdateSpace(ctx context.Context, actor audit.Actor, id string, version int64, in UpdateSpaceInput) (*Space, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, apperr.New(apperr.CodeInvalid, "name is required")
	}
	if in.DisconnectPolicy != "interrupt" && in.DisconnectPolicy != "kill" {
		return nil, apperr.New(apperr.CodeInvalid, "disconnect_policy must be interrupt or kill")
	}
	err := svc.s.W().TxNamed(ctx, "fleet.update_space", func(tx *store.Tx) error {
		n, err := tx.Q.UpdateSpaceProfile(ctx, db.UpdateSpaceProfileParams{Name: strings.TrimSpace(in.Name), Description: in.Description, Goals: in.Goals,
			StandingInstructions: in.StandingInstructions, DisconnectPolicy: in.DisconnectPolicy, UpdatedAt: tx.NowMicros(), ID: id, Version: version})
		if err != nil {
			return err
		}
		if n == 0 {
			return svc.versionConflict(ctx, tx, id, version)
		}
		if err := svc.record(ctx, tx, id, "space", id, "updated", version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space.updated", Target: audit.Target{Type: "space", ID: id}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.GetSpace(ctx, id)
}

func (svc *Service) versionConflict(ctx context.Context, tx *store.Tx, id string, version int64) error {
	cur, err := tx.Q.GetSpace(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return errSpaceNotFound
	} else if err != nil {
		return err
	}
	return apperr.New(apperr.CodeConflict, "the space changed (version %d, you sent %d); reload and retry", cur.Version, version)
}

// Pause modes (SM-SPACE-003).
const (
	PauseHold      = "hold"      // admit no new work; running work continues and drains
	PauseInterrupt = "interrupt" // also interrupt active runs and stop them
	PauseEmergency = "emergency" // also kill active runs immediately
)

// SetSpaceState pauses (with a mode), resumes, archives or restores a space. Archiving requires no active runs;
// pausing with interrupt or emergency stops them first.
func (svc *Service) SetSpaceState(ctx context.Context, actor audit.Actor, id string, version int64, state, mode string) (*Space, error) {
	switch {
	case state == "paused" && (mode == PauseHold || mode == PauseInterrupt || mode == PauseEmergency):
	case (state == "active" || state == "archived") && mode == "":
	default:
		return nil, apperr.New(apperr.CodeInvalid, "invalid state transition %q (mode %q)", state, mode)
	}
	var nodeID string
	err := svc.s.W().TxNamed(ctx, "fleet.set_space_state", func(tx *store.Tx) error {
		sp, err := tx.Q.GetSpace(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return errSpaceNotFound
		} else if err != nil {
			return err
		}
		nodeID = sp.NodeID
		if sp.Version != version {
			return apperr.New(apperr.CodeConflict, "the space changed (version %d, you sent %d); reload and retry", sp.Version, version)
		}
		active, err := tx.Q.ListActiveRunsBySpace(ctx, id)
		if err != nil {
			return err
		}
		switch {
		case state == "archived" && len(active) > 0:
			return apperr.New(apperr.CodePrecondition, "stop the space's %d active run(s) before archiving (pause with interrupt or emergency)", len(active))
		case sp.State == "archived" && state == "paused":
			return apperr.New(apperr.CodePrecondition, "restore the space before pausing it")
		case sp.DesiredNodeID.Valid && state != "paused":
			// A relocating space stays stopped until the relocation ends (SM-INV-003, SM-SPACE-005).
			return apperr.New(apperr.CodePrecondition, "the space is relocating; it stays paused until the relocation finishes or is cancelled")
		}
		if state == "paused" && mode != PauseHold {
			stopMode := "interrupt"
			if mode == PauseEmergency {
				stopMode = "kill"
			}
			for _, r := range active {
				if err := svc.stopRunTx(ctx, tx, r, stopMode, "space paused ("+mode+")"); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Q.SetSpaceState(ctx, db.SetSpaceStateParams{State: state, PauseMode: mode, Now: tx.NowMicros(), ID: id, Version: version}); err != nil {
			return err
		}
		if err := svc.record(ctx, tx, id, "space", id, "updated", version+1); err != nil {
			return err
		}
		action := "space." + state
		if state == "active" && sp.State == "archived" {
			action = "space.restored"
		} else if state == "active" {
			action = "space.resumed"
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: action, Target: audit.Target{Type: "space", ID: id},
			Details: map[string]any{"mode": mode, "stopped_runs": len(active)}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.hub.Kick(nodeID)
	return svc.GetSpace(ctx, id)
}

// PurgeSpace permanently deletes an archived space's SpaceMan records (runs, commands, lease). It never touches
// upstream repositories (SM-SPACE-004); the node's folder is retained per the node's recovery policy.
func (svc *Service) PurgeSpace(ctx context.Context, actor audit.Actor, id string) error {
	return svc.s.W().TxNamed(ctx, "fleet.purge_space", func(tx *store.Tx) error {
		sp, err := tx.Q.GetSpace(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return errSpaceNotFound
		} else if err != nil {
			return err
		}
		if sp.State != "archived" {
			return apperr.New(apperr.CodePrecondition, "only archived spaces can be purged")
		}
		for _, q := range []string{
			`DELETE FROM node_commands WHERE space_id = ?`,
			`DELETE FROM run_credentials WHERE run_id IN (SELECT id FROM runs WHERE space_id = ?)`,
			`DELETE FROM runs WHERE space_id = ?`,
			`DELETE FROM role_grants WHERE domain = ?`,
		} {
			if _, err := tx.SQL.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		if _, err := tx.Q.DeleteSpace(ctx, id); err != nil {
			return err
		}
		if err := svc.record(ctx, tx, id, "space", id, "deleted", sp.Version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space.purged", Target: audit.Target{Type: "space", ID: id},
			Details: map[string]any{"slug": sp.Slug}})
		return err
	})
}

// leaseFor grants or renews the lease of a space for its pinned node. A new grant keeps the generation when the same
// node held the previous (non-revoked) lease, and increments it otherwise.
func (svc *Service) leaseFor(ctx context.Context, tx *store.Tx, sp db.Space) (proto.LeaseGrant, error) {
	exp := tx.Now.Add(svc.opts.LeaseTTL)
	gen := int64(1)
	// granted is true when the lease starts, moves or resumes after expiry or revocation (not on a plain renewal); only
	// then do watchers of the space need to refetch it.
	granted := true
	if l, err := tx.Q.GetLease(ctx, sp.ID); err == nil {
		gen = l.Generation
		if l.NodeID != sp.NodeID || l.State == "revoked" {
			gen++
		} else if l.ExpiresAt > tx.NowMicros() {
			granted = false
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return proto.LeaseGrant{}, err
	}
	if granted {
		if err := svc.record(ctx, tx, sp.ID, "space", sp.ID, "updated", sp.Version); err != nil {
			return proto.LeaseGrant{}, err
		}
	}
	if err := tx.Q.UpsertLease(ctx, db.UpsertLeaseParams{SpaceID: sp.ID, NodeID: sp.NodeID, Generation: gen, GrantedAt: tx.NowMicros(),
		RenewedAt: tx.NowMicros(), ExpiresAt: exp.UnixMicro()}); err != nil {
		return proto.LeaseGrant{}, err
	}
	return proto.LeaseGrant{LeaseState: proto.LeaseState{SpaceID: sp.ID, Generation: gen, ExpiresAt: exp}, Workdir: sp.Workdir,
		DisconnectPolicy: sp.DisconnectPolicy, Grace: svc.opts.DisconnectGrace}, nil
}
