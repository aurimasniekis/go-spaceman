package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// The node half of a role rename (ADR-0093 §3.3, protocol minor 14). Central renamed a space role and queued one durable
// memory.rename for the space's node, which holds the role's memory under <data>/memory/<space>/<role>. The node
// journals the rename (role_renames), moves that folder to the new name and reports memory.renamed until central
// acknowledges the report:
//
//   - ok: the folder is under the new name (moved now or before; nothing to move counts too).
//   - deferred: a run of either name, or a dream run, is still active in the space on this node; its memory path, prompt
//     and checkpoint timers name the old folder. The move is tried again when a run of the space ends, and at start.
//   - error: the move failed. The rename stays journaled and is tried again at the next agent start.
//
// The move never merges and never deletes: a folder already under the new name is a leftover (central refused the
// rename if it had a collection of that name) and is set aside under <space>/.retained/. Before the move every instance
// checkout of the old name is checkpointed, so edits a run left behind are part of the history that moves; whatever a
// checkpoint could not commit stays in the checkout, which moves with the folder. Instance checkouts fetch from the shared
// repository by path and keep no remote, so they work under the new name as they are. The collection's sync state moves
// with it (acked keeps naming commits central already has; retries and the last error reset), so the next sync bundles
// from where the old name stopped. Central refuses memory under both names until it has recorded the ok, so the node
// holds syncs of both names until central acknowledged that ok, then syncs the new name at once.
//
// Crash safety: the state goes pending → moving (journaled) before the folder is renamed, and the sync state moves in the
// same journal transaction that marks the rename done. A rename found moving at start is finished: the old folder, if
// still there, is renamed; if it is gone, the rename happened before the crash.

const (
	renamePending = "pending"
	renameMoving  = "moving"
	renameDone    = "done"
	renameDropped = "dropped"
)

// renameRow is one journaled role rename.
type renameRow struct {
	ID, SpaceID, From, To, State, Result, Message string
	Acked                                         bool
}

// handleMemoryRename handles a memory.rename command (fresh or repeated). A rename the journal already holds is answered
// again from it: a done one with ok, an open one by trying the move again.
func (a *Agent) handleMemoryRename(ctx context.Context, c proto.Command, fresh bool) {
	reject := func(reason string) {
		a.j.commandDone(ctx, c.CommandID, "rejected", reason)
		a.ack(ctx, c, proto.AckRejected, reason)
	}
	var p proto.MemoryRename
	if err := json.Unmarshal(c.Payload, &p); err != nil {
		reject("bad memory.rename payload")
		return
	}
	if err := p.Validate(); err != nil {
		reject(err.Error())
		return
	}
	if p.SpaceID != c.SpaceID {
		reject("the rename is for another space")
		return
	}
	r, found, err := a.j.roleRename(ctx, p.RenameID)
	if err != nil {
		a.log.Warn("journal role rename", obs.Err(err))
		return // no ack: central sends it again after a reconnect
	}
	if found && (r.SpaceID != p.SpaceID || r.From != p.From || r.To != p.To) {
		reject("the rename id names other roles than the journaled rename")
		return
	}
	if !found {
		if _, ok := a.lease(c.SpaceID, c.Generation); !ok {
			reject("no valid lease for this space and generation")
			return
		}
		if err := a.j.insertRoleRename(ctx, renameRow{ID: p.RenameID, SpaceID: p.SpaceID, From: p.From, To: p.To, State: renamePending}); err != nil {
			a.log.Warn("journal role rename", obs.Err(err))
			return
		}
	}
	if fresh {
		a.ack(ctx, c, proto.AckAccepted, "")
	} else {
		a.ack(ctx, c, proto.AckDuplicate, "")
	}
	a.j.commandDone(ctx, c.CommandID, "done", "")
	bctx := context.WithoutCancel(ctx)
	a.goTrack(func() { a.renameMemory(bctx, p.RenameID, true) })
}

