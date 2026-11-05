package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// MemoryExporter writes Git bundles of a space's acknowledged role memory for a relocation's destination (ADR-0025,
// ADR-0037): one file per collection with history, named memory-<role>.bundle, in dir.
type MemoryExporter interface {
	ExportBundles(ctx context.Context, spaceID, dir string) ([]proto.RelocateMemory, error)
}

// relocation engine state shared by the service.
type relocEngine struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
	// retain wakes the retention timer (a relocation finished or a retention changed).
	retain chan struct{}
}

func (svc *Service) relocLock(key string) func() {
	e := &svc.reloc
	e.mu.Lock()
	if e.locks == nil {
		e.locks = map[string]*sync.Mutex{}
	}
	l, ok := e.locks[key]
	if !ok {
		l = &sync.Mutex{}
		e.locks[key] = l
	}
	e.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (svc *Service) relocDir(id string) string {
	if svc.opts.RelocationsDir == "" {
		return ""
	}
	return filepath.Join(svc.opts.RelocationsDir, id)
}

func (svc *Service) relocLog() *slog.Logger { return obs.Component(svc.opts.Log, "relocation") }

// relocKick advances a relocation in the background (after a commit, a report or a run's end).
func (svc *Service) relocKick(ctx context.Context, id string) {
	go svc.advanceRelocation(context.WithoutCancel(ctx), id)
}

// kickSpaceRelocation advances the space's relocation in progress, if any (a run of the space ended).
func (svc *Service) kickSpaceRelocation(ctx context.Context, spaceID string) {
	if r, err := svc.s.R().GetActiveRelocation(ctx, spaceID); err == nil && r.State == RelocDraining {
		svc.relocKick(ctx, r.ID)
	}
}

// kickNodeRelocations advances relocations that involve a node (its state changed).
func (svc *Service) kickNodeRelocations(ctx context.Context, nodeID string) {
	rows, err := svc.s.R().ListActiveRelocationsByNode(ctx, nodeID)
	if err != nil {
		return
	}
	for _, r := range rows {
		svc.relocKick(ctx, r.ID)
	}
}

// advanceRelocation performs every step that can happen now. Steps are idempotent and each ends in one transaction
// that checks the relocation's version, so a restart at any point resumes where the database says it is.
func (svc *Service) advanceRelocation(ctx context.Context, id string) {
	defer svc.relocLock(id)()
	for range 8 {
		r, err := svc.s.R().GetRelocation(ctx, id)
		if err != nil || relocEnded(r.State) {
			return
		}
		moved, err := svc.relocStep(ctx, r)
		if err != nil {
			svc.relocLog().Warn("relocation step", slog.String("relocation.id", id), slog.String("state", r.State), obs.Err(err))
			return
		}
		if !moved {
			return
		}
	}
}

func (svc *Service) relocStep(ctx context.Context, r db.Relocation) (bool, error) {
	if r.Mode != RelocPrecheck && r.State != RelocFinal {
		if n, err := svc.s.R().GetNode(ctx, r.DestNodeID); err == nil && (n.State == "revoked" || n.State == "disabled") {
			return true, svc.failRelocation(ctx, r, fmt.Sprintf("the destination node %s was %s", n.Name, n.State))
		}
	}
	switch {
	case r.State == RelocDraining:
		active, err := svc.s.R().ListActiveRunsBySpace(ctx, r.SpaceID)
		if err != nil || len(active) > 0 {
			return false, err
		}
		return true, svc.s.W().TxNamed(ctx, "fleet.relocation_check", func(tx *store.Tx) error {
			sp, err := tx.Q.GetSpace(ctx, r.SpaceID)
			if err != nil {
				return err
			}
			cmd, err := svc.queueRelocationCommand(ctx, tx, r.SourceNodeID, r.SpaceID, r.GenerationBefore, r.ID, proto.CmdRelocateCheck,
				proto.RelocateCheckPayload{RelocationID: r.ID, Workdir: sp.Workdir, Flush: true, SyncWait: svc.syncWaitSeconds()})
			if err != nil {
				return err
			}
			tx.AfterCommit(func() { svc.hub.Kick(r.SourceNodeID) })
			return svc.relocMove(ctx, tx, r, RelocChecking, "", cmd, "")
		})
	case r.State == RelocPreparing && r.StepCommandID == "":
		payload, err := svc.preparePayload(ctx, r)
		if err != nil {
			return true, svc.blockRelocation(ctx, r, "prepare", err.Error())
		}
		return true, svc.s.W().TxNamed(ctx, "fleet.relocation_prepare", func(tx *store.Tx) error {
			cmd, err := svc.queueRelocationCommand(ctx, tx, r.DestNodeID, r.SpaceID, r.GenerationBefore+1, r.ID, proto.CmdRelocatePrepare, payload)
			if err != nil {
				return err
			}
			tx.AfterCommit(func() { svc.hub.Kick(r.DestNodeID) })
			return svc.relocMove(ctx, tx, r, RelocPreparing, "", cmd, "")
		})
	case r.State == RelocFinal:
		return true, svc.finalizeRelocation(ctx, r)
	}
	return false, nil
}

// relocMove changes a relocation's state inside tx (optimistic on its version) and records the change.
func (svc *Service) relocMove(ctx context.Context, tx *store.Tx, r db.Relocation, state, blocked, cmd, msg string) error {
	var fin sql.NullInt64
	if relocEnded(state) {
		fin = sql.NullInt64{Int64: tx.NowMicros(), Valid: true}
	}
	n, err := tx.Q.UpdateRelocationStep(ctx, db.UpdateRelocationStepParams{State: state, BlockedStep: blocked, StepCommandID: cmd, Error: truncate(msg, 2000),
		Now: tx.NowMicros(), FinishedAt: fin, ID: r.ID, Version: r.Version})
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("the relocation changed concurrently")
	}
	return svc.record(ctx, tx, r.SpaceID, "relocation", r.ID, "updated", r.Version+1)
}

