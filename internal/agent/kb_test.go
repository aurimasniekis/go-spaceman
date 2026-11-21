package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// kbFixture is a KB repository cloned the normal way: the primary checkout has main checked out. The session's
// branch kb/k/s1 lives in a secondary worktree (as a kb session's does).
type kbFixture struct {
	a       *Agent
	repo    string // the primary checkout
	sessDir string
	space   string
}

const kbSpace = "spc_kb"

func newKBFixture(t *testing.T) *kbFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	data := t.TempDir()
	space := filepath.Join(data, "spaces", "kb")
	bare, seed := filepath.Join(data, "remote.git"), filepath.Join(data, "seed")
	gitRun(t, data, "init", "-q", "--bare", "-b", "main", bare)
	gitRun(t, data, "clone", "-q", bare, seed)
	gitRun(t, seed, "checkout", "-q", "-b", "main")
	writeFile(t, filepath.Join(seed, "a.md"), "a\n")
	writeFile(t, filepath.Join(seed, "b.md"), "b\n")
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-q", "-m", "init")
	gitRun(t, seed, "push", "-q", "-u", "origin", "main")
	repo := filepath.Join(space, "kb", "knowledge")
	if err := os.MkdirAll(filepath.Dir(repo), 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, data, "clone", "-q", bare, repo)
	sess := filepath.Join(space, "worktrees", "kb-s1")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "kb/k/s1", sess, "main")
	a := &Agent{o: Options{Clock: clock.System{}, DataDir: data}, log: testAgent().log, leases: map[string]leaseRow{}}
	a.leases[kbSpace] = leaseRow{SpaceID: kbSpace, Generation: 1, ExpiresAt: time.Now().Add(time.Hour), Workdir: "kb"}
	return &kbFixture{a: a, repo: repo, sessDir: sess, space: space}
}

// propose commits file=content on the session's branch and returns the commit.
func (f *kbFixture) propose(t *testing.T, file, content string) string {
	t.Helper()
	writeFile(t, filepath.Join(f.sessDir, file), content)
	gitRun(t, f.sessDir, "add", ".")
	gitRun(t, f.sessDir, "commit", "-q", "-m", "propose "+file)
	return gitRun(t, f.sessDir, "rev-parse", "HEAD")
}

func (f *kbFixture) accept(commit string, validator ...string) proto.KBResult {
	return f.a.kbAccept(context.Background(), proto.Command{SpaceID: kbSpace, Generation: 1, CommandID: "cmd_1"}, proto.KBAcceptPayload{
		ProposalID: "kbp_1", Repo: "kb/knowledge", Branch: "kb/k/s1", Commit: commit, Target: "main", Validator: validator, Message: "accept",
	})
}

func (f *kbFixture) mustBeConsistent(t *testing.T, dir string) {
	t.Helper()
	if st := gitRun(t, dir, "status", "--porcelain"); st != "" {
		t.Fatalf("checkout %s is not clean: %q", dir, st)
	}
	if h, m := gitRun(t, dir, "rev-parse", "HEAD"), gitRun(t, dir, "rev-parse", "refs/heads/main"); h != m {
		t.Fatalf("HEAD %s != main %s", h, m)
	}
}

// ADR-0026: acceptance in a KB cloned the normal way (the primary checkout has the target checked out) fast-forwards
// that checkout, so its files and index match the new target.
func TestKBAcceptFastForwardsCheckedOutTarget(t *testing.T) {
	f := newKBFixture(t)
	c := f.propose(t, "c.md", "c\n")
	res := f.accept(c)
	if res.Result != proto.KBMerged || res.Merged != c {
		t.Fatalf("result %+v", res)
	}
	f.mustBeConsistent(t, f.repo)
	if b, _ := os.ReadFile(filepath.Join(f.repo, "c.md")); string(b) != "c\n" {
		t.Fatalf("checkout file stale: %q", b)
	}
}