// renameMemory tries a journaled rename and reports its outcome. resend reports a done rename's ok again even when central
// acknowledged it (a repeated command asks for it); an unchanged outcome central has acknowledged is not sent again.
func (a *Agent) renameMemory(ctx context.Context, id string, resend bool) {
	a.renameMu.Lock()
	defer a.renameMu.Unlock()
	r, found, err := a.j.roleRename(ctx, id)
	if err != nil || !found {
		return
	}
	log := a.log.With(slog.String(obs.FieldSpaceID, r.SpaceID), slog.String("rename.id", r.ID), slog.String("rename.from", r.From),
		slog.String("rename.to", r.To))
	switch r.State {
	case renameDropped:
		return
	case renameDone:
		if !resend && r.Acked {
			return
		}
		if err := a.j.setRoleRenameResult(ctx, r.ID, proto.MemoryRenameOK, ""); err != nil {
			log.Warn("journal role rename result", obs.Err(err))
			return
		}
	default:
		result, msg := a.moveRoleMemory(ctx, r)
		switch result {
		case proto.MemoryRenameOK:
			log.Info("role memory moved to the new name")
		case proto.MemoryRenameDeferred:
			log.Info("role memory move deferred", slog.String("reason", msg))
		default:
			log.Warn("role memory move failed; tried again at the next start", slog.String("reason", msg))
		}
		if result != proto.MemoryRenameOK { // ok is journaled with the move itself (finishRoleRename)
			if !resend && r.Acked && r.Result == result && r.Message == msg {
				return // nothing new for central
			}
			if err := a.j.setRoleRenameResult(ctx, r.ID, result, msg); err != nil {
				log.Warn("journal role rename result", obs.Err(err))
				return
			}
		}
	}
	if r, found, err = a.j.roleRename(ctx, id); err == nil && found {
		a.sendRoleRenamed(ctx, r)
	}
}

// moveRoleMemory moves the rename's role folder (ADR-0093 §3.3 steps 2-7) and returns the outcome to report. The memory
// locks of both names are held throughout, so no checkpoint, preparation or sync of either name runs meanwhile.
func (a *Agent) moveRoleMemory(ctx context.Context, r renameRow) (result, msg string) {
	m := a.mem
	keys := []string{colKey(r.SpaceID, r.From), colKey(r.SpaceID, r.To)}
	slices.Sort(keys)
	defer m.lock(keys[0])()
	defer m.lock(keys[1])()
	// Checked with both locks held: a launch preparing either name holds its lock, so no run can start between this
	// check and the move (prepare refuses the old name once the rename is journaled).
	if busy, err := a.renameBusy(ctx, r); err != nil {
		return proto.MemoryRenameError, "list the node's runs: " + err.Error()
	} else if busy != "" {
		return proto.MemoryRenameDeferred, busy
	}
	m.forget(r.SpaceID, r.From)
	m.forget(r.SpaceID, r.To)
	dir := filepath.Join(m.root, r.SpaceID)
	from, to := filepath.Join(dir, r.From), filepath.Join(dir, r.To)
	if r.State == renamePending {
		if m.git != nil && exists(filepath.Join(from, "shared.git")) {
			m.checkpointAll(ctx, r.SpaceID, r.From)
		}
		// Not moved yet, so whatever is under the new name is a leftover: set it aside, never merge it.
		if exists(to) {
			if err := retainRenamed(dir, to, r); err != nil {
				return proto.MemoryRenameError, "set the existing " + r.To + " folder aside: " + err.Error()
			}
		}
		if err := a.j.setRoleRenameState(ctx, r.ID, renameMoving); err != nil {
			return proto.MemoryRenameError, "journal the move: " + err.Error()
		}
	}
	// Moving: the old folder is renamed now, or was before a crash (then it is gone and the new one is the moved one).
	if exists(from) {
		if exists(to) { // recreated after the leftover was set aside: still not the renamed role's
			if err := retainRenamed(dir, to, r); err != nil {
				return proto.MemoryRenameError, "set the existing " + r.To + " folder aside: " + err.Error()
			}
		}
		if err := os.Rename(from, to); err != nil {
			return proto.MemoryRenameError, "move the folder: " + err.Error()
		}
	}
	if err := a.j.finishRoleRename(ctx, r); err != nil {
		return proto.MemoryRenameError, "journal the moved folder: " + err.Error()
	}
	return proto.MemoryRenameOK, ""
}

