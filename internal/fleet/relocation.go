package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Space relocation (SM-INV-003, SM-SPACE-005…008, ADR-0037). A relocation is always an explicit operator request; nothing
// in SpaceMan starts one on its own. It is a resumable state machine stored in `relocations`:
//
//	clean:              draining → checking → [blocked] → [exporting] → preparing_destination → finalizing → done
//	source_unavailable: preparing_destination → finalizing → done (central's copies only, acknowledged loss)
//	precheck:           checking → checked (a dry run: nothing changes)
//
// with failed and cancelled as the other ends. While it runs, spaces.desired_node_id holds the destination and
// spaces.node_id the effective assignment (SM-SPACE-006); launches are refused and the source's lease is revoked. The
// finalizing transaction moves the assignment and the lease to the destination with a higher generation, which fences
// everything the source might still do. The space stays paused at the destination; the source's folder is retained or
// deleted per the recovery policy.

// Relocation modes and states.
const (
	RelocClean             = "clean"
	RelocSourceUnavailable = "source_unavailable"
	RelocPrecheck          = "precheck"

	RelocRequested = "requested"
	RelocDraining  = "draining"
	RelocChecking  = "checking"
	RelocBlocked   = "blocked"
	RelocExporting = "exporting"
	RelocPreparing = "preparing_destination"
	RelocFinal     = "finalizing"
	RelocDone      = "done"
	RelocChecked   = "checked"
	RelocFailed    = "failed"
	RelocCancelled = "cancelled"
)

func relocEnded(state string) bool {
	switch state {
	case RelocDone, RelocChecked, RelocFailed, RelocCancelled:
		return true
	}
	return false
}

// Relocation is one relocation of a space, as the API shows it.
type Relocation struct {
	ID                string                `json:"id"`
	SpaceID           string                `json:"space_id"`
	SourceNodeID      string                `json:"source_node_id"`
	DestinationNodeID string                `json:"destination_node_id"`
	Mode              string                `json:"mode" enum:"clean,source_unavailable,precheck"`
	State             string                `json:"state" enum:"requested,draining,checking,blocked,exporting,preparing_destination,finalizing,done,checked,failed,cancelled"`
	BlockedStep       string                `json:"blocked_step,omitempty" enum:",check,export,prepare" doc:"Where a retry resumes"`
	DirtyState        string                `json:"dirty_state" enum:"refuse,export"`
	StopRuns          bool                  `json:"stop_runs"`
	Recovery          Recovery              `json:"recovery"`
	Acknowledgement   string                `json:"acknowledgement,omitempty" doc:"The operator's acknowledgement of possibly lost local state (source_unavailable)"`
	Repositories      []RepoSpec            `json:"repositories,omitempty" doc:"Repositories cloned on the destination of a source_unavailable relocation: the operator's list and the configured repositories with a URL"`
	Report            *proto.SpaceCheck     `json:"report,omitempty" doc:"The source's last inspection"`
	Issues            []Issue               `json:"issues" doc:"What the inspection found that a clean relocation would lose"`
	Loss              *LossReport           `json:"loss,omitempty" doc:"What may be lost when the source is unavailable"`
	Export            *proto.RelocateExport `json:"export,omitempty" doc:"State exported from the source and stored by central"`
	Prepared          *proto.PrepareResult  `json:"prepared,omitempty" doc:"What the destination materialized and verified"`
	Continuity        Continuity            `json:"continuity"`
	GenerationBefore  int64                 `json:"generation_before"`
	GenerationAfter   int64                 `json:"generation_after,omitempty"`
	PausedSpace       bool                  `json:"paused_space" doc:"The relocation paused the space"`
	Error             string                `json:"error,omitempty"`
	SourceState       string                `json:"source_state,omitempty" enum:",pending,retained,deleted,unreachable" doc:"What happened to the source folder"`
	RetainUntil       *time.Time            `json:"retain_until,omitempty"`
	Version           int64                 `json:"version"`
	CreatedAt         time.Time             `json:"created_at"`
	CreatedBy         string                `json:"created_by"`
	UpdatedAt         time.Time             `json:"updated_at"`
	FinishedAt        *time.Time            `json:"finished_at,omitempty"`
}

// Recovery is what happens to the source's folder after a relocation.
type Recovery struct {
	Mode       string `json:"mode" enum:"retain,delete" doc:"retain (default) moves the folder aside on the source; delete removes it"`
	RetainDays int    `json:"retain_days" doc:"Days a retained folder is kept; 0 keeps it until it is released"`
}

