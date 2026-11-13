package dist

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// manifest is the part of <v>/manifest.json this package reads.
type manifest struct {
	Version   string `json:"version"`
	Platforms map[string]struct {
		Binary   string `json:"binary"`
		Checksum string `json:"checksum"`
		Size     int64  `json:"size"`
	} `json:"platforms"`
}

// Ensure returns version's verified cached install, downloading it first when it is missing. Concurrent calls for
// one version share a single download (and a file lock keeps a second agent on the same directory out). The download
// runs on the manager's own context bounded by the download timeout, so a caller that gives up doesn't abort it for
// the others.
func (m *Manager) Ensure(ctx context.Context, version string) (Installed, error) {
	if !ValidVersion(version) {
		return Installed{}, fmt.Errorf("%w: %q", ErrInvalidVersion, version)
	}
	if inst, ok := m.lookup(version); ok {
		return inst, nil
	}
	c := m.start(version) //nolint:contextcheck // the shared install runs on the manager's context, not one caller's
	select {
	case <-c.done:
		return c.inst, c.err
	case <-ctx.Done():
		return Installed{}, ctx.Err()
	}
}

// Prefetch starts installing version in the background unless it is cached or already being installed.
func (m *Manager) Prefetch(version string) {
	if !ValidVersion(version) {
		return
	}
	if _, ok := m.lookup(version); ok {
		return
	}
	m.start(version)
}

// start joins or starts the single in-process install of version.
func (m *Manager) start(version string) *call {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.inflight[version]; ok {
		return c
	}
	c := &call{done: make(chan struct{})}
	if err := m.ctx.Err(); err != nil { // closed
		c.err = err
		close(c.done)
		return c
	}
	m.inflight[version] = c
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, m.timeout)
		c.inst, c.err = m.install(ctx, version)
		cancel()
		if c.err != nil {
			m.log.Warn("claude code install failed", slog.String("version", version), slog.String("error", c.err.Error()))
		} else {
			m.log.Info("claude code installed", slog.String("version", version), slog.Int64("size", c.inst.Size))
		}
		m.mu.Lock()
		delete(m.inflight, version)
		m.mu.Unlock()
		close(c.done)
	}()
	return c
}

func (m *Manager) install(ctx context.Context, version string) (Installed, error) {
	unlock, err := lockFile(ctx, m.lockPath(version))
	if err != nil {
		return Installed{}, err
	}
	defer unlock()
	if inst, ok := m.lookup(version); ok {
		return inst, nil // another agent on this directory installed it meanwhile
	}
	// Whatever sits at <v> now failed lookup (damaged, tampered with, not a real directory): remove it, or the rename
	// below would fail on every attempt and a channel target would download again and again.
	if _, err := os.Lstat(m.versionDir(version)); err == nil {
		if err := m.remove(version); err != nil {
			return Installed{}, fmt.Errorf("claude code %s: remove the unusable cached copy: %w", version, err)
		}
		m.log.Warn("claude code: removed an unusable cached version before reinstalling it", slog.String("version", version))
	}

	raw, err := m.get(ctx, m.base.JoinPath(version, "manifest.json"), maxManifestBytes)
	if err != nil {
		return Installed{}, fmt.Errorf("claude code %s: manifest: %w", version, err)
	}
	var mf manifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		return Installed{}, fmt.Errorf("%w: %s: manifest is not JSON", ErrVerify, version)
	}
	if mf.Version != version {
		return Installed{}, fmt.Errorf("%w: manifest is for version %q, want %s", ErrVerify, mf.Version, version)
	}
	p, ok := mf.Platforms[m.platform]
	switch {
	case !ok:
		return Installed{}, fmt.Errorf("%w: %s has no %s build", ErrNotFound, version, m.platform)
	case p.Binary != BinaryName:
		return Installed{}, fmt.Errorf("%w: %s: unexpected binary name in manifest", ErrVerify, version)
	case !sha256Re.MatchString(p.Checksum):
		return Installed{}, fmt.Errorf("%w: %s: manifest checksum is not a SHA-256", ErrVerify, version)
	case p.Size <= 0 || p.Size > maxBinarySize:
		return Installed{}, fmt.Errorf("%w: %s: manifest size %d out of range", ErrVerify, version, p.Size)
	}
	sum := sha256.Sum256(raw)
	inst := Installed{
		Version: version, Platform: m.platform, SHA256: p.Checksum, Size: p.Size,
		ManifestSHA256: hex.EncodeToString(sum[:]), FetchedAt: m.now().UTC(),
	}

	tmp, err := os.MkdirTemp(m.dir, version+".tmp-")
	if err != nil {
		return Installed{}, err
	}
	defer func() { _ = removeTree(tmp) }() // a no-op once renamed into place
	if err := m.download(ctx, tmp, inst); err != nil {
		return Installed{}, err
	}
	b, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return Installed{}, err
	}
	if err := writeReadOnly(filepath.Join(tmp, manifestFile), raw); err != nil {
		return Installed{}, err
	}
	if err := writeReadOnly(filepath.Join(tmp, verifiedFile), append(b, '\n')); err != nil {
		return Installed{}, err
	}
	if err := syncDir(tmp); err != nil {
		return Installed{}, err
	}
	// Read-only from here on: this guards against accidental changes only, since runs share the node's uid (whoever
	// can write as that user can chmod it back).
	if err := os.Chmod(tmp, 0o500); err != nil { //nolint:gosec // G302: a directory needs x to be read
		return Installed{}, err
	}
	if err := os.Rename(tmp, m.versionDir(version)); err != nil {
		return Installed{}, fmt.Errorf("claude code %s: install: %w", version, err)
	}
	if err := syncDir(m.dir); err != nil {
		return Installed{}, err
	}
	inst.Path = m.binaryPath(version)
	return inst, nil
}