func (svc *Service) blockRelocation(ctx context.Context, r db.Relocation, step, msg string) error {
	return svc.s.W().TxNamed(ctx, "fleet.relocation_blocked", func(tx *store.Tx) error {
		if err := svc.relocMove(ctx, tx, r, RelocBlocked, step, "", msg); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "relocation.blocked", Outcome: audit.OutcomeFailure,
			Target: audit.Target{Type: "space", ID: r.SpaceID}, Details: map[string]any{"relocation_id": r.ID, "step": step, "reason": truncate(msg, 300)}})
		return err
	})
}

func (svc *Service) failRelocation(ctx context.Context, r db.Relocation, msg string) error {
	err := svc.s.W().TxNamed(ctx, "fleet.relocation_failed", func(tx *store.Tx) error {
		return svc.endRelocationTx(ctx, tx, r, RelocFailed, msg, audit.System)
	})
	if err == nil {
		svc.hub.Kick(r.SourceNodeID)
		svc.hub.Kick(r.DestNodeID)
		svc.removeRelocationFiles(r.ID)
	}
	return err
}

// checkIssues turns the source's inspection into what a clean relocation would lose (exportable) or what stops it.
func checkIssues(c *proto.SpaceCheck) []Issue {
	out := []Issue{}
	add := func(code, path, detail string, exportable bool) {
		out = append(out, Issue{Code: code, Path: path, Detail: detail, Exportable: exportable})
	}
	for _, e := range c.Errors {
		add("inspect_error", "", e, false)
	}
	for _, r := range c.Repositories {
		if r.Error != "" {
			add("inspect_error", r.Path, r.Error, false)
			continue
		}
		if len(r.Remotes) == 0 {
			add("no_remote", r.Path, "the repository has no remote: its history exists only on the source", true)
		}
		if r.Dirty > 0 {
			add("dirty", r.Path, fmt.Sprintf("%d changed tracked file(s)", r.Dirty), true)
		}
		if r.Untracked > 0 {
			add("untracked", r.Path, fmt.Sprintf("%d untracked file(s) or folder(s)", r.Untracked), true)
		}
		if r.Stashes > 0 {
			add("stash", r.Path, fmt.Sprintf("%d stash entr(ies)", r.Stashes), true)
		}
		if r.Detached > 0 {
			add("detached", r.Path, fmt.Sprintf("%d commit(s) only on the detached HEAD", r.Detached), true)
		}
		for _, b := range r.Branches {
			if b.Unpushed > 0 {
				what := "not on its upstream"
				if b.NoUpstream {
					what = "on no remote (the branch has no upstream)"
				}
				add("unpushed", r.Path, fmt.Sprintf("branch %s: %d commit(s) %s", b.Name, b.Unpushed, what), true)
			}
		}
		for _, w := range r.Worktrees {
			switch {
			case w.Error != "":
				add("inspect_error", w.Path, w.Error, false)
			case w.Dirty > 0 || w.Untracked > 0:
				add("worktree", w.Path, fmt.Sprintf("worktree of %s: %d changed, %d untracked", r.Path, w.Dirty, w.Untracked), true)
			}
		}
	}
	if c.Loose.Files > 0 {
		add("loose_files", "", fmt.Sprintf("%d file(s), %d bytes outside any repository", c.Loose.Files, c.Loose.Bytes), true)
	}
	for _, m := range c.Memory {
		if m.Pending || m.Uncheckpointed > 0 || m.LocalCommits > 0 {
			add("memory_unsynced", m.Role, fmt.Sprintf("role memory not acknowledged by central (%d uncheckpointed file(s), %d local commit(s)); retry once it syncs",
				m.Uncheckpointed, m.LocalCommits), false)
		}
	}
	if len(c.Recordings) > 0 {
		add("recordings_pending", "", fmt.Sprintf("%d run recording(s) not fully stored by central; retry once they are", len(c.Recordings)), false)
	}
	if len(c.Transcripts) > 0 {
		add("transcripts_pending", "", fmt.Sprintf("%d native transcript upload(s) unfinished; retry once they are", len(c.Transcripts)), false)
	}
	if len(c.ActiveRuns) > 0 {
		add("active_runs", "", fmt.Sprintf("%d run(s) still active on the source", len(c.ActiveRuns)), false)
	}
	return out
}