// RepoSpec is a repository the operator names for a source_unavailable relocation, besides the space's configured
// repositories with a URL (ADR-0044).
type RepoSpec struct {
	Path   string `json:"path" doc:"Folder relative to the space folder"`
	URL    string `json:"url" doc:"Clone URL; the destination node's own Git authentication is used (no credentials in the URL)"`
	Branch string `json:"branch,omitempty"`
}

// Issue is one thing that would be lost by a clean relocation, or that stops it.
type Issue struct {
	Code       string `json:"code" enum:"dirty,untracked,unpushed,stash,detached,no_remote,worktree,loose_files,memory_unsynced,recordings_pending,transcripts_pending,active_runs,inspect_error"`
	Path       string `json:"path,omitempty"`
	Detail     string `json:"detail"`
	Exportable bool   `json:"exportable" doc:"An explicit export carries it; otherwise it blocks until resolved"`
}

// LossReport is what a source_unavailable relocation may lose, from what central knows.
type LossReport struct {
	Statement      string       `json:"statement"`
	SourceLastSeen *time.Time   `json:"source_last_seen,omitempty"`
	Memory         []MemoryLoss `json:"memory"`
	Runs           []string     `json:"runs" doc:"Runs on the source that are marked lost"`
	LastCheckAt    *time.Time   `json:"last_check_at,omitempty" doc:"When the source was last inspected for a relocation"`
	LastIssues     []Issue      `json:"last_issues,omitempty" doc:"What that inspection found"`
	Unknown        []string     `json:"unknown" doc:"Local state central cannot see and that may be lost"`
}

// MemoryLoss is a collection's last node report against central's acknowledged head.
type MemoryLoss struct {
	Role           string     `json:"role"`
	CentralHead    string     `json:"central_head,omitempty"`
	LocalCommits   int        `json:"local_commits"`
	Uncheckpointed int        `json:"uncheckpointed"`
	Pending        bool       `json:"pending"`
	ReportedAt     *time.Time `json:"reported_at,omitempty"`
}

// Continuity states what carries over (SM-SPACE-008).
type Continuity struct {
	Project        string `json:"project" enum:"guaranteed"`
	NativeSessions string `json:"native_sessions" enum:"not_carried"`
	Label          string `json:"label"`
	Detail         string `json:"detail"`
}

var continuity = Continuity{Project: "guaranteed", NativeSessions: "not_carried", Label: "Fresh session, project continuity only",
	Detail: "Repositories, role memory, the inbox, queue, human actions, messages and run history move with the space. Claude Code and " +
		"Codex keep their native sessions in node-local instance folders, which a relocation does not carry: runs on the destination " +
		"start fresh native sessions, and earlier sessions cannot be resumed there."}

// StartRelocationInput asks for a relocation.
type StartRelocationInput struct {
	DestinationNodeID string     `json:"destination_node_id" doc:"The node the space moves to"`
	DirtyState        string     `json:"dirty_state,omitempty" enum:"refuse,export" doc:"refuse (default) stops at unpushed, changed or untracked state; export carries it through central"`
	StopRuns          bool       `json:"stop_runs,omitempty" doc:"Pause the space (interrupt) and stop its runs; without it the space must be paused with no active runs"`
	Recovery          string     `json:"recovery,omitempty" enum:"retain,delete" doc:"What happens to the source folder (default retain)"`
	RetainDays        int        `json:"retain_days,omitempty" minimum:"0" maximum:"3650" doc:"With retain: days to keep it; 0 keeps it until released"`
	Precheck          bool       `json:"precheck,omitempty" doc:"Only inspect the source and report; nothing changes"`
	SourceUnavailable bool       `json:"source_unavailable,omitempty" doc:"Relocate from central's copies only, because the source is offline, disabled or revoked"`
	AcknowledgeLoss   bool       `json:"acknowledge_loss,omitempty" doc:"Required with source_unavailable: local state on the source may be lost"`
	Acknowledgement   string     `json:"acknowledgement,omitempty" maxLength:"500" doc:"Required with source_unavailable: the operator's own words, recorded in the audit log"`
	Repositories      []RepoSpec `json:"repositories,omitempty" maxItems:"50" doc:"source_unavailable only: repositories to clone on the destination, besides the configured ones with a URL (a listed folder overrides the configured repository there)"`
}

// RetryRelocationInput resumes a blocked relocation.
type RetryRelocationInput struct {
	DirtyState string `json:"dirty_state,omitempty" enum:"refuse,export" doc:"Change the policy before retrying (export carries the reported state)"`
}

var errNoRelocation = apperr.New(apperr.CodeNotFound, "the space has no relocation")

