package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// handleWorktreeRemove runs a space.worktree.remove command in the background (ADR-0107). The removal is only
// removeWorktree, so every refusal of the safe-removal path stands; the command carries no way to force anything. The
// outcome (removed, refused with its code, or error) is journaled with the command and reported as worktree.removed, and
// a repeated delivery re-sends the stored report without removing anything again.
func (a *Agent) handleWorktreeRemove(ctx context.Context, c proto.Command) {
	var p proto.WorktreeRemovePayload
	if err := json.Unmarshal(c.Payload, &p); err != nil || p.Validate() != nil {
		a.j.commandDone(ctx, c.CommandID, "rejected", "bad space.worktree.remove payload")
		a.ack(ctx, c, proto.AckRejected, "bad space.worktree.remove payload")
		return
	}
	l, ok := a.lease(c.SpaceID, c.Generation)
	if !ok || l.Workdir == "" {
		a.j.commandDone(ctx, c.CommandID, "rejected", "no valid lease for this space and generation")
		a.ack(ctx, c, proto.AckRejected, "no valid lease for this space and generation")
		return
	}
	a.ack(ctx, c, proto.AckAccepted, "")
	a.goTrack(func() {
		ctx := context.WithoutCancel(ctx)
		rep := a.worktreeRemoveReport(ctx, c, a.spaceDir(l.Workdir), p)
		b, _ := json.Marshal(rep)
		a.j.commandDone(ctx, c.CommandID, "done", string(b))
		a.sendWorktreeRemoved(ctx, rep)
	})
}

// worktreeRemoveReport removes the worktree and describes the outcome.
func (a *Agent) worktreeRemoveReport(ctx context.Context, c proto.Command, space string, p proto.WorktreeRemovePayload) proto.WorktreeRemoved {
	rep := proto.WorktreeRemoved{CommandID: c.CommandID, SpaceID: c.SpaceID, Repo: p.Repository.Path, Name: p.Name}
	out, err := a.removeWorktree(ctx, c.SpaceID, space, p.Repository, p.Name)
	rep.Path, rep.Branch, rep.Head = out.Path, out.Branch, out.Head
	describeRemoval(&rep, err)
	rep.CheckedAt = a.now().UTC().Truncate(time.Second)
	return rep
}

// describeRemoval sets rep's result (and code or reason) from the error of a removal.
func describeRemoval(rep *proto.WorktreeRemoved, err error) {
	var r *WorktreeRefusal
	var pe *pruneError
	switch {
	case err == nil:
		rep.Result = proto.WorktreeRemovedOK
	case errors.As(err, &r):
		rep.Result, rep.Code, rep.Reason = proto.WorktreeRefused, r.Code, truncateStr(r.Reason, proto.WorktreeRemoveMaxReason)
	case errors.As(err, &pe):
		// The worktree is gone; a stale entry of another worktree may remain.
		rep.Result, rep.Reason = proto.WorktreeRemovedOK, truncateStr(err.Error(), proto.WorktreeRemoveMaxReason)
	default:
		rep.Result, rep.Reason = proto.WorktreeFailed, truncateStr(err.Error(), proto.WorktreeRemoveMaxReason)
	}
}

// resendWorktreeRemoved re-sends the stored report of a repeated space.worktree.remove (central keeps the first).
func (a *Agent) resendWorktreeRemoved(ctx context.Context, c proto.Command) {
	var state, reason string
	if err := a.j.s.ReadDB().QueryRowContext(ctx, `SELECT state, reason FROM commands WHERE id = ?`, c.CommandID).Scan(&state, &reason); err != nil || state != "done" {
		return
	}
	var rep proto.WorktreeRemoved
	if json.Unmarshal([]byte(reason), &rep) == nil && rep.CommandID == c.CommandID {
		a.sendWorktreeRemoved(ctx, rep)
	}
}

func (a *Agent) sendWorktreeRemoved(ctx context.Context, rep proto.WorktreeRemoved) {
	if !a.protocolAtLeast(proto.MinorWorktreeRemove) {
		return
	}
	if err := a.send(ctx, proto.TypeWorktreeRemoved, rep); err != nil {
		a.log.Debug("worktree removal report not delivered; central asking again re-sends it", slog.String(obs.FieldSpaceID, rep.SpaceID))
	}
}