// download streams the binary into dir through SHA-256 with a hard cap of the manifest size. The file stays 0600 (not
// executable) until size and checksum match, then becomes 0555 (read-only).
func (m *Manager) download(ctx context.Context, dir string, inst Installed) error {
	resp, err := m.do(ctx, m.base.JoinPath(inst.Version, m.platform, BinaryName))
	if err != nil {
		return fmt.Errorf("claude code %s: download: %w", inst.Version, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ContentLength > inst.Size {
		return fmt.Errorf("%w: %s: the server announces %d bytes, the manifest %d", ErrVerify, inst.Version, resp.ContentLength, inst.Size)
	}
	path := filepath.Join(dir, BinaryName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // inside our temp dir
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, inst.Size+1))
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("claude code %s: download: %w", inst.Version, err)
	}
	switch {
	case n > inst.Size:
		_ = f.Close()
		return fmt.Errorf("%w: %s: the binary is larger than the manifest's %d bytes", ErrVerify, inst.Version, inst.Size)
	case n < inst.Size:
		_ = f.Close()
		return fmt.Errorf("%w: %s: the binary is %d bytes, the manifest says %d", ErrVerify, inst.Version, n, inst.Size)
	case hex.EncodeToString(h.Sum(nil)) != inst.SHA256:
		_ = f.Close()
		return fmt.Errorf("%w: %s: SHA-256 does not match the manifest", ErrVerify, inst.Version)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o555) //nolint:gosec // G302: the harness binary must be executable
}

// lookup is the cheap check that version is installed (see check).
func (m *Manager) lookup(version string) (Installed, bool) {
	inst, err := m.check(version)
	return inst, err == nil
}

// check opens <dir>/<version> and checks it cheaply: a real directory (no link followed), a regular verified.json
// for this version and platform, and an executable regular binary of the recorded size. Verify also re-hashes.
func (m *Manager) check(version string) (Installed, error) {
	if !ValidVersion(version) {
		return Installed{}, fmt.Errorf("%w: %q", ErrInvalidVersion, version)
	}
	vd, err := m.openVersion(version)
	if err != nil {
		return Installed{}, err
	}
	defer func() { _ = vd.Close() }()
	inst, err := readVerified(vd, version, m.platform)
	if err != nil {
		return Installed{}, err
	}
	fi, err := vd.Lstat(BinaryName)
	switch {
	case err != nil:
		return Installed{}, err
	case !fi.Mode().IsRegular():
		return Installed{}, fmt.Errorf("%w: %s: the binary is not a regular file", ErrVerify, version)
	case fi.Size() != inst.Size:
		return Installed{}, fmt.Errorf("%w: %s: the binary is %d bytes, verified.json says %d", ErrVerify, version, fi.Size(), inst.Size)
	case fi.Mode().Perm()&0o111 == 0:
		return Installed{}, fmt.Errorf("%w: %s: the binary is not executable", ErrVerify, version)
	}
	inst.Path = m.binaryPath(version)
	return inst, nil
}

// openVersion opens <dir>/<version> as a handle, refusing a link or a swap (as the agent's openDir does): the name is
// opened first, then must be a real directory and the very one that was opened. Files are then read relative to the
// handle, so a link planted inside or a directory swapped afterwards is never followed by path.
func (m *Manager) openVersion(version string) (*os.Root, error) {
	parent, err := os.OpenRoot(m.dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	child, err := parent.OpenRoot(version)
	if err != nil {
		return nil, err
	}
	li, err := parent.Lstat(version)
	if err == nil && !li.IsDir() {
		err = fmt.Errorf("%w: %s is not a directory", ErrVerify, version)
	}
	if err == nil {
		var ci fs.FileInfo
		if ci, err = child.Stat("."); err == nil && !os.SameFile(li, ci) {
			err = fmt.Errorf("%w: %s changed while it was opened", ErrVerify, version)
		}
	}
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	return child, nil
}

// openRegular opens name inside root only if it is a regular file (not a link) and the very file that was checked.
func openRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	li, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%w: %s is not a regular file", ErrVerify, name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err == nil && !os.SameFile(li, fi) {
		err = fmt.Errorf("%w: %s changed while it was opened", ErrVerify, name)
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, fi, nil
}