func relocationOut(r db.Relocation) *Relocation {
	o := &Relocation{ID: r.ID, SpaceID: r.SpaceID, SourceNodeID: r.SourceNodeID, DestinationNodeID: r.DestNodeID, Mode: r.Mode, State: r.State,
		BlockedStep: r.BlockedStep, DirtyState: r.DirtyPolicy, StopRuns: r.StopRuns == 1, Recovery: Recovery{Mode: r.Recovery, RetainDays: int(r.RetainDays)},
		Acknowledgement: r.Acknowledgement, Continuity: continuity, GenerationBefore: r.GenerationBefore, GenerationAfter: r.GenerationAfter,
		PausedSpace: r.PausedSpace == 1, Error: r.Error, SourceState: r.SourceState, RetainUntil: microsPtr(r.RetainUntil), Version: r.Version,
		CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), CreatedBy: r.CreatedBy, UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(),
		FinishedAt: microsPtr(r.FinishedAt), Issues: []Issue{}}
	_ = json.Unmarshal([]byte(r.ReposJson), &o.Repositories)
	_ = json.Unmarshal([]byte(r.IssuesJson), &o.Issues)
	if r.ReportJson != "" {
		o.Report = &proto.SpaceCheck{}
		_ = json.Unmarshal([]byte(r.ReportJson), o.Report)
	}
	if r.LossJson != "" {
		o.Loss = &LossReport{}
		_ = json.Unmarshal([]byte(r.LossJson), o.Loss)
	}
	if r.ExportJson != "" {
		o.Export = &proto.RelocateExport{}
		_ = json.Unmarshal([]byte(r.ExportJson), o.Export)
	}
	if r.PrepareJson != "" {
		o.Prepared = &proto.PrepareResult{}
		_ = json.Unmarshal([]byte(r.PrepareJson), o.Prepared)
	}
	if o.Issues == nil {
		o.Issues = []Issue{}
	}
	return o
}

// GetRelocation returns the space's relocation in progress, or its latest one.
func (svc *Service) GetRelocation(ctx context.Context, spaceID string) (*Relocation, error) {
	r, err := svc.s.R().GetActiveRelocation(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		r, err = svc.s.R().GetLatestRelocation(ctx, spaceID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoRelocation
	} else if err != nil {
		return nil, err
	}
	return relocationOut(r), nil
}

// ListRelocations returns a space's relocations, newest first.
func (svc *Service) ListRelocations(ctx context.Context, spaceID string, limit int) ([]*Relocation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := svc.s.R().ListRelocationsBySpace(ctx, db.ListRelocationsBySpaceParams{SpaceID: spaceID, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]*Relocation, 0, len(rows))
	for _, r := range rows {
		out = append(out, relocationOut(r))
	}
	return out, nil
}

func validRepoSpec(r RepoSpec) string {
	p := strings.TrimSpace(r.Path)
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "..") || path.Clean(p) != p || p == "instances" || strings.HasPrefix(p, "instances/") ||
		p == "worktrees" || strings.HasPrefix(p, "worktrees/") {
		return "path must be a relative folder inside the space (not instances/ or worktrees/)"
	}
	u, err := url.Parse(r.URL)
	switch {
	case r.URL == "" || len(r.URL) > 2000:
		return "url is required"
	case err == nil && u.User != nil:
		return "url must not contain credentials; the destination node's own Git authentication is used"
	case strings.ContainsAny(r.URL, " \n\r\t"):
		return "url must not contain whitespace"
	}
	// The repository URL rules (ADR-0044); local URLs stay allowed here, as relocation always allowed them.
	if err := proto.ValidateRepoURL(r.URL, true); err != nil {
		return "url " + err.Error()
	}
	if r.Branch != "" && (!reBranch.MatchString(r.Branch) || strings.Contains(r.Branch, "..")) {
		return "branch must be a plain branch name"
	}
	return ""
}

