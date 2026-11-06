package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/aurimasniekis/go-spaceman/internal/gitsshtest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// SM-REPO-005, ADR-0057: a repository with a SpaceMan SSH key is cloned and fetched over SSH with that key only, served
// from an in-memory agent, against the pinned host key; a wrong host key or a key the host does not know fails; the
// key's folder is gone afterwards and nothing of it is on disk.
func TestRepoSSHKeyCloneAndFetch(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh is not installed")
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("SSH_AUTH_SOCK", "") // the node's own agent is never used
	space, bare, seed := reposFixture(t)
	key, pub := gitsshtest.NewKey(t)
	addr, known := gitsshtest.Server(t, pub)
	url := "ssh://git@" + addr + bare
	ctx := context.Background()
	a := testAgent()

	src := proto.RepoSource{ID: "rep_api", Name: "api", Path: "api", URL: url, DefaultBranch: "main", ClonePolicy: proto.CloneAuto, SSHKey: key, KnownHosts: known}
	rep := a.syncRepos(ctx, "spc_1", space, proto.ReposSyncPayload{Repositories: []proto.RepoSource{src}, Clone: []string{"rep_api"}})
	if st := rep.Repositories[0]; !st.Cloned || st.Error != "" || st.Remote == nil || !st.Remote.Reachable {
		t.Fatalf("clone with the key: %+v remote %+v", st, st.Remote)
	}
	if b, err := os.ReadFile(filepath.Join(space, "api", "README.md")); err != nil || string(b) != "api\n" {
		t.Fatalf("cloned content %q %v", b, err)
	}
	// A new commit upstream arrives with a fetch through the same key.
	writeFile(t, filepath.Join(seed, "NEW.md"), "new\n")
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-q", "-m", "second")
	gitRun(t, seed, "push", "-q", "origin", "main")
	head := strings.TrimSpace(gitRun(t, seed, "rev-parse", "HEAD"))
	rep = a.syncRepos(ctx, "spc_1", space, proto.ReposSyncPayload{Repositories: []proto.RepoSource{src}, Fetch: []string{"rep_api"}})
	if st := rep.Repositories[0]; !st.Fetched || st.Tracking != head {
		t.Fatalf("fetch with the key: %+v", st)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "smk-*")); len(left) != 0 {
		t.Fatalf("key folders left behind: %v", left)
	}
	walk(t, space, func(p string, b []byte) {
		if strings.Contains(string(b), "PRIVATE KEY") {
			t.Fatalf("the private key is on disk in %s", p)
		}
	})

	// A different host key (a man in the middle) and a key the host does not know both fail.
	_, otherKnown := gitsshtest.Server(t, pub)
	bad := src
	bad.Path, bad.ID = "api2", "rep_api2"
	bad.KnownHosts = knownhosts.Normalize(addr) + " " + strings.SplitN(otherKnown, " ", 2)[1]
	rep = a.syncRepos(ctx, "spc_1", space, proto.ReposSyncPayload{Repositories: []proto.RepoSource{bad}, Clone: []string{"rep_api2"}})
	if st := rep.Repositories[0]; st.Cloned || !strings.Contains(st.Error, "does not match the pinned known_hosts") {
		t.Fatalf("wrong host key: %+v", st)
	}
	other, _ := gitsshtest.NewKey(t)
	bad = src
	bad.Path, bad.ID, bad.SSHKey = "api3", "rep_api3", other
	rep = a.syncRepos(ctx, "spc_1", space, proto.ReposSyncPayload{Repositories: []proto.RepoSource{bad}, Clone: []string{"rep_api3"}})
	if st := rep.Repositories[0]; st.Cloned || st.Error == "" {
		t.Fatalf("unknown key: %+v", st)
	}
	// A key on an https:// URL is refused instead of being ignored.
	bad = src
	bad.Path, bad.ID, bad.URL = "api4", "rep_api4", "https://example.com/api.git"
	rep = a.syncRepos(ctx, "spc_1", space, proto.ReposSyncPayload{Repositories: []proto.RepoSource{bad}, Clone: []string{"rep_api4"}})
	if st := rep.Repositories[0]; st.Cloned || !strings.Contains(st.Error, "not an SSH URL") {
		t.Fatalf("key on https: %+v", st)
	}
}