func summarize(issues []Issue, exportable bool) string {
	var parts []string
	for _, i := range issues {
		if i.Exportable == exportable {
			s := i.Code
			if i.Path != "" {
				s += " (" + i.Path + ")"
			}
			parts = append(parts, s)
		}
	}
	if len(parts) > 6 {
		parts = append(parts[:6], fmt.Sprintf("and %d more", len(parts)-6))
	}
	return strings.Join(parts, ", ")
}

// exportPayload lists what the source exports: the repositories with exportable issues and, if any, the loose files.
func exportPayload(id, workdir string, c *proto.SpaceCheck, issues []Issue) proto.RelocateExportPayload {
	p := proto.RelocateExportPayload{RelocationID: id, Workdir: workdir, Repos: []string{}, MaxBytes: proto.RelocationExportMax}
	repos := map[string]bool{}
	for _, r := range c.Repositories {
		repos[r.Path] = true
	}
	seen := map[string]bool{}
	for _, i := range issues {
		if !i.Exportable {
			continue
		}
		if i.Code == "loose_files" {
			p.Loose = true
			continue
		}
		path := i.Path
		if i.Code == "worktree" {
			path = ""
			for _, r := range c.Repositories {
				for _, w := range r.Worktrees {
					if w.Path == i.Path {
						path = r.Path
					}
				}
			}
		}
		if repos[path] && !seen[path] {
			seen[path] = true
			p.Repos = append(p.Repos, path)
		}
	}
	sort.Strings(p.Repos)
	return p
}

// relocationReport applies a node's report for the step the relocation waits for; anything else is stale and ignored.
func (svc *Service) relocationReport(ctx context.Context, nodeID string, rep proto.RelocationReport) {
	log := svc.relocLog().With(slog.String("relocation.id", rep.RelocationID), slog.String("kind", rep.Kind))
	moved := false
	func() {
		defer svc.relocLock(rep.RelocationID)()
		r, err := svc.s.R().GetRelocation(ctx, rep.RelocationID)
		if err != nil {
			return
		}
		if rep.Kind == proto.CmdRelocateRelease {
			if err := svc.applyRelease(ctx, nodeID, r, rep); err != nil {
				log.Warn("apply release report", obs.Err(err))
			}
			return
		}
		if relocEnded(r.State) || r.StepCommandID == "" || r.StepCommandID != rep.CommandID {
			return
		}
		want := r.SourceNodeID
		if rep.Kind == proto.CmdRelocatePrepare {
			want = r.DestNodeID
		}
		if nodeID != want {
			log.Warn("relocation report from the wrong node", slog.String(obs.FieldNodeID, nodeID))
			return
		}
		var err2 error
		switch {
		case rep.Kind == proto.CmdRelocateCheck && r.State == RelocChecking:
			err2 = svc.applyCheck(ctx, r, rep)
		case rep.Kind == proto.CmdRelocateExport && r.State == RelocExporting:
			err2 = svc.applyExport(ctx, r, rep)
		case rep.Kind == proto.CmdRelocatePrepare && r.State == RelocPreparing:
			err2 = svc.applyPrepare(ctx, r, rep)
		default:
			return
		}
		if err2 != nil {
			log.Warn("apply relocation report", obs.Err(err2))
			return
		}
		moved = true
	}()
	if moved {
		svc.advanceRelocation(ctx, rep.RelocationID)
	}
}

