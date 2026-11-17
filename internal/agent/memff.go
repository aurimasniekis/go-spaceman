package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Fast-forward of idle memory checkouts (ADR-0094). When a checkpoint moved a collection's shared head, the checkouts
// of the collection's other instances whose live runs are between turns are moved onto it with `merge --ff-only`, so
// their next edit is made on the newest notes instead of becoming a conflict. Run start (prepare) uses the same
// content and ancestry checks. A checkout is only ever moved forward along history the shared repository already has:
// never reset, never merged, never when it holds anything that is not in that history.

// Fixed reasons a checkout is not fast-forwarded (logged; tests assert them). "" means it was fast-forwarded.
const (
	ffDisabled        = "disabled"
	ffNoCheckout      = "no_checkout"
	ffUnsafeConfig    = "unsafe_config"
	ffMerging         = "merge_in_progress"
	ffRebasing        = "rebase_in_progress"
	ffCherryPicking   = "cherry_pick_in_progress"
	ffReverting       = "revert_in_progress"
	ffBisecting       = "bisect_in_progress"
	ffIndexLocked     = "index_locked"
	ffStatusFailed    = "status_failed"
	ffStaged          = "staged_changes"
	ffModified        = "modified_files"
	ffUntracked       = "untracked_files"
	ffIgnored         = "ignored_files"
	ffNoHead          = "no_head"
	ffNoShared        = "no_shared_head"
	ffUpToDate        = "up_to_date"
	ffDiverged        = "diverged"
	ffMergeFailed     = "merge_failed"
	ffNoLiveRun       = "no_live_run"
	ffRunNotRunning   = "run_not_running"
	ffTurnNotEnded    = "turn_not_ended"
	ffCheckpointDue   = "checkpoint_pending"
	ffTakenOver       = "taken_over"
	ffInputInDelivery = "input_in_delivery"
	ffInputPending    = "input_pending"
	ffOtherLiveRun    = "other_live_run"
	ffJournalFailed   = "journal_failed"
)

// inProgress are the files git keeps in the git directory while an operation is unfinished, with their reasons.
var inProgress = []struct{ name, reason string }{
	{"MERGE_HEAD", ffMerging},
	{"rebase-merge", ffRebasing},
	{"rebase-apply", ffRebasing},
	{"CHERRY_PICK_HEAD", ffCherryPicking},
	{"REVERT_HEAD", ffReverting},
	{"BISECT_LOG", ffBisecting},
	{"index.lock", ffIndexLocked},
}

// inputGate is held while the node delivers input to a run of the instance, and by a fast-forward of the instance's
// checkout from its last idle check to the end of the merge: input waits for a fast-forward, and a fast-forward
// skips an instance that is being given input.
func (m *memManager) inputGate(instance string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gates == nil {
		m.gates = map[string]*sync.Mutex{}
	}
	g, ok := m.gates[instance]
	if !ok {
		g = &sync.Mutex{}
		m.gates[instance] = g
	}
	return g
}

// inputMark is input the node delivered to a run whose turn the run has not reported yet: the turn counter it had.
type inputMark struct {
	runID string
	turn  uint64
}

// deliverInput runs send (the node typing input into run r) under the instance's input gate. Input that was sent marks
// the instance busy until the run reports a later turn: its status may still say idle while the turn begins.
func (m *memManager) deliverInput(r runRow, send func() error) error {
	if r.InstanceID == "" || !m.ffEnabled() {
		return send() // memory.fast_forward off: no fast-forward to wait for, and no mark to read
	}
	g := m.inputGate(r.InstanceID)
	g.Lock()
	defer g.Unlock()
	err := send()
	if err == nil {
		m.mu.Lock()
		if m.inputs == nil {
			m.inputs = map[string]inputMark{}
		}
		m.inputs[r.InstanceID] = inputMark{runID: r.RunID, turn: r.Turn}
		m.mu.Unlock()
	}
	return err
}

// awaitInputGate waits until no fast-forward of the instance's checkout is merging (a takeover that starts after it).
func (m *memManager) awaitInputGate(instance string) {
	if instance == "" || !m.ffEnabled() {
		return
	}
	g := m.inputGate(instance)
	g.Lock()
	g.Unlock() //nolint:staticcheck // SA2001: an empty critical section is the point: wait for the holder
}

// ffEnabled reports whether ADR-0094's fast-forward is on (memory.fast_forward).
func (m *memManager) ffEnabled() bool { return !m.a.o.NoMemoryFastForward }

