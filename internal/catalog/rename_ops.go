package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Operator operations on a stuck rename (ADR-0093 addendum, T-H4). A rename is open while its node state is pending or
// failed; both operations refuse any other state, so a repeated call is a 409 and changes nothing. The caller (the API)
// holds the admin-only permission RoleRenameAdmin; nothing here deletes anything.

// RenameNotOpen is the refusal reason when a retry or close finds the rename done, not_needed or abandoned.
const RenameNotOpen = "rename_not_open"

// MaxRenameCloseReason bounds the reason an admin gives when closing a rename.
const MaxRenameCloseReason = 500

// RenameRetried is the outcome of a retry.
type RenameRetried struct {
	Rename *RoleRename `json:"rename"`
	// Queued is true when a new memory.rename was queued. False: one is still waiting for the node (pending or sent), so
	// the retry only woke its delivery.
	Queued bool `json:"queued"`
	// NodeTooOld is true when the space's node last spoke a protocol below 1.14: the command waits until it connects at
	// 1.14 or more (upgrade it, or close the rename).
	NodeTooOld bool `json:"node_too_old"`
}

// openRename loads a rename of the space inside tx and refuses it unless it is open.
func openRename(ctx context.Context, q *db.Queries, spaceID, id string) (db.SpaceRoleRename, error) {
	r, err := q.GetSpaceRoleRename(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && r.SpaceID != spaceID) {
		return r, apperr.New(apperr.CodeNotFound, "role rename not found in this space")
	} else if err != nil {
		return r, err
	}
	if r.NodeState != RenameNodePending && r.NodeState != RenameNodeFailed {
		return r, apperr.Wrap(apperr.CodePrecondition, &RenameRefused{Reason: RenameNotOpen},
			"the rename of %s to %s is %s, not open: only a pending or failed rename can be retried or closed (%s)", r.FromName, r.ToName, r.NodeState, RenameNotOpen)
	}
	return r, nil
}

// RetryRoleRename sends an open rename's node move again: it queues a new memory.rename (key role-rename:<id>:<n>) unless
// one is still waiting for the node, and makes a failed rename pending. The node answers a repeated command from its
// journal, trying an open move again. It never ends the hold itself.
func (svc *Service) RetryRoleRename(ctx context.Context, actor audit.Actor, spaceID, id string) (*RenameRetried, error) {
	out := &RenameRetried{}
	err := svc.s.W().TxNamed(ctx, "catalog.role_rename_retry", func(tx *store.Tx) error {
		*out = RenameRetried{}
		r, err := openRename(ctx, tx.Q, spaceID, id)
		if err != nil {
			return err
		}
		sp, err := tx.Q.GetSpace(ctx, spaceID)
		if err != nil {
			return err
		}
		if sp.NodeID == "" || sp.NodeID != r.NodeID {
			return apperr.Wrap(apperr.CodePrecondition, &RenameRefused{Reason: RenameNotOpen},
				"the space is no longer on node %s, which holds the rename's memory folder; close the rename instead (%s)", r.NodeID, RenameNotOpen)
		}
		if n, err := tx.Q.GetNode(ctx, r.NodeID); err != nil {
			return err
		} else if n.ProtocolMinor < proto.MinorRoleRename {
			out.NodeTooOld = true
		}
		key := RenameCommandKey(r.ID)
		cmds, err := tx.Q.ListRoleRenameCommands(ctx, db.ListRoleRenameCommandsParams{NodeID: r.NodeID, Key: key, KeyEnd: key + ";"})
		if err != nil {
			return err
		}
		out.Queued = true
		if len(cmds) > 0 {
			if last := cmds[len(cmds)-1].State; last == "pending" || last == "sent" {
				out.Queued = false
			}
		}
		if out.Queued {
			if err := QueueMemoryRename(ctx, tx, sp, r.ID, fmt.Sprintf("%s:%d", key, len(cmds)), r.FromName, r.ToName); err != nil {
				return err
			}
		}
		if _, err := tx.Q.ReopenRoleRename(ctx, r.ID); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space", spaceID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "role_rename.retried", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"rename_id": r.ID, "from": r.FromName, "to": r.ToName, "node_id": r.NodeID, "previous_state": r.NodeState,
				"queued": out.Queued, "node_too_old": out.NodeTooOld}})
		return err
	})
	if err != nil {
		return nil, err
	}
	rn, err := svc.s.R().GetSpaceRoleRename(ctx, id)
	if err != nil {
		return nil, err
	}
	out.Rename = renameOut(rn)
	if svc.AfterRenameRetry != nil {
		svc.AfterRenameRetry(ctx, *out.Rename)
	}
	return out, nil
}

// RenameClosedKey is the idempotency key of the closed notice a forget queues for the node: a memory.rename with Closed
// set. It is outside the range of RenameCommandKey's keys, so it is never counted as the rename's move command.
func RenameClosedKey(renameID string) string { return "role-rename-closed:" + renameID }