func (svc *Service) applyCheck(ctx context.Context, r db.Relocation, rep proto.RelocationReport) error {
	if !rep.OK || rep.Check == nil {
		if r.Mode == RelocPrecheck {
			return svc.s.W().TxNamed(ctx, "fleet.relocation_checked", func(tx *store.Tx) error {
				return svc.relocMove(ctx, tx, r, RelocFailed, "", "", "the source could not inspect the space: "+rep.Error)
			})
		}
		return svc.blockRelocation(ctx, r, "check", "the source could not inspect the space: "+rep.Error)
	}
	issues := checkIssues(rep.Check)
	report, _ := json.Marshal(rep.Check)
	issuesJSON, _ := json.Marshal(issues)
	var sp db.Space
	return svc.s.W().TxNamed(ctx, "fleet.relocation_check_report", func(tx *store.Tx) error {
		var err error
		if sp, err = tx.Q.GetSpace(ctx, r.SpaceID); err != nil {
			return err
		}
		if err := tx.Q.SetRelocationReport(ctx, db.SetRelocationReportParams{ReportJson: string(report), IssuesJson: string(issuesJSON), ID: r.ID}); err != nil {
			return err
		}
		blocking, exportable := summarize(issues, false), summarize(issues, true)
		details := map[string]any{"relocation_id": r.ID, "issues": len(issues), "repositories": len(rep.Check.Repositories)}
		action, outcome := "relocation.checked", audit.OutcomeSuccess
		var moveErr error
		switch {
		case r.Mode == RelocPrecheck:
			moveErr = svc.relocMove(ctx, tx, r, RelocChecked, "", "", "")
		case blocking != "":
			action, outcome = "relocation.blocked", audit.OutcomeFailure
			details["blocking"] = blocking
			moveErr = svc.relocMove(ctx, tx, r, RelocBlocked, "check", "", "resolve before relocating: "+blocking)
		case exportable != "" && r.DirtyPolicy != "export":
			// SM-SPACE-005: never lose state silently; the operator chooses an explicit export or resolves it.
			action, outcome = "relocation.refused", audit.OutcomeDenied
			details["exportable"] = exportable
			moveErr = svc.relocMove(ctx, tx, r, RelocBlocked, "check", "",
				"refused: the source has state that is not pushed: "+exportable+". Push it, or retry with dirty_state export to carry it through central")
		case exportable != "":
			cmd, err := svc.queueRelocationCommand(ctx, tx, r.SourceNodeID, r.SpaceID, r.GenerationBefore, r.ID, proto.CmdRelocateExport,
				exportPayload(r.ID, sp.Workdir, rep.Check, issues))
			if err != nil {
				return err
			}
			tx.AfterCommit(func() { svc.hub.Kick(r.SourceNodeID) })
			details["exportable"] = exportable
			moveErr = svc.relocMove(ctx, tx, r, RelocExporting, "", cmd, "")
		default:
			moveErr = svc.relocMove(ctx, tx, r, RelocPreparing, "", "", "")
		}
		if moveErr != nil {
			return moveErr
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: action, Outcome: outcome, Target: audit.Target{Type: "space", ID: r.SpaceID},
			Details: details})
		return err
	})
}

func (svc *Service) applyExport(ctx context.Context, r db.Relocation, rep proto.RelocationReport) error {
	if !rep.OK || rep.Export == nil {
		return svc.blockRelocation(ctx, r, "export", "the source could not export its state: "+rep.Error)
	}
	// Every exported file must be stored by central with the announced size and digest.
	dir := svc.relocDir(r.ID)
	for _, b := range rep.Export.Blobs {
		if !proto.ValidBlobName(b.Name) || dir == "" {
			return svc.blockRelocation(ctx, r, "export", "invalid export file "+b.Name)
		}
		size, sum, err := fileDigest(filepath.Join(dir, b.Name))
		if err != nil || size != b.Size || sum != b.SHA256 {
			return svc.blockRelocation(ctx, r, "export", fmt.Sprintf("export file %s is missing or does not match on central", b.Name))
		}
	}
	b, _ := json.Marshal(rep.Export)
	return svc.s.W().TxNamed(ctx, "fleet.relocation_exported", func(tx *store.Tx) error {
		if err := tx.Q.SetRelocationExport(ctx, db.SetRelocationExportParams{ExportJson: string(b), ID: r.ID}); err != nil {
			return err
		}
		if err := svc.relocMove(ctx, tx, r, RelocPreparing, "", "", ""); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "relocation.exported", Target: audit.Target{Type: "space", ID: r.SpaceID},
			Details: map[string]any{"relocation_id": r.ID, "repositories": len(rep.Export.Repos), "files": len(rep.Export.Blobs), "loose_files": rep.Export.LooseFiles}})
		return err
	})
}