// retainRenamed moves a leftover folder under the rename's new name to <space>/.retained/rename-<id>-<to>[-n]. The dot
// folder is never a collection: collections() needs <name>/shared.git and validMemoryKey refuses the name.
func retainRenamed(spaceDir, to string, r renameRow) error {
	root := filepath.Join(spaceDir, ".retained")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	name := "rename-" + r.ID + "-" + r.To
	dst := filepath.Join(root, name)
	for i := 1; exists(dst); i++ {
		dst = filepath.Join(root, fmt.Sprintf("%s-%d", name, i))
	}
	return os.Rename(to, dst)
}

// renameBusy names what keeps the rename from moving now: an active run of either name in the space, or an active dream
// run in the space (its workspace links the role's checkout by the old name). "" means nothing does.
func (a *Agent) renameBusy(ctx context.Context, r renameRow) (string, error) {
	runs, err := a.j.runs(ctx, true)
	if err != nil {
		return "", err
	}
	for _, x := range runs {
		switch {
		case x.SpaceID != r.SpaceID:
		case x.Role == r.From || x.Role == r.To:
			return fmt.Sprintf("run %s of role %s is active on the node; the memory moves when it ends", x.RunID, x.Role), nil
		case x.Kind == "dreamer":
			return fmt.Sprintf("dream run %s is active in the space on the node; the memory moves when it ends", x.RunID), nil
		}
	}
	return "", nil
}

// retryRoleRenames tries the open renames again: at start every one (spaceID ""), when a run of a space ended only that
// space's deferred ones (a failed move waits for the next start).
func (a *Agent) retryRoleRenames(ctx context.Context, spaceID string) {
	rows, err := a.j.openRoleRenames(ctx)
	if err != nil {
		a.log.Warn("list open role renames", obs.Err(err))
		return
	}
	for _, r := range rows {
		if spaceID != "" && (r.SpaceID != spaceID || r.Result != proto.MemoryRenameDeferred) {
			continue
		}
		a.renameMemory(ctx, r.ID, false)
	}
}

// sendRoleRenamed reports a rename's outcome when the connection speaks minor 14. A report that does not go out stays
// unacknowledged in the journal and is sent again after the next welcome (resendRoleRenames).
func (a *Agent) sendRoleRenamed(ctx context.Context, r renameRow) {
	if r.Result == "" || !a.protocolAtLeast(proto.MinorRoleRename) {
		return
	}
	rep := proto.MemoryRenamed{RenameID: r.ID, SpaceID: r.SpaceID, Result: r.Result, Message: truncateStr(r.Message, proto.MemoryRenameMaxMessage)}
	if err := a.send(ctx, proto.TypeMemoryRenamed, rep); err != nil {
		return
	}
	_ = a.j.roleRenameReported(ctx, r.ID)
}

// resendRoleRenames sends every outcome central has not acknowledged yet (after a welcome).
func (a *Agent) resendRoleRenames(ctx context.Context) {
	if !a.protocolAtLeast(proto.MinorRoleRename) {
		return
	}
	rows, err := a.j.unackedRoleRenames(ctx)
	if err != nil {
		return
	}
	for _, r := range rows {
		a.sendRoleRenamed(ctx, r)
	}
}

