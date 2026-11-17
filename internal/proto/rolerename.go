package proto

import (
	"errors"
	"regexp"
)

// Role rename (protocol minor 14, ADR-0093). A rename of a space role commits on central at once and moves central's
// copy of the role's memory; the space's node holds the memory itself, under the role's name, so central queues one
// durable memory.rename command for it. The node moves its role folder and answers memory.renamed; central records the
// outcome and answers memory.renamed.ack. The node keeps the outcome and sends it again on every reconnect until central
// has acknowledged it (the relocation-report pattern), so a lost report or ack only delays the end of the rename.
//
// The command is execution in the space: like every command other than run.stop it needs the node's lease of the space
// at the command's generation. Central sends it at the space's current generation. Central queues it only for a node that
// negotiated minor 14 or later; a rename on a space whose node is older is refused (node_too_old), since central cannot
// know what memory the node holds.
//
// Results:
//   - ok: the folder is under the new name (moved now or before; nothing to move counts too). Central ends the rename
//     (node_state done) and launches of the role run again.
//   - deferred: the node did not move yet, because a run of the role or a dream run in the space is still active on it.
//     It moves when that run ends, or at its next start, and reports again. The rename stays pending.
//   - error: the node could not move the folder (Message says why). The rename is failed and the role stays held; the
//     node tries again at its next start and reports again, and a later ok ends the rename.
//
// Only ok is final: a deferred or failed rename becomes done once an ok arrives, and a deferred report after an error
// makes it pending again. A report for a rename that is already done changes nothing. Until the rename is done, central refuses memory syncs under the old name and under the new one
// ("role renamed") and creates no collection for it: the node offers that history again under the new name after its
// move.

// MinorRoleRename is the first minor version that carries memory.rename, memory.renamed and memory.renamed.ack.
const MinorRoleRename = 14

// Results of a memory.rename.
const (
	MemoryRenameOK       = "ok"
	MemoryRenameDeferred = "deferred"
	MemoryRenameError    = "error"
)

// MemoryRenameClosed is the Error of a memory.renamed.ack for a rename an admin closed (ADR-0093 addendum): central gave up
// the node's move, so the node drops the rename if it has not moved the folder yet and leaves the old folder in place.
const MemoryRenameClosed = "closed"

// MemoryRenameMaxMessage bounds MemoryRenamed.Message; central truncates a longer one.
const MemoryRenameMaxMessage = 500

var (
	reRenameID = regexp.MustCompile(`^rrn_[0-9a-z]{26}$`)
	reRoleName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
)

// MemoryRename is the payload of a memory.rename command: move the memory folder of role From in the space to To. The
// node never merges: a folder already under To is set aside (kept, not deleted) before the move.
type MemoryRename struct {
	RenameID string `json:"rename_id"`
	SpaceID  string `json:"space_id"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// Validate checks the payload before any side effect.
func (m MemoryRename) Validate() error {
	switch {
	case !reRenameID.MatchString(m.RenameID):
		return errors.New("invalid rename_id")
	case !reSpace.MatchString(m.SpaceID):
		return errors.New("invalid space_id")
	case !reRoleName.MatchString(m.From) || !reRoleName.MatchString(m.To):
		return errors.New("invalid role name")
	case m.From == m.To:
		return errors.New("from and to are the same role")
	}
	return nil
}

// MemoryRenamed is the node's outcome of a memory.rename (node → central).
type MemoryRenamed struct {
	RenameID string `json:"rename_id"`
	SpaceID  string `json:"space_id"`
	Result   string `json:"result"`
	Message  string `json:"message,omitempty"`
}

// Validate checks a report's identifiers and result.
func (m MemoryRenamed) Validate() error {
	switch {
	case !reRenameID.MatchString(m.RenameID):
		return errors.New("invalid rename_id")
	case !reSpace.MatchString(m.SpaceID):
		return errors.New("invalid space_id")
	}
	switch m.Result {
	case MemoryRenameOK, MemoryRenameDeferred, MemoryRenameError:
		return nil
	}
	return errors.New("invalid result")
}

// MemoryRenamedAck answers a MemoryRenamed (central → node): central has recorded the report, or will never record it
// (Error says why: the rename is unknown or not this node's). Either way the node stops sending that report again.
type MemoryRenamedAck struct {
	RenameID string `json:"rename_id"`
	Result   string `json:"result"`
	Error    string `json:"error,omitempty"`
}