// StartRelocation starts an explicit relocation (or a precheck) of a space to another node. It never happens without
// such a request (SM-INV-003). A clean relocation needs the source connected and either a paused space without active
// runs or stop_runs; a source_unavailable one needs the source offline, disabled or revoked and an acknowledgement of
// possibly lost local state (SM-SPACE-007).
func (svc *Service) StartRelocation(ctx context.Context, actor audit.Actor, spaceID string, in StartRelocationInput) (*Relocation, error) {
	mode := RelocClean
	switch {
	case in.Precheck && in.SourceUnavailable:
		return nil, apperr.New(apperr.CodeInvalid, "precheck inspects the source; it cannot be combined with source_unavailable")
	case in.Precheck:
		mode = RelocPrecheck
	case in.SourceUnavailable:
		mode = RelocSourceUnavailable
	}
	v := &apperr.Validation{}
	if in.DirtyState == "" {
		in.DirtyState = "refuse"
	}
	if in.DirtyState != "refuse" && in.DirtyState != "export" {
		v.Add("dirty_state", "must be refuse or export")
	}
	if in.Recovery == "" {
		in.Recovery = "retain"
	}
	if in.Recovery != "retain" && in.Recovery != "delete" {
		v.Add("recovery", "must be retain or delete")
	}
	if in.RetainDays < 0 || in.RetainDays > 3650 || (in.Recovery == "delete" && in.RetainDays != 0) {
		v.Add("retain_days", "must be 0-3650, and 0 with recovery delete")
	}
	in.Acknowledgement = strings.TrimSpace(in.Acknowledgement)
	if mode == RelocSourceUnavailable {
		if !in.AcknowledgeLoss {
			v.Add("acknowledge_loss", "must be true: local state on the unavailable source may be lost")
		}
		if len(in.Acknowledgement) < 10 || len(in.Acknowledgement) > 500 {
			v.Add("acknowledgement", "is required (10-500 characters) and is recorded in the audit log")
		}
	} else if in.AcknowledgeLoss || in.Acknowledgement != "" || len(in.Repositories) > 0 {
		v.Add("source_unavailable", "acknowledge_loss, acknowledgement and repositories apply only to a source_unavailable relocation")
	}
	seen := map[string]bool{}
	for i, r := range in.Repositories {
		if msg := validRepoSpec(r); msg != "" {
			v.Add("repositories["+itoa(i)+"]", "%s", msg)
		}
		if seen[r.Path] {
			v.Add("repositories["+itoa(i)+"]", "duplicate path")
		}
		seen[r.Path] = true
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	id := ids.New(ids.Relocation)
	var nodes []string
	err := svc.s.W().TxNamed(ctx, "fleet.relocation_start", func(tx *store.Tx) error {
		sp, err := tx.Q.GetSpace(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return errSpaceNotFound
		} else if err != nil {
			return err
		}
		src := sp.NodeID
		nodes = []string{src, in.DestinationNodeID}
		if err := svc.relocationPreconditions(ctx, tx, sp, mode, in); err != nil {
			return err
		}
		lease, err := tx.Q.GetLease(ctx, spaceID)
		gen := int64(0)
		if err == nil {
			gen = lease.Generation
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		active, err := tx.Q.ListActiveRunsBySpace(ctx, spaceID)
		if err != nil {
			return err
		}
		paused := false
		state := RelocDraining
		var loss string
		switch mode {
		case RelocPrecheck:
			state = RelocChecking
		case RelocClean, RelocSourceUnavailable:
			if sp.State == "active" || len(active) > 0 {
				// relocationPreconditions checked stop_runs: pause with interrupt and stop the active runs.
				for _, r := range active {
					if mode == RelocClean {
						if err := svc.stopRunTx(ctx, tx, r, "interrupt", "space relocating"); err != nil {
							return err
						}
					}
				}
				if sp.State == "active" {
					if _, err := tx.Q.SetSpaceState(ctx, db.SetSpaceStateParams{State: "paused", PauseMode: PauseInterrupt, Now: tx.NowMicros(), ID: spaceID,
						Version: sp.Version}); err != nil {
						return err
					}
					paused = true
				}
			}
			if _, err := tx.Q.SetSpaceDesiredNode(ctx, db.SetSpaceDesiredNodeParams{DesiredNodeID: sql.NullString{String: in.DestinationNodeID, Valid: true},
				Now: tx.NowMicros(), ID: spaceID}); err != nil {
				return err
			}
			if mode == RelocSourceUnavailable {
				state = RelocPreparing
				l, err := svc.lossReport(ctx, tx, sp, active)
				if err != nil {
					return err
				}
				b, _ := json.Marshal(l)
				loss = string(b)
				if _, err := tx.Q.RevokeLease(ctx, db.RevokeLeaseParams{Now: tx.NowMicros(), SpaceID: spaceID}); err != nil {
					return err
				}
			}
		}
		if mode == RelocSourceUnavailable {
			// The space's configured repositories with a URL are cloned on the destination too (ADR-0044); a folder the
			// operator listed keeps the operator's URL and branch.
			listed := map[string]bool{}
			for _, r := range in.Repositories {
				listed[r.Path] = true
			}
			configured, err := tx.Q.ListSpaceRepositories(ctx, spaceID)
			if err != nil {
				return err
			}
			for _, r := range configured {
				if r.Url != "" && !listed[r.Path] {
					in.Repositories = append(in.Repositories, RepoSpec{Path: r.Path, URL: r.Url, Branch: r.DefaultBranch})
				}
			}
		}
		repos, _ := json.Marshal(nonNilRepos(in.Repositories))
		if err := tx.Q.InsertRelocation(ctx, db.InsertRelocationParams{ID: id, SpaceID: spaceID, SourceNodeID: src, DestNodeID: in.DestinationNodeID,
			Mode: mode, State: state, DirtyPolicy: in.DirtyState, StopRuns: b2i(in.StopRuns), Recovery: in.Recovery, RetainDays: int64(in.RetainDays),
			Acknowledgement: in.Acknowledgement, ReposJson: string(repos), LossJson: loss, GenerationBefore: gen, PausedSpace: b2i(paused),
			CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor), UpdatedAt: tx.NowMicros()}); err != nil {
			if store.IsConstraint(err) {
				return apperr.New(apperr.CodeConflict, "the space already has a relocation in progress")
			}
			return err
		}
		if mode == RelocPrecheck {
			cmd, err := svc.queueRelocationCommand(ctx, tx, src, spaceID, gen, id, proto.CmdRelocateCheck,
				proto.RelocateCheckPayload{RelocationID: id, Workdir: sp.Workdir, Flush: true, SyncWait: svc.syncWaitSeconds()})
			if err != nil {
				return err
			}
			if _, err := tx.Q.UpdateRelocationStep(ctx, db.UpdateRelocationStepParams{State: state, StepCommandID: cmd, Now: tx.NowMicros(), ID: id, Version: 1}); err != nil {
				return err
			}
		}
		if err := svc.record(ctx, tx, spaceID, "space", spaceID, "updated", sp.Version+1); err != nil {
			return err
		}
		if err := svc.record(ctx, tx, spaceID, "relocation", id, "created", 1); err != nil {
			return err
		}
		ev := audit.Event{Actor: actor, Action: "relocation.requested", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"relocation_id": id, "mode": mode, "from": src, "to": in.DestinationNodeID, "dirty_state": in.DirtyState,
				"stop_runs": in.StopRuns, "stopped_runs": len(active), "recovery": in.Recovery, "retain_days": in.RetainDays, "generation": gen}}
		if mode == RelocSourceUnavailable {
			ev.Action = "relocation.loss_acknowledged"
			ev.Details["acknowledged_loss"] = true
			ev.Sensitive = map[string]any{"acknowledgement": in.Acknowledgement} // the operator's own words, sealed like other free text
			ev.Details["repositories"] = len(in.Repositories)
		}
		_, err = svc.audit.Append(ctx, tx, ev)
		return err
	})
	if err != nil {
		svc.auditDenied(ctx, actor, spaceID, mode, in.DestinationNodeID, err)
		return nil, err
	}
	for _, n := range nodes {
		svc.hub.Kick(n)
	}
	svc.relocKick(ctx, id)
	return svc.relocationByID(ctx, id)
}

