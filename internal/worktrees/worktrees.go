// Package worktrees is central's side of the worktree lifecycle (ADR-0107 decisions 3 to 5): it joins the node's
// repository report with central's own records (the owning run and task), asks the space's node to remove one worktree
// (space.worktree.remove, protocol minor 16) after checking what central can check, and records what the node answered.
//
// The node's safe-removal path is the only thing that removes a worktree; central never deletes anything and cannot
// force a removal. Central only declines earlier, with the same refusal-code vocabulary, when a live run holds the
// worktree, and passes every code the node answers through unchanged. The service is independent of the agent tools:
// the retention pass (ADR-0107 decision 4) calls Request the same way.
//
// Every request and every answer is an audit event of the target worktree_removal (the command ID), so the record is
// durable without a table of its own. Names, branches and reasons come from the node and from agent-writable
// repositories: they are untrusted data.
package worktrees

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/repos"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Statuses of a removal.
const (
	StatusPending = "pending" // queued; the node has not answered (yet)
	StatusRemoved = proto.WorktreeRemovedOK
	StatusRefused = proto.WorktreeRefused
	StatusError   = proto.WorktreeFailed
)

// CodeLiveRun is the refusal code central answers with when a live run of the space holds the worktree. It is one of
// the node's codes (proto.WorktreeRefusalCodes), so a caller handles one vocabulary.
const CodeLiveRun = "live_run"

// Audit actions and the target type of a removal's events.
const (
	TargetType      = "worktree_removal"
	ActionRequested = "worktree.remove_requested"
	ActionRefused   = "worktree.remove_refused"
	ActionResult    = "worktree.removed"
)

// DefaultWait is how long a caller waits for the node's answer by default; a fetch and a clean check of a big tree
// can take a while, so an unanswered request is reported as pending with its command ID, never as a failure.
const DefaultWait = 60 * time.Second

