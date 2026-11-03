package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Local keeps artifacts in directories on central's disk, one per namespace: the key's first segment selects the
// directory and the rest is the path below it. Central's defaults are <artifacts.local_dir or data/artifacts> for
// attachments, <data>/recordings and <data>/transcripts, the layout SpaceMan used before the store existed.
type Local struct {
	roots map[string]string
}

// NewLocal returns a local store with a directory per namespace (directories are created on first write).
func NewLocal(roots map[string]string) *Local {
	r := map[string]string{}
	for ns, dir := range roots {
		if dir != "" {
			r[ns] = filepath.Clean(dir)
		}
	}
	return &Local{roots: r}
}

// Kind implements Store.
func (l *Local) Kind() string { return "local" }

// Location implements Store.
func (l *Local) Location() string {
	parts := make([]string, 0, len(l.roots))
	for _, ns := range Namespaces {
		if d, ok := l.roots[ns]; ok {
			parts = append(parts, ns+"="+d)
		}
	}
	return strings.Join(parts, ", ")
}

// Root returns the directory of a namespace ("" when the store has none).
func (l *Local) Root(ns string) string { return l.roots[ns] }

// Path returns the file that holds key.
func (l *Local) Path(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	ns, rest, _ := strings.Cut(key, "/")
	root, ok := l.roots[ns]
	if !ok {
		return "", fmt.Errorf("the local artifact store has no %s directory", ns)
	}
	return filepath.Join(root, filepath.FromSlash(rest)), nil
}

// Put implements Store: the content goes to a temporary file next to its destination, is checked, and is renamed
// into place.
func (l *Local) Put(_ context.Context, key string, r io.Reader, size int64, sum string) error {
	dst, err := l.Path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), ".put-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s: %d bytes, expected %d", key, n, size)
	}
	if sum != "" && hex.EncodeToString(h.Sum(nil)) != sum {
		return fmt.Errorf("%w: %s", ErrChecksum, key)
	}
	return os.Rename(tmp, dst)
}

// MoveFile implements FileMover: path is renamed into place (it may already be the object itself). Across
// filesystems it is copied instead and then removed.
func (l *Local) MoveFile(ctx context.Context, key, path string, size int64, sum string) error {
	dst, err := l.Path(key)
	if err != nil {
		return err
	}
	if filepath.Clean(path) == dst {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.Rename(path, dst); err == nil {
		return nil
	}
	f, err := os.Open(path) //nolint:gosec // the caller's own spool file
	if err != nil {
		return err
	}
	err = l.Put(ctx, key, f, size, sum)
	_ = f.Close()
	if err != nil {
		return err
	}
	return os.Remove(path)
}

type localObject struct {
	*os.File
	info Info
}

func (o *localObject) Info() Info { return o.info }

// Open implements Store.
func (l *Local) Open(_ context.Context, key string) (Object, error) {
	p, err := l.Path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p) //nolint:gosec // a validated artifact key below a configured root
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, notExist(key)
	}
	return &localObject{File: f, info: Info{Key: key, Size: fi.Size(), ModTime: fi.ModTime().UTC()}}, nil
}

// Stat implements Store.
func (l *Local) Stat(_ context.Context, key string) (Info, error) {
	p, err := l.Path(key)
	if err != nil {
		return Info{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return Info{}, err
	}
	if !fi.Mode().IsRegular() {
		return Info{}, notExist(key)
	}
	return Info{Key: key, Size: fi.Size(), ModTime: fi.ModTime().UTC()}, nil
}

// List implements Store: it walks the directories of the namespaces the prefix can match and returns the files that
// are artifacts (temporary and partial files are not).
func (l *Local) List(_ context.Context, prefix string) ([]Info, error) {
	var out []Info
	for _, ns := range Namespaces {
		root, ok := l.roots[ns]
		if !ok || (!strings.HasPrefix(prefix, ns+"/") && !strings.HasPrefix(ns+"/", prefix)) {
			continue
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			key := ns + "/" + filepath.ToSlash(rel)
			if !ValidKey(key) || !strings.HasPrefix(key, prefix) {
				return nil
			}
			fi, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while listing
			} else if err != nil {
				return err
			}
			out = append(out, Info{Key: key, Size: fi.Size(), ModTime: fi.ModTime().UTC()})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Delete implements Store. A recording's run directory is removed with its last segment.
func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.Path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if ns, _, _ := strings.Cut(key, "/"); ns == NSRecordings {
		_ = os.Remove(filepath.Dir(p)) // fails harmlessly while other files remain
	}
	return nil
}
