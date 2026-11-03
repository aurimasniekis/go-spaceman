package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// File helpers derived from go-hallpass internal/backup/files.go at bdd8329 (Apache-2.0, same author).

// hashingWriter counts and hashes what passes through it.
type hashingWriter struct {
	h hash.Hash
	n int64
}

func newHashingWriter() *hashingWriter { return &hashingWriter{h: sha256.New()} }

func (w *hashingWriter) Write(p []byte) (int, error) {
	w.h.Write(p)
	w.n += int64(len(p))
	return len(p), nil
}

func (w *hashingWriter) sum() string { return hex.EncodeToString(w.h.Sum(nil)) }

// sealFile seals the regular file src into the new file dst (mode 0600, parents 0700) and fsyncs it. It returns the
// plaintext size and SHA-256 and the sealed size and SHA-256.
func sealFile(src, dst string, kek keyring.KeyWrapper, kind, backupID string) (plain, sealed sizeSum, err error) {
	in, err := openRegular(src)
	if err != nil {
		return plain, sealed, err
	}
	defer in.Close()
	return sealReader(in, dst, kek, kind, backupID)
}

type sizeSum struct {
	size int64
	sum  string
}

func sealReader(in io.Reader, dst string, kek keyring.KeyWrapper, kind, backupID string) (plain, sealed sizeSum, err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return plain, sealed, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // destination inside our own staging directory
	if err != nil {
		return plain, sealed, err
	}
	ph, sh := newHashingWriter(), newHashingWriter()
	err = Seal(io.MultiWriter(out, sh), io.TeeReader(in, ph), kek, kind, backupID)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		return plain, sealed, err
	}
	return sizeSum{ph.n, ph.sum()}, sizeSum{sh.n, sh.sum()}, nil
}

// openFile decrypts the sealed file src into the new file dst (mode 0600, parents 0700), checks the plaintext against
// want and fsyncs it. A mismatch removes dst.
func openFile(src, dst string, kek keyring.KeyWrapper, kind, backupID string, want sizeSum) error {
	in, err := openRegular(src)
	if err != nil {
		return err
	}
	defer in.Close()
	r, _, err := OpenSealed(in, kek, kind, backupID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // destination inside our own staging directory
	if err != nil {
		return err
	}
	h := newHashingWriter()
	_, err = io.Copy(io.MultiWriter(out, h), io.LimitReader(r, want.size+1))
	if err == nil {
		// The sealed stream must end exactly here (no truncation, nothing appended).
		var one [1]byte
		if n, rerr := r.Read(one[:]); n > 0 || !errors.Is(rerr, io.EOF) {
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				err = rerr
			} else {
				err = fmt.Errorf("%w: %s is longer than the manifest says", ErrInvalid, kind)
			}
		}
	}
	if err == nil && (h.n != want.size || h.sum() != want.sum) {
		err = fmt.Errorf("%w: %s does not match the manifest (size or SHA-256 differs)", ErrInvalid, kind)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
	}
	return err
}

// readSealed decrypts a sealed file and returns its plaintext checked against want, without writing it anywhere.
func readSealed(src string, kek keyring.KeyWrapper, kind, backupID string, want sizeSum) error {
	in, err := openRegular(src)
	if err != nil {
		return err
	}
	defer in.Close()
	r, _, err := OpenSealed(in, kek, kind, backupID)
	if err != nil {
		return err
	}
	h := newHashingWriter()
	if _, err := io.Copy(h, io.LimitReader(r, want.size+1)); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	if h.n != want.size || h.sum() != want.sum {
		return fmt.Errorf("%w: %s does not match the manifest (size or SHA-256 differs)", ErrInvalid, kind)
	}
	return nil
}

// hashFile returns the size and SHA-256 of a regular file.
func hashFile(p string) (sizeSum, error) {
	f, err := openRegular(p)
	if err != nil {
		return sizeSum{}, err
	}
	defer f.Close()
	h := newHashingWriter()
	if _, err := io.Copy(h, f); err != nil {
		return sizeSum{}, err
	}
	return sizeSum{h.n, h.sum()}, nil
}

// openRegular opens p for reading and refuses anything but a regular file (no symlinks, devices or FIFOs).
func openRegular(p string) (*os.File, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	f, err := os.Open(p) //nolint:gosec // paths come from our own walks or a validated manifest
	if err != nil {
		return nil, err
	}
	if fi2, err := f.Stat(); err != nil || !os.SameFile(fi, fi2) {
		_ = f.Close()
		return nil, fmt.Errorf("%s changed while opening", p)
	}
	return f, nil
}

func writeFileSync(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // our own staging directory
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func readSmall(p string, limit int64) ([]byte, error) {
	f, err := openRegular(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", p, limit)
	}
	return b, nil
}

// fsyncTree fsyncs every directory under root so that all entries are durable (files are fsynced when written).
func fsyncTree(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return store.FsyncDir(p)
		}
		return nil
	})
}

func emptyDir(p string) bool {
	entries, err := os.ReadDir(p)
	return err == nil && len(entries) == 0
}
