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
		"modified":  func(t *testing.T, repo string) { writeFile(t, filepath.Join(repo, "a.md"), "edited\n") },
		"untracked": func(t *testing.T, repo string) { writeFile(t, filepath.Join(repo, "new.md"), "x\n") },
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
	if res := f.accept(c); res.Result != proto.KBFailed || !strings.Contains(res.Detail, held) {
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