// readRegular reads a small regular file inside root (at most limit bytes).
func readRegular(root *os.Root, name string, limit int64) ([]byte, error) {
	f, _, err := openRegular(root, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%w: %s is larger than %d bytes", ErrVerify, name, limit)
	}
	return b, err
}

// readVerified reads and checks a version directory's verified.json.
func readVerified(vd *os.Root, version, platform string) (Installed, error) {
	b, err := readRegular(vd, verifiedFile, maxManifestBytes)
	if err != nil {
		return Installed{}, err
	}
	var inst Installed
	if json.Unmarshal(b, &inst) != nil || inst.Version != version || inst.Platform != platform ||
		!sha256Re.MatchString(inst.SHA256) || !sha256Re.MatchString(inst.ManifestSHA256) {
		return Installed{}, fmt.Errorf("%w: %s: verified.json does not match", ErrVerify, version)
	}
	return inst, nil
}

// Verify re-checks the size and SHA-256 of each cached version against its verified.json (at startup, for the
// versions the node will use). A damaged version is removed so the next Ensure downloads it again. Versions that are
// not cached are skipped. It returns the problems found, keyed by version.
func (m *Manager) Verify(ctx context.Context, versions ...string) map[string]error {
	out := map[string]error{}
	for _, v := range versions {
		if !ValidVersion(v) {
			continue
		}
		if _, err := os.Lstat(m.versionDir(v)); err != nil {
			continue // not cached (a link at the name is checked, and removed, like a damaged version)
		}
		if err := m.verifyOne(ctx, v); err != nil {
			out[v] = err
			m.log.Warn("claude code cached version failed verification; removing it", slog.String("version", v),
				slog.String("error", err.Error()))
			if rerr := m.removeLocked(ctx, v); rerr != nil {
				m.log.Warn("claude code: could not remove a damaged version", slog.String("version", v),
					slog.String("error", rerr.Error()))
			}
		}
	}
	return out
}

func (m *Manager) verifyOne(ctx context.Context, version string) error {
	inst, err := m.check(version)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrVerify, version, err)
	}
	vd, err := m.openVersion(version)
	if err != nil {
		return err
	}
	defer func() { _ = vd.Close() }()
	// The release manifest kept beside the binary must still be the one verified.json was derived from.
	raw, err := readRegular(vd, manifestFile, maxManifestBytes)
	if err != nil {
		return fmt.Errorf("%w: %s: manifest.json: %w", ErrVerify, version, err)
	}
	msum := sha256.Sum256(raw)
	var mf manifest
	if hex.EncodeToString(msum[:]) != inst.ManifestSHA256 || json.Unmarshal(raw, &mf) != nil || mf.Version != version ||
		mf.Platforms[m.platform].Checksum != inst.SHA256 || mf.Platforms[m.platform].Size != inst.Size {
		return fmt.Errorf("%w: %s: manifest.json does not match verified.json", ErrVerify, version)
	}
	f, fi, err := openRegular(vd, BinaryName)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if fi.Size() != inst.Size {
		return fmt.Errorf("%w: %s: the cached binary's size changed", ErrVerify, version)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(&ctxReader{ctx: ctx, r: f}, inst.Size+1))
	if err != nil {
		return err
	}
	if n != inst.Size || hex.EncodeToString(h.Sum(nil)) != inst.SHA256 {
		return fmt.Errorf("%w: %s: the cached binary's size or SHA-256 changed", ErrVerify, version)
	}
	return nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func (m *Manager) versionDir(v string) string { return filepath.Join(m.dir, v) }
func (m *Manager) binaryPath(v string) string { return filepath.Join(m.dir, v, BinaryName) }
func (m *Manager) lockPath(v string) string   { return filepath.Join(m.dir, "."+v+".lock") }

// writeReadOnly creates path (0400) with b and syncs it.
func writeReadOnly(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400) //nolint:gosec // inside our temp dir
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// writeFileAtomic writes v as JSON (0600) through a synced temporary file and a rename.
func writeFileAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	name := f.Name()
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // our own cache directory
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, os.ErrInvalid) {
		return nil // some platforms can't sync a directory
	}
	return err
}

func randSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
