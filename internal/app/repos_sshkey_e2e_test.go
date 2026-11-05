package app_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/gitsshtest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/repos"
)

// SM-REPO-005, ADR-0057 end to end: SpaceMan generates a repository's SSH key and shows only its public half; once the Git
// host accepts that key, a sync clones the repository over SSH through the node with it, against the pinned host key;
// the private key is in no stored command, audit entry or API answer; removing the key leaves the node with its own
// (here: no) Git authentication, so the next fetch fails.
func TestRepositorySSHKeyEndToEnd(t *testing.T) {
	for _, bin := range []string{"git", "ssh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " is not installed")
		}
	}
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("SSH_AUTH_SOCK", "")
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "keys", Name: "Keys", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })

	root := t.TempDir()
	bare, seed := filepath.Join(root, "api.git"), filepath.Join(root, "seed")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", bare)
	gitIn(t, root, "clone", "-q", bare, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("private api\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "checkout", "-q", "-b", "main")
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-q", "-m", "init")
	gitIn(t, seed, "push", "-q", "-u", "origin", "main")

	var accepted atomic.Value // the deploy key the host knows, once the operator added it
	addr, known := gitsshtest.ServerFunc(t, func(k ssh.PublicKey) bool {
		want, _ := accepted.Load().(string)
		return want != "" && string(ssh.MarshalAuthorizedKey(k)) == want
	})

	// Refused: a key on a repository whose URL is not SSH, and known_hosts that is not known_hosts.
	plain, err := e.c.Repos.Create(ctx, admin, sp.ID, repos.Input{Name: "plain", URL: "https://example.com/plain.git", ClonePolicy: proto.CloneManual})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Repos.GenerateSSHKey(ctx, admin, sp.ID, plain.ID, plain.Version); err == nil || !strings.Contains(err.Error(), "SSH URL") {
		t.Fatalf("key on an https repository: %v", err)
	}
	if _, err := e.c.Repos.Create(ctx, admin, sp.ID, repos.Input{Name: "bad", URL: "git@example.com:a/b.git", KnownHosts: "not a key"}); err == nil {
		t.Fatal("invalid known_hosts accepted")
	}

	r, err := e.c.Repos.Create(ctx, admin, sp.ID, repos.Input{Name: "api", URL: "ssh://git@" + addr + bare, ClonePolicy: proto.CloneOnDemand, KnownHosts: known})
	if err != nil {
		t.Fatal(err)
	}
	r, err = e.c.Repos.GenerateSSHKey(ctx, admin, sp.ID, r.ID, r.Version)
	if err != nil {
		t.Fatal(err)
	}
	if r.SSHKey == nil || !strings.HasPrefix(r.SSHKey.PublicKey, "ssh-ed25519 ") || !strings.HasPrefix(r.SSHKey.Fingerprint, "SHA256:") || r.SSHKey.Note != "" {
		t.Fatalf("generated key %+v", r.SSHKey)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(r.SSHKey.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	accepted.Store(string(ssh.MarshalAuthorizedKey(pub))) // the operator adds the deploy key on the host

	if _, err := e.c.Repos.Sync(ctx, admin, sp.ID, repos.SyncInput{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "cloned over SSH with the SpaceMan key", func() bool {
		x, _ := e.c.Repos.Get(ctx, sp.ID, r.ID)
		return x.Status != nil && x.Status.State == "present"
	})
	x, _ := e.c.Repos.Get(ctx, sp.ID, r.ID)
	if x.Status.Remote == nil || !x.Status.Remote.Reachable || x.Status.Error != "" {
		t.Fatalf("status %+v remote %+v", x.Status, x.Status.Remote)
	}

	// The private key is nowhere but sealed in its column: not in stored commands, audit details or the API.
	db := e.c.Store.ReadDB()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM node_commands WHERE payload_json LIKE '%PRIVATE KEY%' OR payload_json LIKE '%ssh_key%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stored commands with a key: %d %v", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE details_json LIKE '%PRIVATE KEY%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("audit entries with a key: %d %v", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM space_repositories WHERE CAST(ssh_key_enc AS TEXT) LIKE '%PRIVATE KEY%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the key is not sealed: %d %v", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE action = 'space_repository.ssh_key_generated'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("key generation audited %d times: %v", n, err)
	}

	// Rotation: the old key no longer works on the host until the new one is added.
	old := r.SSHKey.Fingerprint
	r, err = e.c.Repos.GenerateSSHKey(ctx, admin, sp.ID, r.ID, 0)
	if err != nil || r.SSHKey.Fingerprint == old {
		t.Fatalf("rotation: %+v %v", r.SSHKey, err)
	}

	// Without a SpaceMan key the node falls back to its own Git authentication, which it does not have here.
	if r, err = e.c.Repos.RemoveSSHKey(ctx, admin, sp.ID, r.ID, r.Version); err != nil || r.SSHKey != nil {
		t.Fatalf("remove: %+v %v", r, err)
	}
	if _, err := e.c.Repos.Sync(ctx, admin, sp.ID, repos.SyncInput{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "fetch without the key fails", func() bool {
		x, _ := e.c.Repos.Get(ctx, sp.ID, r.ID)
		return x.Status != nil && x.Status.Error != ""
	})
	if _, err := e.c.Repos.RemoveSSHKey(ctx, admin, sp.ID, r.ID, 0); err == nil {
		t.Fatal("removing a missing key succeeded")
	}
}
