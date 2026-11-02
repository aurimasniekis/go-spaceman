package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// kbAccept merges a KB proposal on the node (ADR-0026): the commit must be on the session's branch; the target moves by
// fast-forward or a clean merge made without touching any checkout, after the KB's validator passed on the result, and
// only if the target still points where it did. A conflict or a failed check leaves everything as it was.
func (a *Agent) kbAccept(ctx context.Context, c proto.Command, p proto.KBAcceptPayload) proto.KBResult {
	res := proto.KBResult{CommandID: c.CommandID, ProposalID: p.ProposalID, Result: proto.KBFailed}
	fail := func(format string, args ...any) proto.KBResult {
		res.Detail = fmt.Sprintf(format, args...)
		return res
	}
	g, err := memory.NewGit()
	if err != nil {
		return fail("git is not installed on this node")
	}
	l, ok := a.lease(c.SpaceID, c.Generation)
	if !ok {
		return fail("no valid lease for this space")
	}
	if !memory.ValidCommit(p.Commit) || !validRef(p.Branch) || !validRef(p.Target) || p.Repo == "" {
		return fail("invalid acceptance request")
	}
	space := a.spaceDir(l.Workdir)
	repo := filepath.Join(space, filepath.Clean("/"+p.Repo))
	if _, err := g.Run(ctx, repo, nil, "rev-parse", "--git-dir"); err != nil {
		return fail("%s is not a git repository", p.Repo)
	}
	if !g.IsAncestor(ctx, repo, p.Commit, "refs/heads/"+p.Branch) {
		return fail("commit %s is not on the session's branch %s", p.Commit, p.Branch)
	}
	// Moving a branch that a worktree has checked out would leave that checkout inconsistent.
	if list, err := g.Run(ctx, repo, nil, "worktree", "list", "--porcelain"); err == nil && strings.Contains(list, "branch refs/heads/"+p.Target+"\n") {
		return fail("the base branch %s is checked out in a worktree; SpaceMan does not move a checked-out branch", p.Target)
	}
	old := g.RevParse(ctx, repo, "refs/heads/"+p.Target)
	if old == "" {
		return fail("the base branch %s does not exist", p.Target)
	}
	next := p.Commit
	id := &memory.Identity{Name: "SpaceMan KB acceptance", Email: "kb@spaceman.invalid"}
	if !g.IsAncestor(ctx, repo, old, p.Commit) {
		tree, err := g.Run(ctx, repo, nil, "merge-tree", "--write-tree", "--name-only", "--messages", old, p.Commit)
		if err != nil {
			res.Result = proto.KBConflict
			return fail("the proposal conflicts with %s: %s", p.Target, conflictFiles(tree))
		}
		t := strings.TrimSpace(strings.SplitN(tree, "\n", 2)[0])
		out, err := g.Run(ctx, repo, id, "commit-tree", t, "-p", old, "-p", p.Commit, "-m", p.Message)
		if err != nil {
			return fail("merge commit: %v", err)
		}
		next = strings.TrimSpace(out)
	}
	if len(p.Validator) > 0 {
		if msg, err := a.kbValidate(ctx, g, repo, next, p); err != nil {
			return fail("the KB's check failed: %v\n%s", err, msg)
		}
	}
	if _, err := g.Run(ctx, repo, nil, "update-ref", "refs/heads/"+p.Target, next, old); err != nil {
		return fail("%s moved while the proposal was checked; accept it again", p.Target)
	}
	res.Result, res.Merged = proto.KBMerged, next
	return res
}

// conflictFiles extracts the conflicting paths from merge-tree output (the tree line, then file names, then messages).
func conflictFiles(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var files []string
	for _, l := range lines[min(1, len(lines)):] {
		if l == "" {
			break
		}
		files = append(files, l)
	}
	if len(files) == 0 {
		return "see the session's branch"
	}
	return strings.Join(files, ", ")
}

func validRef(s string) bool {
	return s != "" && len(s) <= 200 && !strings.Contains(s, "..") && !strings.HasPrefix(s, "-") && !strings.ContainsAny(s, " ~^:?*[\\\x00")
}

// kbValidate runs the KB's validator in a temporary worktree of the candidate commit, with a bounded time and a
// minimal environment, and removes the worktree afterwards.
func (a *Agent) kbValidate(ctx context.Context, g *memory.Git, repo, commit string, p proto.KBAcceptPayload) (string, error) {
	tmp, err := os.MkdirTemp(filepath.Join(a.o.DataDir), "kb-check-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	dir := filepath.Join(tmp, "tree")
	if _, err := g.Run(ctx, repo, nil, "worktree", "add", "--quiet", "--detach", dir, commit); err != nil {
		return "", err
	}
	defer func() {
		_, _ = g.Run(context.WithoutCancel(ctx), repo, nil, "worktree", "remove", "--force", dir)
		_, _ = g.Run(context.WithoutCancel(ctx), repo, nil, "worktree", "prune")
	}()
	timeout := time.Duration(p.Timeout) * time.Second
	if timeout <= 0 || timeout > 10*time.Minute {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, p.Validator[0], p.Validator[1:]...) //nolint:gosec // the KB's validator, configured by its administrators (ADR-0026)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tmp, "LANG=C.UTF-8", "SPACEMAN_KB_CHECK=1"}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	msg := out.String()
	if len(msg) > 1500 {
		msg = "…" + msg[len(msg)-1500:]
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return msg, fmt.Errorf("timed out after %s", timeout)
	}
	return msg, err
}

// handleKBAccept runs an acceptance in the background and reports its result; the result is journaled so it can be
// reported again after a reconnect.
func (a *Agent) handleKBAccept(ctx context.Context, c proto.Command) {
	var p proto.KBAcceptPayload
	if err := json.Unmarshal(c.Payload, &p); err != nil {
		a.ack(ctx, c, proto.AckRejected, "bad kb.accept payload")
		return
	}
	a.ack(ctx, c, proto.AckAccepted, "")
	go func() {
		ctx := context.WithoutCancel(ctx)
		res := a.kbAccept(ctx, c, p)
		b, _ := json.Marshal(res)
		a.j.commandDone(ctx, c.CommandID, "done", string(b))
		_ = a.send(ctx, proto.TypeKBResult, res)
	}()
}

// resendKBResults reports recent acceptance results again (central ignores those it already has).
func (a *Agent) resendKBResults(ctx context.Context) {
	rows, err := a.j.s.ReadDB().QueryContext(ctx, `SELECT reason FROM commands WHERE kind = ? AND state = 'done' AND updated_at > ?`,
		proto.CmdKBAccept, a.now().Add(-24*time.Hour).UnixMicro())
	if err != nil {
		return
	}
	defer rows.Close()
	var results []proto.KBResult
	for rows.Next() {
		var reason string
		var r proto.KBResult
		if rows.Scan(&reason) == nil && json.Unmarshal([]byte(reason), &r) == nil && r.ProposalID != "" {
			results = append(results, r)
		}
	}
	if rows.Err() != nil {
		return
	}
	for _, r := range results {
		_ = a.send(ctx, proto.TypeKBResult, r)
	}
}