// auditDenied records a refused relocation request (precondition, conflict or validation), outside the failed transaction.
func (svc *Service) auditDenied(ctx context.Context, actor audit.Actor, spaceID, mode, dest string, cause error) {
	code := apperr.CodeOf(cause)
	if code == "" || code == apperr.CodeNotFound {
		return
	}
	_ = svc.s.W().TxNamed(ctx, "fleet.relocation_denied", func(tx *store.Tx) error {
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "relocation.requested", Outcome: audit.OutcomeDenied,
			Target: audit.Target{Type: "space", ID: spaceID}, Details: map[string]any{"mode": mode, "to": dest, "reason": truncate(cause.Error(), 300)}})
		return err
	})
}

func (svc *Service) relocationPreconditions(ctx context.Context, tx *store.Tx, sp db.Space, mode string, in StartRelocationInput) error {
	if sp.State == "archived" {
		return apperr.New(apperr.CodePrecondition, "the space is archived; restore it before relocating")
	}
	if in.DestinationNodeID == sp.NodeID {
		return apperr.New(apperr.CodeInvalid, "destination_node_id: the space is already pinned to that node")
	}
	dest, err := tx.Q.GetNode(ctx, in.DestinationNodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return apperr.New(apperr.CodeInvalid, "destination_node_id: no such node")
	} else if err != nil {
		return err
	}
	switch {
	case dest.State != "active":
		return apperr.New(apperr.CodePrecondition, "the destination node %s is %s; only an enrolled, enabled node can receive a space", dest.Name, dest.State)
	case dest.ProtocolMinor < proto.MinorRelocation:
		return apperr.New(apperr.CodePrecondition, "the destination node %s last spoke node protocol 1.%d; relocation needs 1.%d (upgrade its spaceman-agent and let it connect)",
			dest.Name, dest.ProtocolMinor, proto.MinorRelocation)
	}
	src, err := tx.Q.GetNode(ctx, sp.NodeID)
	if err != nil {
		return err
	}
	connected := svc.hub.Connected(src.ID) && src.State == "active"
	switch mode {
	case RelocClean, RelocPrecheck:
		if !connected {
			return apperr.New(apperr.CodePrecondition, "the source node %s is not connected; a clean relocation inspects it first. Wait for it to reconnect, "+
				"or relocate with source_unavailable and an acknowledgement that its local state may be lost", src.Name)
		}
		if src.ProtocolMinor < proto.MinorRelocation {
			return apperr.New(apperr.CodePrecondition, "the source node %s speaks node protocol 1.%d; relocation needs 1.%d (upgrade its spaceman-agent)",
				src.Name, src.ProtocolMinor, proto.MinorRelocation)
		}
	case RelocSourceUnavailable:
		if connected {
			return apperr.New(apperr.CodePrecondition, "the source node %s is connected; relocate it cleanly instead (its state is checked and carried)", src.Name)
		}
	}
	if mode == RelocPrecheck {
		return nil
	}
	active, err := tx.Q.ListActiveRunsBySpace(ctx, sp.ID)
	if err != nil {
		return err
	}
	if (sp.State == "active" || len(active) > 0) && !in.StopRuns {
		return apperr.New(apperr.CodePrecondition, "only a stopped space is relocated: pause it and let its %d active run(s) end, or set stop_runs "+
			"to pause it (interrupt) and stop them", len(active))
	}
	return nil
}

