package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/harness"
)

// Logins a harness refreshes itself (Codex ChatGPT logins, ADR-0053) live in one folder per account on the node:
// <data>/logins/<sha256(account)[:32]>/. Every instance's config dir links to that folder's files, so a refresh by one
// run is what the next run of any instance reads. Copies would break: the provider accepts a refresh token once.
const (
	loginsDir  = "logins"
	loginSeed  = "seed.sha256" // the digest of the seed the live copy started from
	loginMain  = "auth.json"   // the file the seed is written to; the others are created empty
	loginsMode = 0o700
)

func (a *Agent) loginDir(account string) string {
	sum := sha256.Sum256([]byte(account))
	return filepath.Join(a.o.DataDir, loginsDir, hex.EncodeToString(sum[:16]))
}

// linkLogin brings an account's live login up to date with central's seed and links it into the harness config dir.
// The live copy is replaced only when the seed changed since it was written (a rotation); otherwise it is kept,
// because the harness may have refreshed it since. Without a login, links left by an earlier one are removed.
func (a *Agent) linkLogin(l *harness.Login, links map[string]string) error {
	if l == nil {
		return unlinkLogins(filepath.Join(a.o.DataDir, loginsDir), links)
	}
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	dir := a.loginDir(l.Account)
	if err := os.MkdirAll(dir, loginsMode); err != nil {
		return err
	}
	if err := os.Chmod(dir, loginsMode); err != nil {
		return err
	}
	sum := sha256.Sum256(l.Seed)
	digest := []byte(hex.EncodeToString(sum[:]) + "\n")
	cur, err := os.ReadFile(filepath.Join(dir, loginSeed)) //nolint:gosec // a fixed name in the node's own login folder
	_, live := os.Stat(filepath.Join(dir, loginMain))
	if err != nil || !bytes.Equal(cur, digest) || live != nil {
		if err := writeAtomic(filepath.Join(dir, loginMain), l.Seed); err != nil {
			return err
		}
		if err := writeAtomic(filepath.Join(dir, loginSeed), digest); err != nil {
			return err
		}
	}
	for path, name := range links {
		if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") || name == loginSeed {
			return fmt.Errorf("login file %q is not a plain name", name)
		}
		target := filepath.Join(dir, name)
		if name != loginMain {
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDONLY, 0o600) //nolint:gosec // a fixed name in the node's own login folder
			if err != nil {
				return err
			}
			_ = f.Close()
		}
		if dest, err := os.Readlink(path); err == nil && dest == target {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.Symlink(target, path); err != nil {
			return err
		}
	}
	return nil
}

// unlinkLogins removes links into the logins folder (an instance moved from a ChatGPT login to an API key). A
// regular file is the harness's own and is kept.
func unlinkLogins(root string, links map[string]string) error {
	for path := range links {
		dest, err := os.Readlink(path)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(root, dest); err == nil && !strings.HasPrefix(rel, "..") {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// writeAtomic writes a 0600 file through a temporary file and a rename, so a harness never reads half a login.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// writeSecretFiles writes credential files a harness reads from its config dir (ADR-0054): 0600, through a temporary
// file and a rename, so a link at the path (a ChatGPT login left by an earlier launch) is replaced, never followed.
func writeSecretFiles(files map[string][]byte) error {
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := writeAtomic(path, content); err != nil {
			return err
		}
	}
	return nil
}

// removeCredentialFiles removes credential files a launch must not have (harness.Launch.RemoveFiles): a link itself,
// never its target. It fails when one is still there afterwards, so the launch never starts next to it.
func removeCredentialFiles(paths []string) error {
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s is still there", filepath.Base(path))
		}
	}
	return nil
}