// The agent socket only signs: it lists the one key, signs with it, never hands out the private key, and stops serving
// when the operation ends.
func TestRepoAuthAgentSignsOnly(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	key, pub := gitsshtest.NewKey(t)
	auth, err := newRepoAuth(context.Background(), proto.RepoSource{ID: "rep_1", Name: "api", URL: "git@github.com:acme/api.git", SSHKey: key})
	if err != nil {
		t.Fatal(err)
	}
	var sock string
	for _, e := range auth.Env() {
		if v, ok := strings.CutPrefix(e, "GIT_SSH_COMMAND="); ok {
			if !strings.Contains(v, "StrictHostKeyChecking=yes") || !strings.Contains(v, "-F /dev/null") {
				t.Fatalf("ssh command %q", v)
			}
			i := strings.Index(v, "IdentityAgent='") + len("IdentityAgent='")
			sock = v[i : i+strings.Index(v[i:], "'")]
		}
	}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(sock), "known_hosts")); err != nil || !strings.Contains(string(b), "github.com ssh-ed25519") {
		t.Fatalf("built-in known hosts: %q %v", b, err)
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.NewClient(c)
	keys, err := ag.List()
	if err != nil || len(keys) != 1 || string(keys[0].Marshal()) != string(pub.Marshal()) {
		t.Fatalf("listed keys %v %v", keys, err)
	}
	sig, err := ag.Sign(pub, []byte("data"))
	if err != nil || pub.Verify([]byte("data"), sig) != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := ag.RemoveAll(); err != nil {
		t.Log("remove all:", err) // removing keys from this connection's view changes nothing outside the operation
	}
	auth.Close() // cuts off the open connection and removes the folder
	if _, err := io.ReadAll(c); err != nil && !strings.Contains(err.Error(), "closed") {
		t.Log(err)
	}
	if _, err := net.Dial("unix", sock); err == nil {
		t.Fatal("the agent still serves after the operation")
	}
	if _, err := os.Stat(filepath.Dir(sock)); !os.IsNotExist(err) {
		t.Fatalf("key folder still there: %v", err)
	}
	if a, err := newRepoAuth(context.Background(), proto.RepoSource{}); a != nil || err != nil || a.Env() != nil {
		t.Fatal("a repository without a key uses the node's own authentication")
	}
}

func walk(t *testing.T, root string, fn func(string, []byte)) {
	t.Helper()
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() && fi.Size() < 1<<20 {
			if b, err := os.ReadFile(p); err == nil {
				fn(p, b)
			}
		}
		return nil
	})
}

// A host-key failure names what decided it: SpaceMan's pins for a repository with a SpaceMan key, the node's own SSH
// setup otherwise (with the ways out: an https URL, a SpaceMan key, or the node's known_hosts).
func TestSSHErrorNamesWhatDecided(t *testing.T) {
	failed := errors.New("git clone: Host key verification failed.\r\nfatal: Could not read from remote repository.")
	if got := sshError(failed, true).Error(); !strings.Contains(got, "pinned known_hosts") {
		t.Fatalf("keyed: %q", got)
	}
	got := sshError(failed, false).Error()
	if strings.Contains(got, "pinned known_hosts") || !strings.Contains(got, "node's own SSH setup") || !strings.Contains(got, "https://") {
		t.Fatalf("not keyed: %q", got)
	}
	changed := errors.New("@@@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @@@")
	if got := sshError(changed, false).Error(); !strings.Contains(got, "node user's known_hosts") {
		t.Fatalf("changed, not keyed: %q", got)
	}
	other := errors.New("repository not found")
	if !errors.Is(sshError(other, true), other) || sshError(nil, false) != nil {
		t.Fatal("other errors pass through")
	}
}