func (svc *Service) lossReport(ctx context.Context, tx *store.Tx, sp db.Space, active []db.Run) (*LossReport, error) {
	l := &LossReport{Statement: "The source node is unavailable. The destination is prepared only from central's copies: role memory as central " +
		"acknowledged it and the repositories listed with the request. Everything that existed only on the source may be lost.",
		Memory: []MemoryLoss{}, Runs: []string{},
		Unknown: []string{
			"Uncommitted, untracked, stashed and unpushed work in the space's repositories and worktrees",
			"Files in the space folder outside repositories",
			"Role memory the source had not synced to central (last report below)",
			"Recording output and native transcripts the source had not uploaded",
			"Native harness sessions (always node-local)",
		}}
	if n, err := tx.Q.GetNode(ctx, sp.NodeID); err == nil {
		l.SourceLastSeen = microsPtr(n.LastSeenAt)
	}
	cols, err := tx.Q.ListMemoryCollections(ctx, sp.ID)
	if err != nil {
		return nil, err
	}
	for _, c := range cols {
		m := MemoryLoss{Role: c.Role, CentralHead: c.Head, ReportedAt: microsPtr(c.StatusAt)}
		var st proto.MemoryCollectionStatus
		if json.Unmarshal([]byte(c.NodeStatusJson), &st) == nil {
			m.LocalCommits, m.Uncheckpointed, m.Pending = st.LocalCommits, st.Uncheckpointed, st.Pending
		}
		l.Memory = append(l.Memory, m)
	}
	for _, r := range active {
		l.Runs = append(l.Runs, r.ID)
	}
	if last, err := tx.Q.GetLatestRelocation(ctx, sp.ID); err == nil && last.ReportJson != "" {
		var c proto.SpaceCheck
		if json.Unmarshal([]byte(last.ReportJson), &c) == nil {
			at := c.CheckedAt
			l.LastCheckAt = &at
			l.LastIssues = checkIssues(&c)
		}
	}
	return l, nil
}