// Service is the worktree lifecycle service.
type Service struct {
	s     *store.Store
	audit *audit.Writer
	fleet *fleet.Service
	repos *repos.Service
	queue *queue.Service
	clk   clock.Clock
	log   *slog.Logger

	// Wait is how long Remove and the tools wait for a node's answer (default DefaultWait).
	Wait time.Duration

	mu      sync.Mutex
	waiters map[string][]chan struct{} // by command ID
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, fl *fleet.Service, rp *repos.Service, q *queue.Service, clk clock.Clock, log *slog.Logger) *Service {
	if clk == nil {
		clk = clock.System{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{s: s, audit: aw, fleet: fl, repos: rp, queue: q, clk: clk, log: log, waiters: map[string][]chan struct{}{}}
}

// Outcome is one removal as central knows it. Code and Reason are the node's, verbatim, for a refusal (or central's own
// live_run refusal, By "central").
type Outcome struct {
	CommandID string    `json:"command_id,omitempty"`
	Repo      string    `json:"repo" doc:"The repository's folder"`
	Name      string    `json:"name" doc:"The worktree's folder name inside worktrees/"`
	Status    string    `json:"status" enum:"pending,removed,refused,error" doc:"pending: queued, the node has not answered yet (ask again with command_id); removed: gone, the branch is kept; refused: nothing changed, see code; error: the node could not tell, nothing is known to be removed"`
	Code      string    `json:"code,omitempty" doc:"The refusal code, verbatim: invalid, outside, not_worktree, locked, no_remote, fetch_failed, live_run, dirty, untracked, unpushed or git_error"`
	Reason    string    `json:"reason,omitempty" doc:"Why, as the node (or central, for live_run) said it; untrusted text"`
	By        string    `json:"by,omitempty" enum:"node,central" doc:"Who decided: central refuses a worktree a live run holds before asking the node"`
	Branch    string    `json:"branch,omitempty"`
	Head      string    `json:"head,omitempty"`
	Path      string    `json:"path,omitempty"`
	At        time.Time `json:"at,omitempty" doc:"When the node checked, or central refused"`
}

// Request asks the space's node to remove the worktree name (its folder name inside worktrees/, or worktrees/<name>) of
// the configured repository repo (name, ID or folder). It returns at once: a pending outcome with the command ID, or a
// refusal central decided itself (a live run holds the worktree). Errors: not found, a node below protocol minor 16, a
// node that is not connected, a relocating space.
func (svc *Service) Request(ctx context.Context, actor audit.Actor, spaceID, repo, name string) (*Outcome, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "worktrees/")
	if name == "" || len(name) > 255 || strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
		return nil, apperr.New(apperr.CodeInvalid, "name: the worktree's folder name inside worktrees/ (one path element)")
	}
	var out *Outcome
	err := svc.s.W().TxNamed(ctx, "worktrees.request", func(tx *store.Tx) error {
		sp, err := tx.Q.GetSpace(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "space not found")
		} else if err != nil {
			return err
		}
		if sp.DesiredNodeID.Valid {
			return apperr.New(apperr.CodePrecondition, "the space is relocating; worktrees are not removed until the move is done")
		}
		rows, err := tx.Q.ListSpaceRepositories(ctx, spaceID)
		if err != nil {
			return err
		}
		var rep *db.SpaceRepository
		for i := range rows {
			if rows[i].ID == repo || rows[i].Name == repo || rows[i].Path == repo {
				rep = &rows[i]
				break
			}
		}
		if rep == nil {
			return apperr.New(apperr.CodeNotFound, "repository %q is not configured in this space", repo)
		}
		node, err := tx.Q.GetNode(ctx, sp.NodeID)
		if err != nil {
			return err
		}
		if node.ProtocolMinor < proto.MinorWorktreeRemove {
			return apperr.New(apperr.CodePrecondition, "node %s speaks node protocol 1.%d; removing a worktree needs 1.%d (upgrade spaceman-agent)",
				node.Name, node.ProtocolMinor, proto.MinorWorktreeRemove)
		}
		// Only a worktree of the last report can be named: a typo or a folder the node never listed is refused here
		// instead of being sent to the node.
		listed, err := svc.listed(ctx, tx.Q, sp, *rep, name)
		if err != nil {
			return err
		}
		if !listed {
			return apperr.New(apperr.CodeNotFound, "no worktree %q of repository %s in the node's last report (ask for a repository sync, then list again); only folders directly inside worktrees/ can be removed", name, rep.Name)
		}
		base := &Outcome{Repo: rep.Path, Name: name}
		// Central checks the live runs first (the node checks too, and its answer stands): never ask for the removal of a
		// worktree a run holds.
		if holder, err := liveHolder(ctx, tx.Q, spaceID, rep.Path, name); err != nil {
			return err
		} else if holder != "" {
			base.Status, base.Code, base.By, base.At = StatusRefused, CodeLiveRun, "central", tx.Now
			base.Reason = "the live run " + holder + " works in this worktree; stop it or wait until it ends"
			out = base
			_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: ActionRefused, Target: audit.Target{Type: "worktree", ID: spaceID + "/" + rep.Path + "/" + name},
				Outcome: audit.OutcomeDenied, Details: map[string]any{"space_id": spaceID, "repo": rep.Path, "name": name, "code": CodeLiveRun, "run_id": holder}})
			return err
		}
		p := proto.WorktreeRemovePayload{Repository: repos.Source(*rep), Name: name}
		if err := p.Validate(); err != nil {
			return apperr.New(apperr.CodeInvalid, "%s", err.Error())
		}
		id, err := svc.fleet.QueueSpaceCommand(ctx, tx, spaceID, proto.CmdWorktreeRemove, "worktree-remove:"+ids.New(ids.Operation), p.Redacted())
		if err != nil {
			return err
		}
		base.CommandID, base.Status = id, StatusPending
		out = base
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: ActionRequested, Target: audit.Target{Type: TargetType, ID: id},
			Details: map[string]any{"space_id": spaceID, "repo": rep.Path, "name": name}})
		return err
	})
	if err != nil {
		return nil, err
	}
	if out.CommandID != "" {
		svc.fleet.KickSpace(ctx, spaceID)
	}
	return out, nil
}

