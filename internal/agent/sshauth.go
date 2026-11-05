package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// repoAuth serves a repository's SpaceMan SSH key to the git commands of one clone or fetch (ADR-0057). The key stays
// in this process's memory: it is offered through a private ssh-agent socket (signing only; the agent protocol never
// returns a private key) in a fresh 0700 folder, and the folder is removed when the operation ends. ssh is told to use
// only that agent, no configuration file and only the pinned host keys. Agents run as the same Unix user, so one
// could ask the socket to sign while it is open; it can never read the key, and a read-only deploy key gives no more
// than the checkout it already has.
type repoAuth struct {
	dir    string
	ln     net.Listener
	wg     sync.WaitGroup
	env    []string
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// newRepoAuth prepares the key of r; it returns nil (no extra environment) when r has none, so the node's own Git
// authentication applies (ADR-0044).
func newRepoAuth(ctx context.Context, r proto.RepoSource) (*repoAuth, error) {
	if r.SSHKey == "" {
		return nil, nil
	}
	if !proto.IsSSHURL(r.URL) {
		return nil, fmt.Errorf("repository %s has a SpaceMan SSH key but its URL is not an SSH URL (git@host:owner/repo.git or ssh://)", r.Name)
	}
	key, err := ssh.ParseRawPrivateKey([]byte(r.SSHKey))
	if err != nil {
		return nil, errors.New("the repository's SSH key cannot be read")
	}
	kr := agent.NewKeyring()
	if err := kr.Add(agent.AddedKey{PrivateKey: key, Comment: "spaceman " + r.ID}); err != nil {
		return nil, err
	}
	known := strings.TrimSpace(r.KnownHosts)
	if known == "" {
		known = proto.BuiltinKnownHosts
	}
	dir, err := os.MkdirTemp("", "smk-")
	if err != nil {
		return nil, err
	}
	a := &repoAuth{dir: dir, conns: map[net.Conn]struct{}{}}
	kh := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(kh, []byte(strings.TrimSpace(known)+"\n"), 0o600); err != nil {
		a.Close()
		return nil, err
	}
	sock := filepath.Join(dir, "agent")
	if a.ln, err = (&net.ListenConfig{}).Listen(ctx, "unix", sock); err != nil {
		a.Close()
		return nil, err
	}
	a.wg.Go(func() {
		for {
			c, err := a.ln.Accept()
			if err != nil {
				return
			}
			a.mu.Lock()
			if a.closed {
				a.mu.Unlock()
				_ = c.Close()
				return
			}
			a.conns[c] = struct{}{}
			a.mu.Unlock()
			a.wg.Go(func() {
				defer func() {
					_ = c.Close()
					a.mu.Lock()
					delete(a.conns, c)
					a.mu.Unlock()
				}()
				_ = agent.ServeAgent(kr, c)
			})
		}
	})
	a.env = []string{"GIT_SSH_VARIANT=ssh", "GIT_SSH_COMMAND=" + strings.Join([]string{"ssh", "-F", "/dev/null",
		"-o", "IdentityAgent=" + shellQuote(sock), "-o", "UserKnownHostsFile=" + shellQuote(kh), "-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "StrictHostKeyChecking=yes", "-o", "BatchMode=yes", "-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no"}, " ")}
	return a, nil
}

// Env is the extra git environment (nil for a repository without a SpaceMan key).
func (a *repoAuth) Env() []string {
	if a == nil {
		return nil
	}
	return a.env
}

// Close stops serving the key and removes the folder.
func (a *repoAuth) Close() {
	if a == nil {
		return
	}
	if a.ln != nil {
		_ = a.ln.Close()
	}
	a.mu.Lock()
	a.closed = true
	for c := range a.conns { // a client still connected when the operation ended is cut off
		_ = c.Close()
	}
	a.mu.Unlock()
	a.wg.Wait()
	_ = os.RemoveAll(a.dir)
}

// sshError turns ssh's host-key failures into one line an operator can act on; other errors are returned as they are.
func sshError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		return errors.New("the Git host's SSH key does not match the pinned known_hosts; check the host's published keys and the repository's known_hosts")
	case strings.Contains(msg, "Host key verification failed"):
		return errors.New("host key verification failed: the Git host is not in the pinned known_hosts; add its keys to the repository's known_hosts")
	}
	return err
}

// shellQuote quotes s for the shell git runs GIT_SSH_COMMAND with.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