func nonNilRepos(r []RepoSpec) []RepoSpec {
	if r == nil {
		return []RepoSpec{}
	}
	return r
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func (svc *Service) relocationByID(ctx context.Context, id string) (*Relocation, error) {
	r, err := svc.s.R().GetRelocation(ctx, id)
	if err != nil {
		return nil, err
	}
	return relocationOut(r), nil
}

func (svc *Service) syncWaitSeconds() int {
	if svc.opts.RelocationSyncWait > 0 {
		return int(svc.opts.RelocationSyncWait / time.Second)
	}
	return 60
}

// queueRelocationCommand queues one relocation step for a node and returns its command ID. The generation is the space's
// lease epoch at the start (at least 1): relocation steps are not execution, and the node accepts them without a lease.
func (svc *Service) queueRelocationCommand(ctx context.Context, tx *store.Tx, nodeID, spaceID string, gen int64, relocID, kind string, payload any) (string, error) {
	return svc.queueCommandID(ctx, tx, nodeID, spaceID, "", max(gen, 1), kind, kind+":"+relocID+":"+ids.New(ids.Message), payload)
}

// RetryRelocation resumes a blocked relocation at the step that stopped it, optionally with a new dirty-state policy.
func (svc *Service) RetryRelocation(ctx context.Context, actor audit.Actor, spaceID string, in RetryRelocationInput) (*Relocation, error) {
	if in.DirtyState != "" && in.DirtyState != "refuse" && in.DirtyState != "export" {
		return nil, apperr.New(apperr.CodeInvalid, "dirty_state must be refuse or export")
	}
	var id string
	var kick []string
	err := svc.s.W().TxNamed(ctx, "fleet.relocation_retry", func(tx *store.Tx) error {
		r, err := tx.Q.GetActiveRelocation(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodePrecondition, "the space has no relocation in progress")
		} else if err != nil {
			return err
		}
		id = r.ID
		if r.State != RelocBlocked {
			return apperr.New(apperr.CodePrecondition, "the relocation is %s; only a blocked relocation can be retried", r.State)
		}
		if in.DirtyState != "" && in.DirtyState != r.DirtyPolicy {
			if err := tx.Q.SetRelocationPolicy(ctx, db.SetRelocationPolicyParams{DirtyPolicy: in.DirtyState, ID: r.ID}); err != nil {
				return err
			}
			r.DirtyPolicy = in.DirtyState
		}
		sp, err := tx.Q.GetSpace(ctx, spaceID)
		if err != nil {
			return err
		}
		state, cmd := RelocPreparing, ""
		switch r.BlockedStep {
		case "check":
			state = RelocChecking
			if cmd, err = svc.queueRelocationCommand(ctx, tx, r.SourceNodeID, spaceID, r.GenerationBefore, r.ID, proto.CmdRelocateCheck,
				proto.RelocateCheckPayload{RelocationID: r.ID, Workdir: sp.Workdir, Flush: true, SyncWait: svc.syncWaitSeconds()}); err != nil {
				return err
			}
			kick = append(kick, r.SourceNodeID)
		case "export":
			var issues []Issue
			_ = json.Unmarshal([]byte(r.IssuesJson), &issues)
			var check proto.SpaceCheck
			_ = json.Unmarshal([]byte(r.ReportJson), &check)
			state = RelocExporting
			if cmd, err = svc.queueRelocationCommand(ctx, tx, r.SourceNodeID, spaceID, r.GenerationBefore, r.ID, proto.CmdRelocateExport,
				exportPayload(r.ID, sp.Workdir, &check, issues)); err != nil {
				return err
			}
			kick = append(kick, r.SourceNodeID)
		}
		if _, err := tx.Q.UpdateRelocationStep(ctx, db.UpdateRelocationStepParams{State: state, StepCommandID: cmd, Now: tx.NowMicros(), ID: r.ID,
			Version: r.Version}); err != nil {
			return err
		}
		if err := svc.record(ctx, tx, spaceID, "relocation", r.ID, "updated", r.Version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "relocation.retried", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"relocation_id": r.ID, "step": r.BlockedStep, "dirty_state": r.DirtyPolicy}})
		return err
	})
	if err != nil {
		return nil, err
	}
	for _, n := range kick {
		svc.hub.Kick(n)
	}
	svc.relocKick(ctx, id)
	return svc.relocationByID(ctx, id)
}

// CancelRelocation ends a relocation that has not reached finalizing. The space stays on its source (paused when the
// relocation paused it); what the destination prepared is discarded there.
func (svc *Service) CancelRelocation(ctx context.Context, actor audit.Actor, spaceID string) (*Relocation, error) {
	var r db.Relocation
	err := svc.s.W().TxNamed(ctx, "fleet.relocation_cancel", func(tx *store.Tx) error {
		var err error
		r, err = tx.Q.GetActiveRelocation(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodePrecondition, "the space has no relocation in progress")
		} else if err != nil {
			return err
		}
		if r.State == RelocFinal {
			return apperr.New(apperr.CodePrecondition, "the relocation is finalizing and can no longer be cancelled")
		}
		return svc.endRelocationTx(ctx, tx, r, RelocCancelled, "cancelled by "+actorRef(actor), actor)
	})
	if err != nil {
		return nil, err
	}
	svc.hub.Kick(r.SourceNodeID)
	svc.hub.Kick(r.DestNodeID)
	svc.removeRelocationFiles(r.ID)
	return svc.relocationByID(ctx, r.ID)
}

