package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Configured space repositories on the node (ADR-0044). Central lists a space's repositories; the node clones them into
// the space folder (on the lease, at first use or on request), fetches them on request from their configured URL and
// reports their state. Git runs as for relocation: with the node's own environment, configuration and credentials
// (SM-REPO-005), hooks and fsmonitor off, the ext transport off, and never in a repository whose own configuration would
// run a program (memory.UnsafeConfig). Nothing is ever reset, merged, pushed or deleted (SM-REPO-006); a folder that
// exists but is not a repository is never overwritten.

const (
	maxSyncRepos   = 50
	cloneTimeout   = 30 * time.Minute
	fetchTimeout   = 10 * time.Minute
	lsRemoteBudget = 30 * time.Second
)

// repoLock serializes a space's repository work (syncs and clones before launches).
func (a *Agent) repoLock(spaceID string) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.repoLocks == nil {
		a.repoLocks = map[string]*sync.Mutex{}
	}
	l := a.repoLocks[spaceID]
	if l == nil {
		l = &sync.Mutex{}
		a.repoLocks[spaceID] = l
	}
	return l
}

// validRepoPath reports whether a configured repository folder is a usable place in the space folder: relative, clean,
// and outside the folders SpaceMan and the harnesses own.
func validRepoPath(rel string) bool {
	return safeRel(rel) && !underReserved(filepath.Clean(filepath.FromSlash(rel)), "worktrees", "work", "dreams") &&
		!strings.HasPrefix(rel, ".spaceman-")
}

// handleReposSync runs a space.repos.sync command in the background and reports the state; the report is journaled
// with the command so a repeated delivery re-sends it.
func (a *Agent) handleReposSync(ctx context.Context, c proto.Command) {
	var p proto.ReposSyncPayload
	if err := json.Unmarshal(c.Payload, &p); err != nil || len(p.Repositories) > maxSyncRepos {
		a.j.commandDone(ctx, c.CommandID, "rejected", "bad space.repos.sync payload")
		a.ack(ctx, c, proto.AckRejected, "bad space.repos.sync payload")
		return
	}
	l, _ := a.lease(c.SpaceID, c.Generation)
	a.ack(ctx, c, proto.AckAccepted, "")
	a.goTrack(func() {
		ctx := context.WithoutCancel(ctx)
		rep := a.syncRepos(ctx, c.SpaceID, a.spaceDir(l.Workdir), p)
		rep.CommandID = c.CommandID
		b, _ := json.Marshal(rep)
		a.j.commandDone(ctx, c.CommandID, "done", string(b))
		a.sendRepoStatus(ctx, rep)
	})
}

// resendReposSync re-sends the stored report of a repeated space.repos.sync (central keeps the newest).
func (a *Agent) resendReposSync(ctx context.Context, c proto.Command) {
	var state, reason string
	if err := a.j.s.ReadDB().QueryRowContext(ctx, `SELECT state, reason FROM commands WHERE id = ?`, c.CommandID).Scan(&state, &reason); err != nil || state != "done" {
		return
	}
	var rep proto.RepoStatusReport
	if json.Unmarshal([]byte(reason), &rep) == nil && rep.SpaceID != "" {
		a.sendRepoStatus(ctx, rep)
	}
}

func (a *Agent) sendRepoStatus(ctx context.Context, rep proto.RepoStatusReport) {
	if !a.protocolAtLeast(proto.MinorRepositories) {
		return
	}
	if err := a.send(ctx, proto.TypeRepoStatus, rep); err != nil {
		a.log.Debug("repository report not delivered; the next sync reports again", slog.String(obs.FieldSpaceID, rep.SpaceID))
	}
}