// preparePayload builds the destination's instructions: clean repositories to clone, the export manifest, and central's
// role memory as bundles (written into the relocation's folder, outside any transaction).
func (svc *Service) preparePayload(ctx context.Context, r db.Relocation) (proto.RelocatePreparePayload, error) {
	sp, err := svc.s.R().GetSpace(ctx, r.SpaceID)
	if err != nil {
		return proto.RelocatePreparePayload{}, err
	}
	p := proto.RelocatePreparePayload{RelocationID: r.ID, Workdir: sp.Workdir, Repos: []proto.RelocateRepo{}, Memory: []proto.RelocateMemory{},
		Blobs: []proto.RelocateBlob{}}
	exported := map[string]bool{}
	if r.ExportJson != "" {
		var ex proto.RelocateExport
		if err := json.Unmarshal([]byte(r.ExportJson), &ex); err != nil {
			return p, err
		}
		p.Export = &ex
		p.Blobs = append(p.Blobs, ex.Blobs...)
		for _, x := range ex.Repos {
			exported[x.Path] = true
		}
	}
	switch r.Mode {
	case RelocClean:
		var c proto.SpaceCheck
		if err := json.Unmarshal([]byte(r.ReportJson), &c); err != nil {
			return p, errors.New("the relocation has no inspection report")
		}
		for _, repo := range c.Repositories {
			if exported[repo.Path] {
				continue
			}
			rr := proto.RelocateRepo{Path: repo.Path, Remotes: repo.Remotes, Branch: repo.Branch, Head: repo.Head, Branches: repo.Branches,
				Worktrees: []proto.ExportedWorktree{}}
			for _, w := range repo.Worktrees {
				// The report comes from the source node: a path outside the space folder is refused here as well as on
				// the destination (security review SR-M02).
				if !localSpacePath(w.Path) || !localSpacePath(repo.Path) {
					return p, fmt.Errorf("the inspection report has an invalid path %q", w.Path)
				}
				rr.Worktrees = append(rr.Worktrees, proto.ExportedWorktree{Path: w.Path, Branch: w.Branch, Head: w.Head})
			}
			p.Repos = append(p.Repos, rr)
		}
		// Configured repositories the source never cloned are cloned on the destination when their policy is auto
		// (ADR-0044): the destination gets the space's configured list, not only what the source had.
		inspected := map[string]bool{}
		for _, repo := range c.Repositories {
			inspected[repo.Path] = true
		}
		configured, err := svc.s.R().ListSpaceRepositories(ctx, r.SpaceID)
		if err != nil {
			return p, err
		}
		for _, cr := range configured {
			// A repository with a SpaceMan SSH key is cloned by the first sync after the move, which carries its key (ADR-0057).
			if cr.ClonePolicy == proto.CloneAuto && cr.Url != "" && len(cr.SshKeyEnc) == 0 && !inspected[cr.Path] && !exported[cr.Path] && localSpacePath(cr.Path) {
				p.Repos = append(p.Repos, proto.RelocateRepo{Path: cr.Path, Remotes: []proto.GitRemote{{Name: "origin", URL: cr.Url}}, Branch: cr.DefaultBranch,
					Branches: []proto.BranchCheck{}, Worktrees: []proto.ExportedWorktree{}})
			}
		}
	case RelocSourceUnavailable:
		var specs []RepoSpec
		_ = json.Unmarshal([]byte(r.ReposJson), &specs)
		for _, s := range specs {
			p.Repos = append(p.Repos, proto.RelocateRepo{Path: s.Path, Remotes: []proto.GitRemote{{Name: "origin", URL: s.URL}}, Branch: s.Branch,
				Branches: []proto.BranchCheck{}, Worktrees: []proto.ExportedWorktree{}})
		}
	}
	cols, err := svc.s.R().ListMemoryCollections(ctx, r.SpaceID)
	if err != nil {
		return p, err
	}
	withHistory := 0
	for _, c := range cols {
		if c.Head != "" {
			withHistory++
		}
	}
	if withHistory > 0 {
		dir := svc.relocDir(r.ID)
		if svc.MemoryExport == nil || dir == "" {
			return p, errors.New("central cannot export role memory for the destination (no memory store)")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return p, err
		}
		mems, err := svc.MemoryExport.ExportBundles(ctx, r.SpaceID, dir)
		if err != nil {
			return p, fmt.Errorf("export role memory: %w", err)
		}
		for _, m := range mems {
			size, sum, err := fileDigest(filepath.Join(dir, m.Blob))
			if err != nil {
				return p, err
			}
			if err := os.WriteFile(filepath.Join(dir, m.Blob+".sha256"), []byte(sum), 0o600); err != nil {
				return p, err
			}
			p.Memory = append(p.Memory, m)
			p.Blobs = append(p.Blobs, proto.RelocateBlob{Name: m.Blob, Size: size, SHA256: sum})
		}
	}
	return p, nil
}