// checkoutReason checks an instance checkout's content and its ancestry against the shared head. It returns the
// shared head to move to, or a reason not to. The collection lock is held.
func (m *memManager) checkoutReason(ctx context.Context, shared, inst string) (string, string) {
	g := m.git
	gitDir := filepath.Join(inst, ".git")
	if fi, err := os.Lstat(gitDir); err != nil || !fi.IsDir() {
		return "", ffNoCheckout
	}
	list, err := g.Run(ctx, inst, nil, "config", "--list", "--name-only", "--show-scope")
	if err != nil || memory.UnsafeConfig(list, true) != nil || !m.ownWorktree(ctx, inst) {
		return "", ffUnsafeConfig
	}
	for _, p := range inProgress {
		if _, err := os.Lstat(filepath.Join(gitDir, p.name)); err == nil {
			return "", p.reason
		}
	}
	out, err := g.Run(ctx, inst, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return "", ffStatusFailed
	}
	if r := statusReason(out); r != "" {
		return "", r
	}
	head := g.RevParse(ctx, inst, "HEAD")
	if head == "" {
		return "", ffNoHead
	}
	theirs := g.RevParse(ctx, shared, "refs/heads/main")
	switch theirs {
	case "":
		return "", ffNoShared
	case head:
		return "", ffUpToDate
	}
	if _, err := g.Run(ctx, inst, nil, "fetch", "--quiet", "--no-tags", shared, "main"); err != nil || g.RevParse(ctx, inst, "FETCH_HEAD") != theirs {
		return "", ffNoShared
	}
	if !g.IsAncestor(ctx, inst, head, theirs) {
		return "", ffDiverged
	}
	return theirs, ""
}

// ownWorktree reports whether git works on the checkout folder itself: a checkout's own configuration could point it
// at another work tree (core.worktree) or declare it bare, and status and merge would then check and write a different
// folder than the one checked.
func (m *memManager) ownWorktree(ctx context.Context, inst string) bool {
	out, err := m.git.Run(ctx, inst, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return false
	}
	want, err := filepath.EvalSymlinks(inst)
	if err != nil {
		return false
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(out))
	return err == nil && got == want
}

// statusReason classifies `git status --porcelain=v1 -z` output: the first kind of local content found, or "" when
// the checkout holds nothing outside its HEAD.
func statusReason(out string) string {
	var staged, modified, untracked, ignored bool
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 3 {
			continue
		}
		x, y := e[0], e[1]
		switch {
		case x == '?' && y == '?':
			untracked = true
		case x == '!' && y == '!':
			ignored = true
		default:
			if x != ' ' {
				staged = true
			}
			if y != ' ' {
				modified = true
			}
			if x == 'R' || x == 'C' {
				i++ // the source path of a rename or copy follows
			}
		}
	}
	switch {
	case staged:
		return ffStaged
	case modified:
		return ffModified
	case untracked:
		return ffUntracked
	case ignored:
		return ffIgnored
	}
	return ""
}

// fastForward moves a checkout to theirs with `merge --ff-only` and logs the change. The collection lock is held.
func (m *memManager) fastForward(ctx context.Context, spaceID, role, instance, inst, theirs, why string) string {
	g := m.git
	old := g.RevParse(ctx, inst, "HEAD")
	// --no-overwrite-ignore: git otherwise replaces an ignored file at a path the new history adds. The checkout's
	// sparse-checkout patterns (memharness.go) make git skip that check, so the paths are looked at here as well.
	added, err := g.Run(ctx, inst, nil, "diff", "--name-only", "-z", "--diff-filter=ACR", old, theirs)
	if err != nil {
		m.ffSkipped(ctx, spaceID, role, instance, ffMergeFailed)
		return ffMergeFailed
	}
	for _, f := range strings.Split(added, "\x00") {
		if f == "" || isHarnessPath(f) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(inst, filepath.FromSlash(f))); err == nil {
			m.ffSkipped(ctx, spaceID, role, instance, ffMergeFailed)
			return ffMergeFailed
		}
	}
	if _, err := g.Run(ctx, inst, nil, "merge", "--quiet", "--ff-only", "--no-overwrite-ignore", theirs); err != nil {
		m.ffSkipped(ctx, spaceID, role, instance, ffMergeFailed)
		return ffMergeFailed
	}
	files := 0
	if out, err := g.Run(ctx, inst, nil, "diff", "--name-only", "-z", old, theirs); err == nil {
		for _, f := range strings.Split(out, "\x00") {
			if f != "" {
				files++
			}
		}
	}
	m.a.log.Info("memory checkout fast-forwarded", slog.String(obs.FieldSpaceID, spaceID), slog.String("memory.role", role),
		slog.String("memory.instance", instance), slog.String("memory.from", old), slog.String("memory.to", theirs), slog.Int("files", files),
		slog.String("memory.when", why))
	return ""
}

// ffSkipped logs a refusal. A busy instance is ordinary (debug); a checkout of an idle instance, or one at run start,
// that holds content outside its history or is in an odd state is not, and it keeps the instance from following the
// head until someone looks (info).
func (m *memManager) ffSkipped(ctx context.Context, spaceID, role, instance, reason string) {
	level := slog.LevelInfo
	switch reason {
	case ffUpToDate, ffNoLiveRun, ffRunNotRunning, ffTurnNotEnded, ffCheckpointDue, ffTakenOver, ffInputInDelivery, ffInputPending, ffOtherLiveRun:
		level = slog.LevelDebug
	}
	m.a.log.Log(ctx, level, "memory checkout not fast-forwarded", slog.String(obs.FieldSpaceID, spaceID), slog.String("memory.role", role),
		slog.String("memory.instance", instance), slog.String("reason", reason))
}