// syncRepos clones and fetches what central asked for and inspects every configured repository.
func (a *Agent) syncRepos(ctx context.Context, spaceID, space string, p proto.ReposSyncPayload) proto.RepoStatusReport {
	l := a.repoLock(spaceID)
	l.Lock()
	defer l.Unlock()
	rep := proto.RepoStatusReport{SpaceID: spaceID, Repositories: []proto.RepoState{}}
	g, gerr := newProjectGit()
	configured := map[string]bool{}
	for _, r := range p.Repositories {
		configured[r.Path] = true
		st := proto.RepoState{ID: r.ID, Path: r.Path}
		switch {
		case gerr != nil:
			st.Error = gerr.Error()
		case !validRepoPath(r.Path):
			st.Error = "invalid repository folder"
		case !validRef(r.DefaultBranch):
			st.Error = "invalid default branch"
		default:
			a.syncRepo(ctx, g, space, r, slices.Contains(p.Clone, r.ID), slices.Contains(p.Fetch, r.ID), &st)
		}
		rep.Repositories = append(rep.Repositories, st)
	}
	if lay, err := walkSpace(space); err == nil {
		for _, rel := range lay.repos {
			if !configured[rel] && len(rep.Unlisted) < maxSyncRepos {
				rep.Unlisted = append(rep.Unlisted, rel)
			}
		}
	}
	rep.CheckedAt = a.now()
	return rep
}

func (a *Agent) syncRepo(ctx context.Context, g *projectGit, space string, r proto.RepoSource, clone, fetch bool, st *proto.RepoState) {
	dir := filepath.Join(space, filepath.FromSlash(r.Path))
	var env []string
	if r.URL != "" {
		auth, err := newRepoAuth(ctx, r)
		switch {
		case err != nil:
			st.Error = truncateStr(err.Error(), 500)
			r.URL = ""
		default:
			defer auth.Close()
			env = auth.Env()
		}
	}
	if r.URL != "" {
		if err := proto.ValidateRepoURL(r.URL, true); err != nil {
			st.Error = "url " + err.Error()
			r.URL = ""
		} else {
			st.Remote = g.remoteState(ctx, space, r.URL, r.DefaultBranch, env)
		}
	}
	present := isRepoDir(dir)
	switch {
	case !present && clone && r.URL != "" && r.ClonePolicy != proto.CloneManual:
		if err := g.cloneSource(ctx, space, r, st.Remote, env); err != nil {
			st.Error = truncateStr(err.Error(), 500)
			a.log.Warn("repository clone failed", slog.String("repository.id", r.ID), obs.Err(err))
		} else {
			present, st.Cloned = true, true
		}
	case present && fetch && r.URL != "":
		if err := g.fetchSource(ctx, dir, r, env); err != nil {
			st.Error = truncateStr(err.Error(), 500)
		} else {
			st.Fetched = true
		}
	}
	st.Present = present
	if !present {
		return
	}
	rc := g.inspectRepo(ctx, space, r.Path)
	st.Check = &rc
	if rc.Error != "" && st.Error == "" {
		st.Error = rc.Error
	}
	if rc.Error == "" {
		st.DefaultHead = g.line(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+r.DefaultBranch+"^{commit}")
		st.Tracking = g.line(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/"+g.remoteName(ctx, dir, r.URL)+"/"+r.DefaultBranch+"^{commit}")
		st.LastFetchAt = g.lastFetch(ctx, dir)
	}
}

// ensureRepo clones a launch's configured repository when its folder is missing and its policy allows it (first use).
func (a *Agent) ensureRepo(ctx context.Context, spaceID, space string, r proto.RepoSource) error {
	if !validRepoPath(r.Path) || !validRef(r.DefaultBranch) {
		return errors.New("invalid repository folder or default branch")
	}
	l := a.repoLock(spaceID)
	l.Lock()
	defer l.Unlock()
	dir := filepath.Join(space, filepath.FromSlash(r.Path))
	if isRepoDir(dir) || r.URL == "" || r.ClonePolicy == proto.CloneManual {
		return nil // present, or never cloned by SpaceMan: the worktree step reports a missing repository
	}
	if err := proto.ValidateRepoURL(r.URL, true); err != nil {
		return fmt.Errorf("repository %s: url %w", r.Name, err)
	}
	g, err := newProjectGit()
	if err != nil {
		return err
	}
	auth, err := newRepoAuth(ctx, r)
	if err != nil {
		return err
	}
	defer auth.Close()
	return g.cloneSource(ctx, space, r, g.remoteState(ctx, space, r.URL, r.DefaultBranch, auth.Env()), auth.Env())
}

// isRepoDir reports whether dir is a repository's main folder (a .git directory, not a linked worktree's .git file).
func isRepoDir(dir string) bool {
	fi, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil && fi.IsDir()
}

// remoteState asks the remote for its default branch (git ls-remote): whether it answers and where the branch points.
func (g *projectGit) remoteState(ctx context.Context, space, rawURL, branch string, env []string) *proto.RepoRemoteState {
	rs := &proto.RepoRemoteState{URL: proto.RedactRepoURL(rawURL)}
	out, err := g.runEnv(ctx, space, lsRemoteBudget, env, "ls-remote", "--", rawURL, "refs/heads/"+branch)
	if err != nil {
		rs.Error = truncateStr(sshError(err).Error(), 300)
		return rs
	}
	rs.Reachable = true
	for ln := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if sha, ref, ok := strings.Cut(ln, "\t"); ok && ref == "refs/heads/"+branch {
			rs.Head = sha
		}
	}
	if rs.Head == "" {
		rs.Error = "the remote has no branch " + branch
	}
	return rs
}