// roleRenamedAck handles central's memory.renamed.ack: the report it answers is not sent again. An ack with an error
// (central does not know the rename, or it is not this node's) ends a rename that has not moved: nothing waits for it.
// Once central has the ok, the role's memory syncs under the new name at once.
func (a *Agent) roleRenamedAck(ctx context.Context, ack proto.MemoryRenamedAck) {
	a.renameMu.Lock()
	r, found, err := a.j.roleRename(ctx, ack.RenameID)
	if err == nil && found {
		err = a.j.ackRoleRename(ctx, r.ID, ack.Result, ack.Error != "")
	}
	a.renameMu.Unlock()
	if err != nil || !found {
		return
	}
	if ack.Error != "" {
		a.log.Warn("central did not record the role rename's outcome", slog.String("rename.id", r.ID), slog.String("reason", ack.Error))
	}
	if r.State == renameDone && ack.Result == proto.MemoryRenameOK && a.mem.git != nil {
		a.mem.scheduleSync(r.SpaceID, r.To, 0)
		a.mem.sendStatus(ctx)
	}
}

// forget drops the memory manager's in-memory state of a collection: its sync schedule, the sync waiting for an answer
// and its instances' checkpoint timers. The caller holds the collection's lock and has checkpointed what mattered.
func (m *memManager) forget(spaceID, role string) {
	key := colKey(spaceID, role)
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.syncs[key]; ok {
		t.Stop()
		delete(m.syncs, key)
	}
	delete(m.waiting, key)
	for k, t := range m.timers {
		if len(k) > len(key) && k[:len(key)+1] == key+"/" {
			t.Stop()
			delete(m.timers, k)
		}
	}
}

// checkpointAll checkpoints every instance checkout of a collection; the caller holds its lock. A checkout that cannot be
// checkpointed keeps its changes in its files, which move with the folder.
func (m *memManager) checkpointAll(ctx context.Context, spaceID, role string) {
	insts, _ := os.ReadDir(filepath.Join(m.root, spaceID, role, "instances"))
	for _, i := range insts {
		if !i.IsDir() || !exists(filepath.Join(m.instance(spaceID, role, i.Name()), ".git")) {
			continue
		}
		if _, err := m.publish(ctx, spaceID, role, i.Name(), "rename"); err != nil {
			m.a.log.Warn("memory checkpoint before a role rename failed; the changes move with the folder", slog.String(obs.FieldSpaceID, spaceID),
				slog.String("memory.role", role), slog.String("memory.instance", i.Name()), obs.Err(err))
		}
	}
}

// renameHeld reports whether the collection (spaceID, role) takes part in a rename central has not ended yet: one that
// has not moved, or whose ok central has not acknowledged. Central refuses memory under both names until then.
func (j *Journal) renameHeld(ctx context.Context, spaceID, role string) bool {
	var n int
	err := j.s.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM role_renames WHERE space_id = ? AND (from_role = ? OR to_role = ?)
		AND (state IN ('pending', 'moving') OR (state = 'done' AND acked = 0))`, spaceID, role, role).Scan(&n)
	return err != nil || n > 0
}

// renameFrom reports whether role is the old name of a rename that has not been dropped: its memory moves (or moved) to
// the new name, so nothing may start a collection under the old one.
func (j *Journal) renameFrom(ctx context.Context, spaceID, role string) bool {
	var n int
	err := j.s.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM role_renames WHERE space_id = ? AND from_role = ?
		AND state IN ('pending', 'moving', 'done')`, spaceID, role).Scan(&n)
	return err != nil || n > 0
}

const renameCols = `rename_id, space_id, from_role, to_role, state, result, message, acked`

func scanRename(sc interface{ Scan(...any) error }) (renameRow, error) {
	var r renameRow
	var acked int64
	err := sc.Scan(&r.ID, &r.SpaceID, &r.From, &r.To, &r.State, &r.Result, &r.Message, &acked)
	r.Acked = acked == 1
	return r, err
}