func TestKBAcceptMergeCommitInCheckedOutTarget(t *testing.T) {
	f := newKBFixture(t)
	c := f.propose(t, "c.md", "c\n")
	// main moves on with an unrelated change, so acceptance needs a merge commit.
	writeFile(t, filepath.Join(f.repo, "b.md"), "b2\n")
	gitRun(t, f.repo, "commit", "-q", "-am", "main moves")
	res := f.accept(c)
	if res.Result != proto.KBMerged || res.Merged == "" || res.Merged == c {
		t.Fatalf("result %+v", res)
	}
	f.mustBeConsistent(t, f.repo)
	if gitRun(t, f.repo, "rev-parse", "HEAD") != res.Merged {
		t.Fatal("main is not the merge commit")
	}
	for file, want := range map[string]string{"c.md": "c\n", "b.md": "b2\n", "a.md": "a\n"} {
		if b, _ := os.ReadFile(filepath.Join(f.repo, file)); string(b) != want {
			t.Fatalf("%s = %q, want %q", file, b, want)
		}
	}
}

func TestKBAcceptRefusesDirtyCheckout(t *testing.T) {
	for name, dirty := range map[string]func(t *testing.T, repo string){
		"modified": func(t *testing.T, repo string) { writeFile(t, filepath.Join(repo, "a.md"), "edited\n") },
		"staged": func(t *testing.T, repo string) {
			writeFile(t, filepath.Join(repo, "a.md"), "edited\n")
			gitRun(t, repo, "add", "a.md")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newKBFixture(t)
			c := f.propose(t, "c.md", "c\n")
			dirty(t, f.repo)
			before, status := gitRun(t, f.repo, "rev-parse", "main"), gitRun(t, f.repo, "status", "--porcelain")
			res := f.accept(c)
			if res.Result != proto.KBFailed || !strings.Contains(res.Detail, f.repo) || !strings.Contains(res.Detail, "uncommitted") {
				t.Fatalf("result %+v", res)
			}
			if gitRun(t, f.repo, "rev-parse", "main") != before || gitRun(t, f.repo, "status", "--porcelain") != status {
				t.Fatal("a refused acceptance changed the checkout")
			}
		})
	}
}

func TestKBAcceptConflictAndValidatorLeaveCheckoutAlone(t *testing.T) {
	f := newKBFixture(t)
	c := f.propose(t, "a.md", "from session\n")
	writeFile(t, filepath.Join(f.repo, "a.md"), "from main\n")
	gitRun(t, f.repo, "commit", "-q", "-am", "main edits a")
	before := gitRun(t, f.repo, "rev-parse", "main")
	if res := f.accept(c); res.Result != proto.KBConflict || !strings.Contains(res.Detail, "a.md") {
		t.Fatalf("conflict: %+v", res)
	}
	f.mustBeConsistent(t, f.repo)

	f2 := newKBFixture(t)
	c2 := f2.propose(t, "c.md", "c\n")
	before2 := gitRun(t, f2.repo, "rev-parse", "main")
	if res := f2.accept(c2, "false"); res.Result != proto.KBFailed || !strings.Contains(res.Detail, "check failed") {
		t.Fatalf("validator: %+v", res)
	}
	if gitRun(t, f2.repo, "rev-parse", "main") != before2 || gitRun(t, f.repo, "rev-parse", "main") != before {
		t.Fatal("main moved")
	}
	f2.mustBeConsistent(t, f2.repo)
	if _, err := os.Stat(filepath.Join(f2.repo, "c.md")); err == nil {
		t.Fatal("working tree changed before the validator passed")
	}
	// A validator that passes lets the ref and the checkout move together.
	if res := f2.accept(c2, "true"); res.Result != proto.KBMerged {
		t.Fatalf("validator ok: %+v", res)
	}
	f2.mustBeConsistent(t, f2.repo)
}

// A target held by a linked worktree is not moved (the node's Git guard refuses to run in linked worktrees): the proposal
// fails, naming the worktree, and nothing changes.
func TestKBAcceptTargetHeldBySecondaryWorktree(t *testing.T) {
	f := newKBFixture(t)
	gitRun(t, f.repo, "checkout", "-q", "--detach")
	held := filepath.Join(f.space, "worktrees", "main-view")
	gitRun(t, f.repo, "worktree", "add", "-q", held, "main")
	c := f.propose(t, "c.md", "c\n")
	before := gitRun(t, f.repo, "rev-parse", "main")
	if res := f.accept(c); res.Result != proto.KBFailed || !strings.Contains(res.Detail, held) || !strings.Contains(res.Detail, "linked worktree") {
		t.Fatalf("result %+v", res)
	}
	if gitRun(t, f.repo, "rev-parse", "main") != before {
		t.Fatal("main moved")
	}
	f.mustBeConsistent(t, held)
}