// cloneSource clones a configured repository into its folder. The configured default branch is checked out when the
// remote has it; an empty or differently organized remote is cloned as it is. The clone is then fetched once, so
// its last fetch time is known.
func (g *projectGit) cloneSource(ctx context.Context, space string, r proto.RepoSource, remote *proto.RepoRemoteState, env []string) error {
	rel := filepath.FromSlash(r.Path)
	dir := filepath.Join(space, rel)
	if err := noSymlinkParents(space, rel); err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%s exists but is not a Git repository; SpaceMan does not overwrite it", r.Path)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := noSymlinkParents(space, rel); err != nil {
		return err
	}
	args := []string{"clone", "--quiet", "--origin", "origin"}
	if remote != nil && remote.Head != "" {
		args = append(args, "--branch", r.DefaultBranch)
	}
	if _, err := g.runEnv(ctx, space, cloneTimeout, env, append(args, "--", r.URL, dir)...); err != nil {
		return fmt.Errorf("clone %s: %w", r.Path, sshError(err))
	}
	return g.fetchSource(ctx, dir, r, env)
}

// fetchSource fetches every branch from the configured URL into the remote-tracking branches of the remote that has
// that URL (origin otherwise). Local branches, the work tree and the index are untouched; nothing is pruned.
func (g *projectGit) fetchSource(ctx context.Context, dir string, r proto.RepoSource, env []string) error {
	name := g.remoteName(ctx, dir, r.URL)
	if _, err := g.runEnv(ctx, dir, fetchTimeout, env, "fetch", "--quiet", "--no-tags", "--", r.URL, "+refs/heads/*:refs/remotes/"+name+"/*"); err != nil {
		return fmt.Errorf("fetch %s: %w", r.Path, sshError(err))
	}
	return nil
}

// remoteName is the name of the repository's remote whose URL is the configured one, or origin.
func (g *projectGit) remoteName(ctx context.Context, dir, rawURL string) string {
	if rawURL != "" {
		for _, name := range strings.Fields(g.line(ctx, dir, "remote")) {
			if g.line(ctx, dir, "remote", "get-url", name) == rawURL && validRef(name) && !strings.Contains(name, "/") {
				return name
			}
		}
	}
	return "origin"
}

// lastFetch is when the repository last fetched (its FETCH_HEAD), or nil when it never did.
func (g *projectGit) lastFetch(ctx context.Context, dir string) *time.Time {
	gd := g.line(ctx, dir, "rev-parse", "--git-dir")
	if gd == "" {
		return nil
	}
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(dir, gd)
	}
	fi, err := os.Stat(filepath.Join(gd, "FETCH_HEAD"))
	if err != nil {
		return nil
	}
	t := fi.ModTime().UTC()
	return &t
}