func (svc *Service) applyPrepare(ctx context.Context, r db.Relocation, rep proto.RelocationReport) error {
	if !rep.OK || rep.Prepare == nil {
		return svc.blockRelocation(ctx, r, "prepare", "the destination could not prepare the space: "+rep.Error)
	}
	// Verify against what was sent: every repository at the source's commit, every collection at central's head.
	var sent proto.RelocatePreparePayload
	if cmd, err := svc.s.R().GetNodeCommand(ctx, r.StepCommandID); err == nil {
		_ = json.Unmarshal([]byte(cmd.PayloadJson), &sent)
	}
	got := map[string]proto.PreparedRepo{}
	for _, x := range rep.Prepare.Repos {
		got[x.Path] = x
	}
	var problems []string
	check := func(path, head string) {
		x, ok := got[path]
		switch {
		case !ok:
			problems = append(problems, path+": not prepared")
		case head != "" && x.Head != head:
			problems = append(problems, fmt.Sprintf("%s: at %.12s, expected %.12s", path, x.Head, head))
		}
	}
	for _, x := range sent.Repos {
		check(x.Path, x.Head)
	}
	if sent.Export != nil {
		for _, x := range sent.Export.Repos {
			check(x.Path, x.Head)
		}
	}
	mem := map[string]string{}
	for _, m := range rep.Prepare.Memory {
		mem[m.Role] = m.Head
	}
	for _, m := range sent.Memory {
		if mem[m.Role] != m.Head {
			problems = append(problems, "memory "+m.Role+": not restored at central's head")
		}
	}
	if len(problems) > 0 {
		return svc.blockRelocation(ctx, r, "prepare", "the destination's copy does not match: "+strings.Join(problems, "; "))
	}
	b, _ := json.Marshal(rep.Prepare)
	return svc.s.W().TxNamed(ctx, "fleet.relocation_prepared", func(tx *store.Tx) error {
		if err := tx.Q.SetRelocationPrepare(ctx, db.SetRelocationPrepareParams{PrepareJson: string(b), ID: r.ID}); err != nil {
			return err
		}
		return svc.relocMove(ctx, tx, r, RelocFinal, "", "", "")
	})
}

