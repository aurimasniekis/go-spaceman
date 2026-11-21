package proto

import (
	"errors"
	"strings"
	"time"
)

// Worktree removal (protocol minor 16, ADR-0107 decision 4). Central asks the space's node to remove one worktree of a
// configured repository with the space.worktree.remove command. The node removes it only through its safe-removal path
// (every refusal there stands: the command can never force a removal) and answers with one worktree.removed report that
// carries the result or the refusal code. The command is execution in the space, so it needs the node's lease of the
// space at the command's generation, like space.repos.sync. Central queues it only for a node that negotiated minor 16
// or later.
//
// The node journals the report with the command: a repeated delivery of the command re-sends the stored report and
// never removes anything again. Central keeps the first report it has for a command.

// MinorWorktreeRemove is the first minor version that carries space.worktree.remove and worktree.removed.
const MinorWorktreeRemove = 16

// CmdWorktreeRemove removes one worktree of a configured repository (minor ≥ 16, ADR-0107).
const CmdWorktreeRemove = "space.worktree.remove"

// TypeWorktreeRemoved is the node's outcome of a space.worktree.remove command (node → central, minor ≥ 16).
const TypeWorktreeRemoved = "worktree.removed"

// Results of a worktree removal.
const (
	WorktreeRemovedOK = "removed" // the worktree is gone; the branch is kept
	WorktreeRefused   = "refused" // the node's safe-removal path refused; Code says why; nothing changed
	WorktreeFailed    = "error"   // the node could not tell or could not run the removal; nothing is known to be removed
)

// WorktreeRemoveMaxReason bounds WorktreeRemoved.Reason; the node truncates a longer one.
const WorktreeRemoveMaxReason = 500

// WorktreeRemovePayload is the body of space.worktree.remove: the configured repository (its URL and, from central at
// dispatch time, its SSH key, are what the node fetches to check the work against; never the URL in the repository's own
// configuration) and the worktree's folder name inside the space's worktrees folder.
type WorktreeRemovePayload struct {
	Repository RepoSource `json:"repository"`
	Name       string     `json:"name" doc:"The worktree's folder name inside the space's worktrees folder"`
}

// Redacted returns a copy without the repository's key (stored payloads never hold it).
func (p WorktreeRemovePayload) Redacted() WorktreeRemovePayload {
	p.Repository = p.Repository.Redacted()
	return p
}

// Validate checks the payload before any side effect. The node's removal path judges the folder names itself.
func (p WorktreeRemovePayload) Validate() error {
	switch {
	case p.Repository.ID == "" || len(p.Repository.ID) > 64:
		return errors.New("invalid repository id")
	case p.Repository.Path == "" || len(p.Repository.Path) > 512:
		return errors.New("invalid repository path")
	case p.Name == "" || len(p.Name) > 255 || strings.ContainsAny(p.Name, "/\\\x00"):
		return errors.New("invalid worktree name")
	}
	return nil
}

// WorktreeRemoved is the node's outcome of a space.worktree.remove command.
type WorktreeRemoved struct {
	CommandID string `json:"command_id"`
	SpaceID   string `json:"space_id"`
	Repo      string `json:"repo" doc:"The repository folder, relative to the space folder"`
	Name      string `json:"name"`
	Result    string `json:"result" doc:"removed, refused or error"`
	// Code is the refusal code of a refused removal (invalid, outside, not_worktree, locked, no_remote, fetch_failed,
	// live_run, dirty, untracked, unpushed, git_error); empty otherwise.
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Path, Branch and Head describe what was (or would have been) removed, as far as the node got: the path relative
	// to the space folder, the kept branch and the checked-out commit.
	Path      string    `json:"path,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	Head      string    `json:"head,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// Validate checks a report before it is recorded.
func (r WorktreeRemoved) Validate() error {
	switch {
	case !reCmd.MatchString(r.CommandID):
		return errors.New("invalid command_id")
	case !reSpace.MatchString(r.SpaceID):
		return errors.New("invalid space_id")
	case r.Result != WorktreeRemovedOK && r.Result != WorktreeRefused && r.Result != WorktreeFailed:
		return errors.New("invalid result")
	case r.Result == WorktreeRefused && r.Code == "":
		return errors.New("a refusal needs its code")
	}
	return nil
}
