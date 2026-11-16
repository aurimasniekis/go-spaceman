package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// The node half of a role rename, as central sees it (ADR-0093 §3.3, protocol minor 14). The rename transaction queues
// one memory.rename for the space's node (catalog.RenameSpaceRole). The hub delivers it at the space's current lease
// generation, and only on a connection that negotiated minor 14. The node's memory.renamed report moves the rename's
// node_state (catalog's state machine: ok → done, error → failed, deferred → pending; done is final) and is
// acknowledged with memory.renamed.ack once recorded, so the node stops sending it again. A memory.rename the node
// refused (no valid lease at that moment, for example) is recorded on the rename and queued again when the node next
// connects.

// roleRenamed records a node's memory.renamed report and returns the ack to send. A report central cannot record now (a
// database error) returns ok false: no ack, so the node sends it again on its next connection.
func (svc *Service) roleRenamed(ctx context.Context, nodeID string, rep proto.MemoryRenamed) (proto.MemoryRenamedAck, bool) {
	ack := proto.MemoryRenamedAck{RenameID: rep.RenameID, Result: rep.Result}
	if err := rep.Validate(); err != nil {
		ack.Error = err.Error()
		return ack, true
	}
	msg := truncate(rep.Message, proto.MemoryRenameMaxMessage)
	var done bool
	err := svc.s.W().TxNamed(ctx, "fleet.memory_renamed", func(tx *store.Tx) error {
		ack.Error, done = "", false
		r, err := tx.Q.GetSpaceRoleRename(ctx, rep.RenameID)
		if errors.Is(err, sql.ErrNoRows) {
			ack.Error = "unknown rename"
			return nil
		} else if err != nil {
			return err
		}
		if r.NodeID != nodeID || r.SpaceID != rep.SpaceID {
			ack.Error = "not this node's rename"
			return nil
		}
		next, at := renameNodeNext(r.NodeState, rep.Result)
		if next == "" {
			return nil // already done (a repeated report) or not the node's to end: nothing changes
		}
		var doneAt sql.NullInt64
		if at {
			doneAt = sql.NullInt64{Int64: tx.NowMicros(), Valid: true}
		}
		n, err := tx.Q.SetRoleRenameNodeState(ctx, db.SetRoleRenameNodeStateParams{NodeState: next, NodeMessage: msg, NodeDoneAt: doneAt, ID: r.ID})
		if err != nil || n == 0 {
			return err
		}
		done = next == catalog.RenameNodeDone
		if err := svc.record(ctx, tx, r.SpaceID, "space", r.SpaceID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorNode, ID: nodeID}, Action: "space.role_rename_node",
			Target: audit.Target{Type: "space", ID: r.SpaceID}, Details: map[string]any{"rename_id": r.ID, "from": r.FromName, "to": r.ToName,
				"result": rep.Result, "node_state": next, "message": msg}})
		return err
	})
	if err != nil {
		svc.opts.Log.Warn("record a node's memory rename; the node reports it again", slog.String("rename.id", rep.RenameID), slog.Any("error", err))
		return ack, false
	}
	if done && svc.opts.OnRoleRenameDone != nil {
		svc.opts.OnRoleRenameDone(rep.SpaceID)
	}
	return ack, true
}

// renameNodeNext is the node state a report moves an open rename to ("" when nothing changes) and whether the move ends
// it (node_done_at is set). Only pending and failed renames are open; done and not_needed never change.
func renameNodeNext(state, result string) (string, bool) {
	if state != catalog.RenameNodePending && state != catalog.RenameNodeFailed {
		return "", false
	}
	switch result {
	case proto.MemoryRenameOK:
		return catalog.RenameNodeDone, true
	case proto.MemoryRenameError:
		return catalog.RenameNodeFailed, false
	case proto.MemoryRenameDeferred:
		return catalog.RenameNodePending, false
	}
	return "", false
}