// finalizeRelocation moves the assignment in one transaction (SM-SPACE-005): the space's node becomes the destination,
// the desired destination is cleared, and the lease moves to the destination with a higher generation, which fences the
// source's runs, commands and reports. The space stays paused. The source is told to retain or delete its folder.
func (svc *Service) finalizeRelocation(ctx context.Context, r db.Relocation) error {
	var gen int64
	err := svc.s.W().TxNamed(ctx, "fleet.relocation_finalize", func(tx *store.Tx) error {
		sp, err := tx.Q.GetSpace(ctx, r.SpaceID)
		if err != nil {
			return err
		}
		if !sp.DesiredNodeID.Valid || sp.DesiredNodeID.String != r.DestNodeID || sp.NodeID != r.SourceNodeID {
			return svc.endRelocationTx(ctx, tx, r, RelocFailed, "the space's assignment changed during the relocation", audit.System)
		}
		gen = r.GenerationBefore
		if l, err := tx.Q.GetLease(ctx, r.SpaceID); err == nil {
			gen = max(gen, l.Generation)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		gen++
		if n, err := tx.Q.FinalizeSpaceNode(ctx, db.FinalizeSpaceNodeParams{NodeID: r.DestNodeID, Now: tx.NowMicros(), ID: r.SpaceID}); err != nil {
			return err
		} else if n == 0 {
			return errors.New("the space's desired node changed concurrently")
		}
		// Granted to the destination at its next lease sync; expired until then, so nothing can run on it yet.
		if err := tx.Q.MoveLease(ctx, db.MoveLeaseParams{SpaceID: r.SpaceID, NodeID: r.DestNodeID, Generation: gen, GrantedAt: tx.NowMicros(),
			RenewedAt: tx.NowMicros(), ExpiresAt: tx.NowMicros()}); err != nil {
			return err
		}
		if _, err := tx.Q.CancelSpaceNodeCommands(ctx, db.CancelSpaceNodeCommandsParams{Reason: "the space was relocated", SpaceID: r.SpaceID,
			NodeID: r.SourceNodeID}); err != nil {
			return err
		}
		lost := int64(0)
		if r.Mode == RelocSourceUnavailable {
			if lost, err = tx.Q.MarkRunsLostForRelocation(ctx, db.MarkRunsLostForRelocationParams{Now: tx.NowMicros(), SpaceID: r.SpaceID,
				NodeID: r.SourceNodeID}); err != nil {
				return err
			}
		}
		if _, err := tx.Q.RevokeSpaceRunCredentials(ctx, db.RevokeSpaceRunCredentialsParams{RevokedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			SpaceID: r.SpaceID}); err != nil {
			return err
		}
		action := proto.ReleaseRetain
		if r.Recovery == "delete" {
			action = proto.ReleaseDelete
		}
		cmd, err := svc.queueRelocationCommand(ctx, tx, r.SourceNodeID, r.SpaceID, r.GenerationBefore, r.ID, proto.CmdRelocateRelease,
			proto.RelocateReleasePayload{RelocationID: r.ID, Workdir: sp.Workdir, Action: action})
		if err != nil {
			return err
		}
		var until sql.NullInt64
		if r.Recovery == "retain" && r.RetainDays > 0 {
			until = sql.NullInt64{Int64: tx.Now.Add(time.Duration(r.RetainDays) * 24 * time.Hour).UnixMicro(), Valid: true}
		}
		if err := tx.Q.SetRelocationFinal(ctx, db.SetRelocationFinalParams{GenerationAfter: gen, SourceState: "pending", ReleaseCommandID: cmd,
			RetainUntil: until, ID: r.ID}); err != nil {
			return err
		}
		if err := svc.relocMove(ctx, tx, r, RelocDone, "", "", ""); err != nil {
			return err
		}
		if err := svc.record(ctx, tx, r.SpaceID, "space", r.SpaceID, "updated", sp.Version+1); err != nil {
			return err
		}
		tx.AfterCommit(func() {
			svc.hub.Kick(r.DestNodeID)
			svc.hub.Kick(r.SourceNodeID)
		})
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "space.relocated", Target: audit.Target{Type: "space", ID: r.SpaceID},
			Details: map[string]any{"relocation_id": r.ID, "mode": r.Mode, "from": r.SourceNodeID, "to": r.DestNodeID, "generation_before": r.GenerationBefore,
				"generation_after": gen, "exported": r.ExportJson != "", "runs_lost": lost, "recovery": r.Recovery, "native_sessions": "not_carried"}})
		return err
	})
	if err != nil {
		return err
	}
	svc.removeMemoryBlobs(r.ID)
	svc.retainKick()
	return nil
}

// applyRelease records what a node did with a space's folder after a relocation finished (source) or was cancelled
// (destination).
func (svc *Service) applyRelease(ctx context.Context, nodeID string, r db.Relocation, rep proto.RelocationReport) error {
	if nodeID == r.DestNodeID && r.State != RelocDone {
		return nil // a discard on the destination after a cancel: nothing to record beyond the node's own log
	}
	if nodeID != r.SourceNodeID || r.ReleaseCommandID != rep.CommandID {
		return nil
	}
	state := "unreachable"
	if rep.OK && rep.Release != nil {
		state = "retained"
		if rep.Release.Action == proto.ReleaseDelete {
			state = "deleted"
		}
	}
	err := svc.s.W().TxNamed(ctx, "fleet.relocation_released", func(tx *store.Tx) error {
		cur, err := tx.Q.GetRelocation(ctx, r.ID)
		if err != nil {
			return err
		}
		until := cur.RetainUntil
		if state != "retained" {
			until = sql.NullInt64{}
		}
		if err := tx.Q.SetRelocationSource(ctx, db.SetRelocationSourceParams{SourceState: state, ReleaseCommandID: "", RetainUntil: until, Now: tx.NowMicros(),
			ID: r.ID}); err != nil {
			return err
		}
		if err := svc.record(ctx, tx, r.SpaceID, "relocation", r.ID, "updated", cur.Version+1); err != nil {
			return err
		}
		ev := audit.Event{Actor: audit.Actor{Type: audit.ActorNode, ID: nodeID}, Action: "relocation.source_released", Target: audit.Target{Type: "space", ID: r.SpaceID},
			Details: map[string]any{"relocation_id": r.ID, "source_state": state}}
		if rep.Release != nil {
			ev.Details["retained_as"] = rep.Release.RetainedAs
		}
		if !rep.OK {
			ev.Outcome = audit.OutcomeFailure
			ev.Details["error"] = truncate(rep.Error, 300)
		}
		_, err = svc.audit.Append(ctx, tx, ev)
		return err
	})
	if err == nil && state == "deleted" {
		svc.removeRelocationFiles(r.ID)
	}
	svc.retainKick()
	return err
}