// With no checkout holding the target (detached primary), the ref moves alone, as before.
func TestKBAcceptTargetNotCheckedOut(t *testing.T) {
	f := newKBFixture(t)
	gitRun(t, f.repo, "checkout", "-q", "--detach")
	c := f.propose(t, "c.md", "c\n")
	if res := f.accept(c); res.Result != proto.KBMerged || gitRun(t, f.repo, "rev-parse", "main") != c {
		t.Fatalf("result %+v", res)
	}
}

// Untracked files alone do not block an acceptance; they stay as they were.
func TestKBAcceptUntrackedFileDoesNotBlock(t *testing.T) {
	f := newKBFixture(t)
	c := f.propose(t, "c.md", "c\n")
	writeFile(t, filepath.Join(f.repo, "new.md"), "x\n")
	if res := f.accept(c); res.Result != proto.KBMerged {
		t.Fatalf("result %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(f.repo, "new.md")); string(b) != "x\n" {
		t.Fatalf("untracked file changed: %q", b)
	}
}

// A local file the new tip would overwrite (untracked or ignored) is never replaced: the acceptance fails, naming the
// checkout, and the ref does not move.
func TestKBAcceptNeverOverwritesLocalFile(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, f *kbFixture){
		"untracked": func(t *testing.T, f *kbFixture) {},
		"ignored": func(t *testing.T, f *kbFixture) {
			writeFile(t, filepath.Join(f.repo, ".git", "info", "exclude"), "c.md\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newKBFixture(t)
			c := f.propose(t, "c.md", "c\n")
			setup(t, f)
			writeFile(t, filepath.Join(f.repo, "c.md"), "mine\n")
			before := gitRun(t, f.repo, "rev-parse", "main")
			res := f.accept(c)
			if res.Result != proto.KBFailed || !strings.Contains(res.Detail, f.repo) {
				t.Fatalf("result %+v", res)
			}
			if gitRun(t, f.repo, "rev-parse", "main") != before || gitRun(t, f.repo, "rev-parse", "HEAD") != before {
				t.Fatal("ref moved")
			}
			if b, _ := os.ReadFile(filepath.Join(f.repo, "c.md")); string(b) != "mine\n" {
				t.Fatalf("local file overwritten: %q", b)
			}
		})
	}
}

func TestKBAcceptRefusesStatesInProgress(t *testing.T) {
	for _, state := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"} {
		t.Run(state, func(t *testing.T) {
			f := newKBFixture(t)
			c := f.propose(t, "c.md", "c\n")
			before := gitRun(t, f.repo, "rev-parse", "main")
			writeFile(t, filepath.Join(f.repo, ".git", state), before+"\n")
			res := f.accept(c)
			if res.Result != proto.KBFailed || !strings.Contains(res.Detail, state) || !strings.Contains(res.Detail, f.repo) {
				t.Fatalf("result %+v", res)
			}
			if gitRun(t, f.repo, "rev-parse", "main") != before {
				t.Fatal("main moved")
			}
		})
	}
}

// A rebase of the target in any worktree refuses the acceptance, including when the primary checkout looks detached
// (which used to send the proposal down the plain update-ref path).
func TestKBAcceptRefusesRebaseOnTarget(t *testing.T) {
	for _, kind := range []string{"rebase-merge", "rebase-apply"} {
		for _, where := range []string{"primary", "primary-detached", "linked"} {
			t.Run(kind+"/"+where, func(t *testing.T) {
				f := newKBFixture(t)
				c := f.propose(t, "c.md", "c\n")
				before := gitRun(t, f.repo, "rev-parse", "main")
				dir, wantPath := filepath.Join(f.repo, ".git"), f.repo
				switch where {
				case "primary-detached":
					gitRun(t, f.repo, "checkout", "-q", "--detach")
				case "linked":
					gitRun(t, f.repo, "checkout", "-q", "--detach")
					dir, wantPath = gitRun(t, f.sessDir, "rev-parse", "--absolute-git-dir"), f.sessDir
				}
				writeFile(t, filepath.Join(dir, kind, "head-name"), "refs/heads/main\n")
				res := f.accept(c)
				if res.Result != proto.KBFailed || !strings.Contains(res.Detail, "rebase") || !strings.Contains(res.Detail, wantPath) {
					t.Fatalf("result %+v", res)
				}
				if gitRun(t, f.repo, "rev-parse", "main") != before {
					t.Fatal("main moved under a rebase")
				}
			})
		}
	}
	// A rebase of another branch does not matter.
	f := newKBFixture(t)
	c := f.propose(t, "c.md", "c\n")
	writeFile(t, filepath.Join(f.repo, ".git", "rebase-merge", "head-name"), "refs/heads/other\n")
	if res := f.accept(c); res.Result != proto.KBMerged {
		t.Fatalf("result %+v", res)
	}
}