func (j *Journal) roleRename(ctx context.Context, id string) (renameRow, bool, error) {
	r, err := scanRename(j.s.ReadDB().QueryRowContext(ctx, `SELECT `+renameCols+` FROM role_renames WHERE rename_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

func (j *Journal) listRenames(ctx context.Context, where string) ([]renameRow, error) {
	rows, err := j.s.ReadDB().QueryContext(ctx, `SELECT `+renameCols+` FROM role_renames WHERE `+where+` ORDER BY created_at, rename_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []renameRow
	for rows.Next() {
		r, err := scanRename(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (j *Journal) openRoleRenames(ctx context.Context) ([]renameRow, error) {
	return j.listRenames(ctx, `state IN ('pending', 'moving')`)
}

func (j *Journal) unackedRoleRenames(ctx context.Context) ([]renameRow, error) {
	return j.listRenames(ctx, `acked = 0 AND result != ''`)
}

func (j *Journal) insertRoleRename(ctx context.Context, r renameRow) error {
	return j.s.W().TxNamed(ctx, "journal.role_rename", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO role_renames (rename_id, space_id, from_role, to_role, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (rename_id) DO NOTHING`, r.ID, r.SpaceID, r.From, r.To, r.State, tx.NowMicros(), tx.NowMicros())
		return err
	})
}

func (j *Journal) setRoleRenameState(ctx context.Context, id, state string) error {
	return j.s.W().TxNamed(ctx, "journal.role_rename_state", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE role_renames SET state = ?, updated_at = ? WHERE rename_id = ?`, state, tx.NowMicros(), id)
		return err
	})
}

// setRoleRenameResult records the outcome to report; central has not acknowledged it yet.
func (j *Journal) setRoleRenameResult(ctx context.Context, id, result, msg string) error {
	return j.s.W().TxNamed(ctx, "journal.role_rename_result", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE role_renames SET result = ?, message = ?, acked = 0, updated_at = ? WHERE rename_id = ?`,
			result, msg, tx.NowMicros(), id)
		return err
	})
}

// finishRoleRename ends a moved rename in one transaction: the collection's sync state moves to the new name (a row the
// new name had belonged to the folder set aside), its retries and last error reset, and the rename is done with ok to
// report.
func (j *Journal) finishRoleRename(ctx context.Context, r renameRow) error {
	return j.s.W().TxNamed(ctx, "journal.role_rename_finish", func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(ctx, `DELETE FROM memory_sync WHERE space_id = ? AND role = ?`, r.SpaceID, r.To); err != nil {
			return err
		}
		if _, err := tx.SQL.ExecContext(ctx, `UPDATE memory_sync SET role = ?, retries = 0, last_error = '', updated_at = ? WHERE space_id = ? AND role = ?`,
			r.To, tx.NowMicros(), r.SpaceID, r.From); err != nil {
			return err
		}
		_, err := tx.SQL.ExecContext(ctx, `UPDATE role_renames SET state = 'done', result = 'ok', message = '', acked = 0, updated_at = ? WHERE rename_id = ?`,
			tx.NowMicros(), r.ID)
		return err
	})
}

func (j *Journal) roleRenameReported(ctx context.Context, id string) error {
	return j.s.W().TxNamed(ctx, "journal.role_rename_reported", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE role_renames SET reported_at = ? WHERE rename_id = ?`, tx.NowMicros(), id)
		return err
	})
}

// ackRoleRename marks the outcome central answered as acknowledged (only when it is still the outcome to report: an ack
// of an older report changes nothing). refused drops a rename that has not moved yet.
func (j *Journal) ackRoleRename(ctx context.Context, id, result string, refused bool) error {
	return j.s.W().TxNamed(ctx, "journal.role_rename_ack", func(tx *store.Tx) error {
		res, err := tx.SQL.ExecContext(ctx, `UPDATE role_renames SET acked = 1, updated_at = ? WHERE rename_id = ? AND result = ?`, tx.NowMicros(), id, result)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 && refused {
			_, err = tx.SQL.ExecContext(ctx, `UPDATE role_renames SET state = 'dropped' WHERE rename_id = ? AND state = 'pending'`, id)
		}
		return err
	})
}