// relocationRejected handles a node refusing a relocation command (for example an agent without protocol minor 3).
func (svc *Service) relocationRejected(ctx context.Context, commandID, reason string) {
	r, err := svc.s.R().GetRelocationByCommand(ctx, commandID)
	if err != nil || relocEnded(r.State) || r.StepCommandID != commandID {
		return
	}
	step := map[string]string{RelocChecking: "check", RelocExporting: "export", RelocPreparing: "prepare"}[r.State]
	if step == "" {
		return
	}
	if r.Mode == RelocPrecheck {
		_ = svc.s.W().TxNamed(ctx, "fleet.relocation_rejected", func(tx *store.Tx) error {
			return svc.endRelocationTx(ctx, tx, r, RelocFailed, "the node rejected the check: "+reason, audit.System)
		})
		return
	}
	if err := svc.blockRelocation(ctx, r, step, "the node rejected the step: "+reason); err != nil {
		svc.relocLog().Warn("block rejected relocation", obs.Err(err))
	}
}

func (svc *Service) retainKick() {
	select {
	case svc.reloc.retain <- struct{}{}:
	default:
	}
}

// RunRelocations resumes relocations in progress after a start and runs the retention timer: one timer for the earliest
// retained source folder that is due for deletion (SM-ARCH-004).
func (svc *Service) RunRelocations(ctx context.Context) {
	if rows, err := svc.s.R().ListActiveRelocations(ctx); err == nil {
		for _, r := range rows {
			svc.advanceRelocation(ctx, r.ID)
		}
	}
	for {
		var due <-chan time.Time
		var t *time.Timer
		if next, err := svc.s.R().NextRelocationRetention(ctx); err == nil && next > 0 {
			t = time.NewTimer(max(time.Until(time.UnixMicro(next)), 0))
			due = t.C
		}
		select {
		case <-ctx.Done():
			if t != nil {
				t.Stop()
			}
			return
		case <-svc.reloc.retain:
		case <-due:
			svc.expireRetention(ctx)
		}
		if t != nil {
			t.Stop()
		}
	}
}

func (svc *Service) expireRetention(ctx context.Context) {
	rows, err := svc.s.R().ListDueRelocationRetention(ctx, sql.NullInt64{Int64: svc.now().UnixMicro(), Valid: true})
	if err != nil {
		return
	}
	for _, r := range rows {
		err := svc.s.W().TxNamed(ctx, "fleet.relocation_retention", func(tx *store.Tx) error {
			return svc.queueSourceDeleteTx(ctx, tx, r, audit.System)
		})
		if err != nil {
			svc.relocLog().Warn("expire retained source", slog.String("relocation.id", r.ID), obs.Err(err))
			continue
		}
		svc.hub.Kick(r.SourceNodeID)
	}
}

// removeRelocationFiles deletes central's files of a relocation (its own folder, never anything else).
func (svc *Service) removeRelocationFiles(id string) {
	if dir := svc.relocDir(id); dir != "" && filepath.Base(dir) == id {
		_ = os.RemoveAll(dir)
	}
}

// removeMemoryBlobs deletes the generated memory bundles of a finished relocation (central keeps the history itself);
// exported state stays until the source's folder is gone.
func (svc *Service) removeMemoryBlobs(id string) {
	dir := svc.relocDir(id)
	if dir == "" {
		return
	}
	ents, _ := os.ReadDir(dir)
	left := 0
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "memory-") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		left++
	}
	if left == 0 {
		_ = os.Remove(dir)
	}
}

// localSpacePath reports whether a slash-separated path reported by a node stays inside the space folder and outside
// the harnesses' instances folder.
func localSpacePath(p string) bool {
	return p != "" && path.Clean(p) == p && !path.IsAbs(p) && p != ".." && !strings.HasPrefix(p, "../") &&
		p != "instances" && !strings.HasPrefix(p, "instances/")
}