// memoryRenameRejected records, inside the ack's transaction, that the node refused a memory.rename. The rename stays
// open; the command is queued again on the node's next connection (requeueRoleRenames).
func memoryRenameRejected(ctx context.Context, tx *store.Tx, cmd db.NodeCommand, reason string) error {
	var p proto.MemoryRename
	if err := json.Unmarshal([]byte(cmd.PayloadJson), &p); err != nil {
		return nil //nolint:nilerr // a payload central wrote and cannot read names no rename to update
	}
	_, err := tx.Q.SetRoleRenameNodeMessage(ctx, db.SetRoleRenameNodeMessageParams{NodeMessage: truncate("the node refused the command: "+reason,
		proto.MemoryRenameMaxMessage), ID: p.RenameID})
	return err
}

// requeueRoleRenames queues a memory.rename again, inside the node's connect transaction, for every open rename of the
// node whose last command the node refused or that has none left to deliver. A command that is pending, sent or accepted
// needs nothing: it is delivered again, or the node has it and reports its outcome until central acknowledges it.
func (svc *Service) requeueRoleRenames(ctx context.Context, tx *store.Tx, nodeID string) error {
	open, err := tx.Q.ListOpenRoleRenamesByNode(ctx, nodeID)
	if err != nil {
		return err
	}
	for _, r := range open {
		key := catalog.RenameCommandKey(r.ID)
		cmds, err := tx.Q.ListRoleRenameCommands(ctx, db.ListRoleRenameCommandsParams{NodeID: nodeID, Key: key, KeyEnd: key + ";"})
		if err != nil {
			return err
		}
		if len(cmds) > 0 {
			if last := cmds[len(cmds)-1]; last.State != "rejected" && last.State != "cancelled" {
				continue
			}
		}
		sp, err := tx.Q.GetSpace(ctx, r.SpaceID)
		if err != nil {
			return err
		}
		if sp.NodeID != nodeID {
			continue // the space is no longer on this node; nothing to move here
		}
		if err := catalog.QueueMemoryRename(ctx, tx, sp, r.ID, fmt.Sprintf("%s:%d", key, len(cmds)), r.FromName, r.ToName); err != nil {
			return err
		}
	}
	return nil
}

// memoryRenameGeneration is the lease generation a memory.rename goes out with: the space's current lease on this node.
// ok is false while the node holds no valid lease of the space, or negotiated a minor below MinorRoleRename: the
// command then waits, undelivered, for a connection that can take it.
func (h *Hub) memoryRenameGeneration(ctx context.Context, c db.NodeCommand, nodeID string, minor int) (int64, bool, error) {
	if minor < proto.MinorRoleRename {
		return 0, false, nil
	}
	l, err := h.svc.s.R().GetLease(ctx, c.SpaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	if l.NodeID != nodeID || l.State != "active" || l.ExpiresAt <= h.svc.now().UnixMicro() {
		return 0, false, nil
	}
	return l.Generation, true, nil
}

// errRenameNotDeliverable marks a memory.rename the hub leaves undelivered on this connection (memoryRenameGeneration).
var errRenameNotDeliverable = errors.New("memory.rename waits for a connection at protocol minor 14 that holds the space's lease")

// renameOpenForRelocation refuses a clean relocation while a role rename of the space is open: the node that holds the
// role's memory has not moved it yet, and a relocation would carry it away under the old name.
func renameOpenForRelocation(ctx context.Context, q *db.Queries, spaceID string) error {
	open, err := q.ListOpenSpaceRoleRenames(ctx, spaceID)
	if err != nil || len(open) == 0 {
		return err
	}
	r := open[0]
	return apperr.New(apperr.CodePrecondition, "the role %s was renamed to %s and node %s has not moved its memory yet (%s, rename %s); "+
		"relocate the space once the rename is done", r.FromName, r.ToName, r.NodeID, r.NodeState, r.ID)
}