// Remove is Request, then waits up to wait (the service's Wait, by default DefaultWait, when zero or negative) for the node's answer. An unanswered
// request comes back pending with its command ID; ask again with Result.
func (svc *Service) Remove(ctx context.Context, actor audit.Actor, spaceID, repo, name string, wait time.Duration) (*Outcome, error) {
	o, err := svc.Request(ctx, actor, spaceID, repo, name)
	if err != nil || o.Status != StatusPending {
		return o, err
	}
	return svc.Await(ctx, spaceID, o, wait)
}

// Await waits up to wait for the answer to a pending outcome and returns the outcome as it stands.
func (svc *Service) Await(ctx context.Context, spaceID string, o *Outcome, wait time.Duration) (*Outcome, error) {
	if o.Status != StatusPending {
		return o, nil
	}
	if wait <= 0 {
		wait = svc.wait()
	}
	ch := svc.subscribe(o.CommandID)
	defer svc.unsubscribe(o.CommandID, ch)
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		// Look first: the answer may have been stored before the subscription.
		if r, err := svc.Result(ctx, spaceID, o.CommandID); err != nil {
			return nil, err
		} else if r.Status != StatusPending {
			return r, nil
		}
		select {
		case <-ch:
		case <-t.C:
			return o, nil
		case <-ctx.Done():
			return o, nil
		}
	}
}

// Result returns the outcome of an earlier removal request of the space: pending until the node's answer is stored.
func (svc *Service) Result(ctx context.Context, spaceID, commandID string) (*Outcome, error) {
	req, err := svc.record(ctx, commandID, ActionRequested)
	if err != nil {
		return nil, err
	}
	if req == nil || fmt.Sprint(req.Details["space_id"]) != spaceID {
		return nil, apperr.New(apperr.CodeNotFound, "no worktree removal %s in this space", commandID)
	}
	o := &Outcome{CommandID: commandID, Repo: fmt.Sprint(req.Details["repo"]), Name: fmt.Sprint(req.Details["name"]), Status: StatusPending}
	ans, err := svc.record(ctx, commandID, ActionResult)
	if err != nil || ans == nil {
		return o, err
	}
	d := ans.Details
	str := func(k string) string { s, _ := d[k].(string); return s }
	o.Status, o.Code, o.Reason, o.By = str("result"), str("code"), str("reason"), "node"
	o.Branch, o.Head, o.Path = str("branch"), str("head"), str("path")
	o.At = time.UnixMicro(ans.At).UTC()
	return o, nil
}

// record returns the audit event of the action for a removal command, nil when there is none.
func (svc *Service) record(ctx context.Context, commandID, action string) (*audit.Record, error) {
	var (
		at      int64
		details string
	)
	err := svc.s.ReadDB().QueryRowContext(ctx, `SELECT at, details_json FROM audit_events WHERE target_type = ? AND target_id = ? AND action = ? ORDER BY seq LIMIT 1`,
		TargetType, commandID, action).Scan(&at, &details)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	r := &audit.Record{At: at, Details: map[string]any{}}
	if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
		return nil, err
	}
	return r, nil
}