// Hooks in the KB repository never run, and merge.verifySignatures does not turn an acceptance into a failure.
func TestKBAcceptIgnoresHooksAndVerifySignatures(t *testing.T) {
	f := newKBFixture(t)
	gitRun(t, f.repo, "config", "merge.verifySignatures", "true")
	c := f.propose(t, "c.md", "c\n")
	writeFile(t, filepath.Join(f.repo, "b.md"), "b2\n")
	gitRun(t, f.repo, "commit", "-q", "-am", "main moves")
	// Installed only now, so the fixture's own commits cannot trip them.
	marker := filepath.Join(f.space, "hook-ran")
	for _, h := range []string{"post-merge", "pre-merge-commit", "post-checkout", "reference-transaction", "pre-commit"} {
		p := filepath.Join(f.repo, ".git", "hooks", h)
		writeFile(t, p, "#!/bin/sh\necho "+h+" >> "+marker+"\n")
		if err := os.Chmod(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if res := f.accept(c); res.Result != proto.KBMerged {
		t.Fatalf("result %+v", res)
	}
	f.mustBeConsistent(t, f.repo)
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("a hook ran: %s", b)
	}
}

// The target moving while the KB's check runs fails the acceptance and changes nothing.
// The target moves after the fast-forward has landed: the proposal is still reported merged, with a note naming the
// observed tips, and nothing is reset.
func TestKBAcceptMovedAfterMergeIsReportedMerged(t *testing.T) {
	f := newKBFixture(t)
	c := f.propose(t, "c.md", "c\n")
	f.a.kbAfterMerge = func() {
		gitRun(t, f.repo, "-c", "user.name=x", "-c", "user.email=x@example.test", "commit", "-q", "--allow-empty", "--no-verify", "-m", "moved")
	}
	res := f.accept(c)
	moved := gitRun(t, f.repo, "rev-parse", "HEAD")
	if res.Result != proto.KBMerged || res.Merged != c {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(res.Detail, "Nothing was reset") || !strings.Contains(res.Detail, moved) {
		t.Fatalf("detail %q lacks the note or the observed tip %s", res.Detail, moved)
	}
	if gitRun(t, f.repo, "log", "-1", "--format=%s", "main") != "moved" {
		t.Fatal("the later commit was undone")
	}
	if _, err := os.Stat(filepath.Join(f.repo, "c.md")); err != nil {
		t.Fatal("merged file missing")
	}
}

func TestKBAcceptTargetMovesDuringValidator(t *testing.T) {
	f := newKBFixture(t)
	c := f.propose(t, "c.md", "c\n")
	mover := filepath.Join(f.space, "move.sh")
	writeFile(t, mover, "#!/bin/sh\ngit -c user.name=x -c user.email=x@example.test -C "+f.repo+" commit -q --allow-empty --no-verify -m moved\n")
	if err := os.Chmod(mover, 0o700); err != nil {
		t.Fatal(err)
	}
	res := f.accept(c, mover)
	if res.Result != proto.KBFailed || !strings.Contains(res.Detail, "moved while the proposal was checked") {
		t.Fatalf("result %+v", res)
	}
	if gitRun(t, f.repo, "log", "-1", "--format=%s", "main") != "moved" {
		t.Fatal("main was changed by the acceptance")
	}
	if _, err := os.Stat(filepath.Join(f.repo, "c.md")); err == nil {
		t.Fatal("working tree changed")
	}
}