// endRelocationTx ends a relocation as cancelled or failed: the destination is released, the space's assignment stays
// with the source, and commands of the relocation not yet delivered are cancelled.
func (svc *Service) endRelocationTx(ctx context.Context, tx *store.Tx, r db.Relocation, state, reason string, actor audit.Actor) error {
	if _, err := tx.Q.UpdateRelocationStep(ctx, db.UpdateRelocationStepParams{State: state, BlockedStep: "", StepCommandID: "", Error: truncate(reason, 1000),
		Now: tx.NowMicros(), FinishedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: r.ID, Version: r.Version}); err != nil {
		return err
	}
	if r.StepCommandID != "" {
		if _, err := tx.SQL.ExecContext(ctx, `UPDATE node_commands SET state = 'cancelled', reason = ? WHERE id = ? AND state IN ('pending', 'sent')`,
			"relocation "+state, r.StepCommandID); err != nil {
			return err
		}
	}
	sp, err := tx.Q.GetSpace(ctx, r.SpaceID)
	if err != nil {
		return err
	}
	if r.Mode != RelocPrecheck && sp.DesiredNodeID.Valid {
		if _, err := tx.Q.SetSpaceDesiredNode(ctx, db.SetSpaceDesiredNodeParams{Now: tx.NowMicros(), ID: r.SpaceID}); err != nil {
			return err
		}
		if err := svc.record(ctx, tx, r.SpaceID, "space", r.SpaceID, "updated", sp.Version+1); err != nil {
			return err
		}
	}
	// The destination may hold a prepared copy: discard it there (a copy, never the only one).
	if r.Mode != RelocPrecheck && (r.State == RelocPreparing || (r.State == RelocBlocked && r.BlockedStep == "prepare")) {
		if _, err := svc.queueRelocationCommand(ctx, tx, r.DestNodeID, r.SpaceID, r.GenerationBefore+1, r.ID, proto.CmdRelocateRelease,
			proto.RelocateReleasePayload{RelocationID: r.ID, Workdir: sp.Workdir, Action: proto.ReleaseDiscard}); err != nil {
			return err
		}
	}
	if err := svc.record(ctx, tx, r.SpaceID, "relocation", r.ID, "updated", r.Version+1); err != nil {
		return err
	}
	ev := audit.Event{Actor: actor, Action: "relocation." + state, Target: audit.Target{Type: "space", ID: r.SpaceID},
		Details: map[string]any{"relocation_id": r.ID, "mode": r.Mode, "from_state": r.State, "reason": truncate(reason, 300)}}
	if state == RelocFailed {
		ev.Outcome = audit.OutcomeFailure
	}
	_, err = svc.audit.Append(ctx, tx, ev)
	return err
}

// ReleaseRelocationSource deletes the retained source folder of a finished relocation now (the operator acknowledges it
// is no longer needed).
func (svc *Service) ReleaseRelocationSource(ctx context.Context, actor audit.Actor, spaceID string) (*Relocation, error) {
	var r db.Relocation
	err := svc.s.W().TxNamed(ctx, "fleet.relocation_release", func(tx *store.Tx) error {
		var err error
		r, err = tx.Q.GetLatestRelocation(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return errNoRelocation
		} else if err != nil {
			return err
		}
		if r.State != RelocDone || (r.SourceState != "retained" && r.SourceState != "pending") {
			return apperr.New(apperr.CodePrecondition, "the latest relocation has no retained source folder to release (source %q)", r.SourceState)
		}
		return svc.queueSourceDeleteTx(ctx, tx, r, actor)
	})
	if err != nil {
		return nil, err
	}
	svc.hub.Kick(r.SourceNodeID)
	return svc.relocationByID(ctx, r.ID)
}

func (svc *Service) queueSourceDeleteTx(ctx context.Context, tx *store.Tx, r db.Relocation, actor audit.Actor) error {
	sp, err := tx.Q.GetSpace(ctx, r.SpaceID)
	if err != nil {
		return err
	}
	cmd, err := svc.queueRelocationCommand(ctx, tx, r.SourceNodeID, r.SpaceID, r.GenerationBefore, r.ID, proto.CmdRelocateRelease,
		proto.RelocateReleasePayload{RelocationID: r.ID, Workdir: sp.Workdir, Action: proto.ReleaseDelete})
	if err != nil {
		return err
	}
	if err := tx.Q.SetRelocationSource(ctx, db.SetRelocationSourceParams{SourceState: r.SourceState, ReleaseCommandID: cmd, Now: tx.NowMicros(), ID: r.ID}); err != nil {
		return err
	}
	if err := svc.record(ctx, tx, r.SpaceID, "relocation", r.ID, "updated", r.Version+1); err != nil {
		return err
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "relocation.source_release_requested", Target: audit.Target{Type: "space", ID: r.SpaceID},
		Details: map[string]any{"relocation_id": r.ID, "node_id": r.SourceNodeID}})
	return err
}