// OnRemoved stores a node's answer to a removal (the hub's worktree.removed hook). Only the node the command was queued
// for, for the command's space, is heard; the first answer for a command is kept; a removed worktree makes central ask
// for a fresh repository report so the listing stops showing it.
func (svc *Service) OnRemoved(ctx context.Context, nodeID string, rep proto.WorktreeRemoved) {
	stored := false
	err := svc.s.W().TxNamed(ctx, "worktrees.removed", func(tx *store.Tx) error {
		var kind, node, space string
		if err := tx.SQL.QueryRowContext(ctx, `SELECT kind, node_id, space_id FROM node_commands WHERE id = ?`, rep.CommandID).Scan(&kind, &node, &space); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if kind != proto.CmdWorktreeRemove || node != nodeID || space != rep.SpaceID {
			return nil // not a removal of this node's space: not heard
		}
		var n int
		if err := tx.SQL.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE target_type = ? AND target_id = ? AND action = ?`, TargetType, rep.CommandID, ActionResult).Scan(&n); err != nil {
			return err
		} else if n > 0 {
			return nil // the first answer is kept
		}
		outcome := audit.OutcomeSuccess
		switch rep.Result {
		case proto.WorktreeRefused:
			outcome = audit.OutcomeDenied
		case proto.WorktreeFailed:
			outcome = audit.OutcomeFailure
		}
		reason := rep.Reason
		if len(reason) > proto.WorktreeRemoveMaxReason {
			reason = reason[:proto.WorktreeRemoveMaxReason]
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorNode, ID: nodeID}, Action: ActionResult,
			Target: audit.Target{Type: TargetType, ID: rep.CommandID}, Outcome: outcome,
			Details: map[string]any{"space_id": rep.SpaceID, "repo": rep.Repo, "name": rep.Name, "result": rep.Result, "code": rep.Code, "reason": reason,
				"path": rep.Path, "branch": rep.Branch, "head": rep.Head}})
		stored = err == nil
		return err
	})
	if err != nil {
		svc.log.Warn("store worktree removal result", slog.String(obs.FieldSpaceID, rep.SpaceID), obs.Err(err))
		return
	}
	if !stored {
		return
	}
	svc.wake(rep.CommandID)
	if rep.Result == proto.WorktreeRemovedOK {
		no := false
		if _, err := svc.repos.Sync(ctx, audit.System, rep.SpaceID, repos.SyncInput{Fetch: &no}); err != nil {
			svc.log.Info("refresh repository report after a worktree removal", slog.String(obs.FieldSpaceID, rep.SpaceID), obs.Err(err))
		}
	}
}

func (svc *Service) wait() time.Duration {
	if svc.Wait > 0 {
		return svc.Wait
	}
	return DefaultWait
}

func (svc *Service) subscribe(id string) chan struct{} {
	ch := make(chan struct{}, 1)
	svc.mu.Lock()
	svc.waiters[id] = append(svc.waiters[id], ch)
	svc.mu.Unlock()
	return ch
}

func (svc *Service) unsubscribe(id string, ch chan struct{}) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	l := svc.waiters[id]
	for i, c := range l {
		if c == ch {
			l = append(l[:i], l[i+1:]...)
			break
		}
	}
	if len(l) == 0 {
		delete(svc.waiters, id)
	} else {
		svc.waiters[id] = l
	}
}

func (svc *Service) wake(id string) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	for _, c := range svc.waiters[id] {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// listed reports whether the repository's last report (of the space's current node) lists worktrees/<name>.
func (svc *Service) listed(ctx context.Context, q *db.Queries, sp db.Space, rep db.SpaceRepository, name string) (bool, error) {
	r, err := q.GetRepositoryReport(ctx, sp.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if !r.ReportedAt.Valid || r.NodeID != sp.NodeID {
		return false, nil
	}
	var report proto.RepoStatusReport
	_ = json.Unmarshal([]byte(r.ReportJson), &report) // an unreadable report lists nothing
	for _, st := range report.Repositories {
		if st.ID != rep.ID || st.Check == nil {
			continue
		}
		for _, w := range st.Check.Worktrees {
			if w.Path == "worktrees/"+name {
				return true, nil
			}
		}
	}
	return false, nil
}

// folderOf is the worktree folder name a run's launch gets (the node's rule: the branch with / replaced by -).
func folderOf(branch string) string { return strings.ReplaceAll(branch, "/", "-") }

// liveHolder returns the ID of a live (or launching) run of the space whose worktree is repoPath's worktrees/<name>,
// "" when none is. A live run is one the node has not reported ended.
func liveHolder(ctx context.Context, q *db.Queries, spaceID, repoPath, name string) (string, error) {
	runs, err := q.ListActiveRunsBySpace(ctx, spaceID)
	if err != nil {
		return "", err
	}
	for _, r := range runs {
		var l fleet.LaunchSpec
		if json.Unmarshal([]byte(r.LaunchJson), &l) != nil || l.Worktree == nil {
			continue
		}
		if path.Clean(l.Worktree.Repo) == path.Clean(repoPath) && folderOf(l.Worktree.Branch) == name {
			return r.ID, nil
		}
	}
	return "", nil
}