// refreshAtStart is prepare's refresh of an existing checkout before a run starts (the run is journaled and not
// started). Another live run of the same instance may be writing in the checkout, so then it is left alone. The
// collection lock is held.
func (m *memManager) refreshAtStart(ctx context.Context, spaceID, role, instance string) string {
	shared, inst := m.shared(spaceID, role), m.instance(spaceID, role, instance)
	if !m.ffEnabled() {
		m.refresh(ctx, shared, inst)
		return ffDisabled
	}
	if runs, err := m.a.j.runs(ctx, true); err != nil {
		m.ffSkipped(ctx, spaceID, role, instance, ffJournalFailed)
		return ffJournalFailed
	} else if n := countInstance(runs, instance); n > 1 {
		m.ffSkipped(ctx, spaceID, role, instance, ffOtherLiveRun)
		return ffOtherLiveRun
	}
	theirs, reason := m.checkoutReason(ctx, shared, inst)
	if reason != "" {
		m.ffSkipped(ctx, spaceID, role, instance, reason)
		return reason
	}
	return m.fastForward(ctx, spaceID, role, instance, inst, theirs, "run start")
}

func countInstance(runs []runRow, instance string) int {
	n := 0
	for _, r := range runs {
		if r.InstanceID == instance {
			n++
		}
	}
	return n
}

// idleReason says why an instance is not idle (ADR-0094), or "" when every run of it on the node is running with its
// turn ended, its own idle checkpoint already ran, no operator has taken it over and no input the node delivered is
// still waiting for its turn to be reported.
func (m *memManager) idleReason(ctx context.Context, instance string) string {
	runs, err := m.a.j.runs(ctx, true)
	if err != nil {
		return ffJournalFailed
	}
	live := 0
	for _, r := range runs {
		if r.InstanceID != instance {
			continue
		}
		live++
		if r.State != proto.RunRunning {
			return ffRunNotRunning
		}
		if r.Status != "idle" && r.Status != "done" {
			return ffTurnNotEnded
		}
		m.mu.Lock()
		_, due := m.timers[colKey(r.SpaceID, r.Role)+"/"+r.InstanceID]
		m.mu.Unlock()
		if due {
			return ffCheckpointDue
		}
		if m.a.takenOver(r.RunID) {
			return ffTakenOver
		}
		m.mu.Lock()
		mark, sent := m.inputs[r.InstanceID]
		m.mu.Unlock()
		if sent && mark.runID == r.RunID && r.Turn <= mark.turn {
			return ffInputPending
		}
	}
	if live == 0 {
		return ffNoLiveRun
	}
	return ""
}

// fastForwardIdle fast-forwards the idle instances of a collection whose shared head just moved, except the instance
// that moved it. It returns each considered instance's outcome ("" for fast-forwarded).
func (m *memManager) fastForwardIdle(ctx context.Context, spaceID, role, except string) map[string]string {
	if m.git == nil || !m.ffEnabled() {
		return nil
	}
	runs, err := m.a.j.runs(ctx, true)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var insts []string
	for _, r := range runs {
		if r.SpaceID != spaceID || r.InstanceID == except || seen[r.InstanceID] || !validMemoryKey(spaceID, role, r.InstanceID) {
			continue
		}
		seen[r.InstanceID] = true
		if exists(filepath.Join(m.instance(spaceID, role, r.InstanceID), ".git")) {
			insts = append(insts, r.InstanceID)
		}
	}
	if len(insts) == 0 {
		return nil
	}
	sort.Strings(insts)
	out := map[string]string{}
	defer m.lock(colKey(spaceID, role))()
	for _, instance := range insts {
		out[instance] = m.fastForwardIdleOne(ctx, spaceID, role, instance)
	}
	return out
}

// fastForwardIdleOne is one instance of fastForwardIdle; the collection lock is held.
func (m *memManager) fastForwardIdleOne(ctx context.Context, spaceID, role, instance string) string {
	skip := func(reason string) string {
		m.ffSkipped(ctx, spaceID, role, instance, reason)
		return reason
	}
	if r := m.idleReason(ctx, instance); r != "" {
		return skip(r)
	}
	shared, inst := m.shared(spaceID, role), m.instance(spaceID, role, instance)
	theirs, reason := m.checkoutReason(ctx, shared, inst)
	if reason != "" {
		return skip(reason)
	}
	gate := m.inputGate(instance)
	if !gate.TryLock() {
		return skip(ffInputInDelivery)
	}
	defer gate.Unlock()
	if m.ffGated != nil {
		m.ffGated(instance)
	}
	// The last checks, right before the merge: a turn may have started, or a file changed, since the first ones.
	if r := m.idleReason(ctx, instance); r != "" {
		return skip(r)
	}
	if _, r := m.checkoutReason(ctx, shared, inst); r != "" {
		return skip(r)
	}
	if m.ffHook != nil {
		m.ffHook(instance)
	}
	return m.fastForward(ctx, spaceID, role, instance, inst, theirs, "idle")
}