// CloseRoleRename gives up an open rename's node move: the rename becomes abandoned and the holds on launches lift. Its
// undelivered commands stay queued, so a node that never journaled the rename still learns of it, and until it has,
// the rename keeps blocking renames of either name (renameBlockers). Central's memory stays under the new name; the
// node's old-name folder is left where it is (orphaned) and never deleted or synced again.
//
// forgetNode also gives up delivering the rename to the node (ADR-0093 addendum, forget): its pending and sent
// commands are cancelled, which releases both names, and, while the space is still on that node, a closed notice is
// queued in their place, so a node that comes back journals the rename as closed and sets the old folder aside instead
// of moving it. It is accepted on an open rename (which it also closes) and on a closed one whose command the node has
// not journaled yet; on anything else it is a 409 rename_not_open.
func (svc *Service) CloseRoleRename(ctx context.Context, actor audit.Actor, spaceID, id, reason string, forgetNode bool) (*RoleRename, error) {
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > MaxRenameCloseReason {
		v := &apperr.Validation{}
		v.Add("reason", "must be at most 500 characters")
		return nil, v.Err()
	}
	err := svc.s.W().TxNamed(ctx, "catalog.role_rename_close", func(tx *store.Tx) error {
		r, err := openRename(ctx, tx.Q, spaceID, id)
		var refused *RenameRefused
		closed := false
		if forgetNode && errors.As(err, &refused) && r.NodeState == RenameNodeAbandoned {
			// Already closed: a forget still applies while the node has not journaled the rename.
			if closed, err = undeliveredClosed(ctx, tx.Q, r); err == nil && !closed {
				err = apperr.Wrap(apperr.CodePrecondition, &RenameRefused{Reason: RenameNotOpen},
					"the rename of %s to %s is closed and node %s already has it (or it was forgotten): nothing to forget (%s)", r.FromName, r.ToName, r.NodeID, RenameNotOpen)
			}
		}
		if err != nil {
			return err
		}
		action := "role_rename.closed"
		if !closed {
			msg := fmt.Sprintf("closed by an admin; orphaned: %s on node %s", r.FromName, r.NodeID)
			if reason != "" {
				msg += ": " + reason
			}
			if n, err := tx.Q.CloseRoleRename(ctx, db.CloseRoleRenameParams{NodeMessage: msg, NodeDoneAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: r.ID}); err != nil {
				return err
			} else if n != 1 {
				return apperr.New(apperr.CodeConflict, "the rename changed concurrently")
			}
		}
		// Without forgetNode the rename's undelivered commands stay queued: a node that was offline (or downgraded below
		// minor 14) has no journal row yet, and must journal the rename when it connects, so it can move the folder or learn of the
		// close (ADR-0093 addendum 3).
		var cancelled int64
		if forgetNode {
			action = "role_rename.forgotten"
			if cancelled, err = svc.forgetRenameNode(ctx, tx, r); err != nil {
				return err
			}
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space", spaceID, "updated", 0); err != nil {
			return err
		}
		details := map[string]any{"rename_id": r.ID, "from": r.FromName, "to": r.ToName, "node_id": r.NodeID, "previous_state": r.NodeState, "reason": reason}
		if forgetNode {
			details["cancelled_commands"] = cancelled
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: action, Target: audit.Target{Type: "space", ID: spaceID}, Details: details})
		return err
	})
	if err != nil {
		return nil, err
	}
	rn, err := svc.s.R().GetSpaceRoleRename(ctx, id)
	if err != nil {
		return nil, err
	}
	out := renameOut(rn)
	if svc.AfterRenameClose != nil {
		svc.AfterRenameClose(ctx, *out)
	}
	return out, nil
}

// undeliveredClosed reports whether a closed rename's command has not been journaled by the node yet (it still blocks
// both names).
func undeliveredClosed(ctx context.Context, q *db.Queries, r db.SpaceRoleRename) (bool, error) {
	l, err := q.ListUndeliveredClosedRoleRenames(ctx, r.SpaceID)
	if err != nil {
		return false, err
	}
	for _, x := range l {
		if x.ID == r.ID {
			return true, nil
		}
	}
	return false, nil
}

// forgetRenameNode cancels a rename's memory.rename commands the node has not acknowledged and, while the space is
// still on the rename's node, queues the closed notice (RenameClosedKey) for it. It returns how many it cancelled.
func (svc *Service) forgetRenameNode(ctx context.Context, tx *store.Tx, r db.SpaceRoleRename) (int64, error) {
	key := RenameCommandKey(r.ID)
	n, err := tx.Q.CancelRoleRenameCommands(ctx, db.CancelRoleRenameCommandsParams{Reason: "the rename was closed and the node forgotten",
		NodeID: r.NodeID, Key: key, KeyEnd: key + ";"})
	if err != nil {
		return 0, err
	}
	sp, err := tx.Q.GetSpace(ctx, r.SpaceID)
	if err != nil {
		return 0, err
	}
	if sp.NodeID == "" || sp.NodeID != r.NodeID {
		return n, nil // the space left the node: nothing of it runs there again
	}
	return n, queueMemoryRename(ctx, tx, sp, proto.MemoryRename{RenameID: r.ID, SpaceID: sp.ID, From: r.FromName, To: r.ToName, Closed: true},
		RenameClosedKey(r.ID))
}
