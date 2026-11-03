package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/s3client"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// Entry is a backup found in a target.
type Entry struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	// Complete is true when manifest.sha256 exists (it is written last).
	Complete bool `json:"complete"`
	// StoredBytes is what the backup occupies in the target.
	StoredBytes int64 `json:"stored_bytes"`
	// Manifest is the plaintext manifest (unauthenticated; Verify with the KEK authenticates it). Nil when incomplete.
	Manifest *Manifest `json:"-"`
}

// Target stores backups: a local directory or S3-compatible object storage.
type Target interface {
	// Kind is "local" or "s3".
	Kind() string
	// Location describes where backups go (a directory, or bucket and prefix), for display.
	Location() string
	// StagingDir is where a new backup is assembled before Store.
	StagingDir() string
	// Store moves or uploads the complete backup directory dir (named after the ID) into the target.
	Store(ctx context.Context, dir string) error
	// List returns the backups in the target, newest first.
	List(ctx context.Context) ([]Entry, error)
	// Fetch returns a local directory holding backup id; cleanup removes any temporary copy.
	Fetch(ctx context.Context, id, tmpRoot string) (dir string, cleanup func(), err error)
	// Delete removes backup id.
	Delete(ctx context.Context, id string) error
}

// LocalTarget keeps backups as directories <Dir>/<id>.
type LocalTarget struct{ Dir string }

// Kind implements Target.
func (t LocalTarget) Kind() string { return "local" }

// Location implements Target.
func (t LocalTarget) Location() string { return t.Dir }

// StagingDir implements Target: backups are created in place (Create renames atomically).
func (t LocalTarget) StagingDir() string { return t.Dir }

// Store implements Target: dir is already <Dir>/<id>.
func (t LocalTarget) Store(_ context.Context, dir string) error {
	if filepath.Dir(dir) != filepath.Clean(t.Dir) {
		return fmt.Errorf("backup %s is not in %s", dir, t.Dir)
	}
	return nil
}

// List implements Target.
func (t LocalTarget) List(_ context.Context) ([]Entry, error) {
	des, err := os.ReadDir(t.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []Entry
	for _, d := range des {
		if !d.IsDir() || !ValidID(d.Name()) {
			continue
		}
		e := Entry{ID: d.Name()}
		dir := filepath.Join(t.Dir, d.Name())
		if m, err := ReadManifest(dir); err == nil && m.ID == e.ID {
			e.Complete, e.Manifest, e.CreatedAt, e.StoredBytes = true, m, m.CreatedAt, m.SealedBytes
		} else if fi, err := d.Info(); err == nil {
			e.CreatedAt = fi.ModTime().UTC()
		}
		out = append(out, e)
	}
	sortEntries(out)
	return out, nil
}

// Fetch implements Target (no copy).
func (t LocalTarget) Fetch(_ context.Context, id, _ string) (string, func(), error) {
	if !ValidID(id) {
		return "", nil, fmt.Errorf("invalid backup ID %q", id)
	}
	dir := filepath.Join(t.Dir, id)
	if _, err := os.Stat(dir); err != nil {
		return "", nil, err
	}
	return dir, func() {}, nil
}

// Delete implements Target.
func (t LocalTarget) Delete(_ context.Context, id string) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid backup ID %q", id)
	}
	dir := filepath.Join(t.Dir, id)
	// The checksum goes first, so an interrupted delete never leaves something that looks complete.
	if err := os.Remove(filepath.Join(dir, ManifestSumFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return store.FsyncDir(t.Dir)
}

func sortEntries(es []Entry) {
	sort.Slice(es, func(i, j int) bool {
		if !es[i].CreatedAt.Equal(es[j].CreatedAt) {
			return es[i].CreatedAt.After(es[j].CreatedAt)
		}
		return es[i].ID > es[j].ID
	})
}

// ObjectStore is the object storage an S3 target uses (satisfied by *s3client.Client).
type ObjectStore interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, payloadSHA256 string) error
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
	List(ctx context.Context, prefix string) ([]s3client.Object, error)
	Delete(ctx context.Context, key string) error
}

// S3Target keeps each backup under <Prefix><id>/ in a bucket. Every object is already sealed; manifest.sha256 is
// uploaded last and deleted first, so its presence marks a complete backup.
type S3Target struct {
	Objects ObjectStore
	Bucket  string
	Prefix  string
	// Staging is a local directory for assembling and downloading backups (inside the data directory).
	Staging string
}

// Kind implements Target.
func (t S3Target) Kind() string { return "s3" }

// Location implements Target.
func (t S3Target) Location() string { return "s3://" + t.Bucket + "/" + t.Prefix }

// StagingDir implements Target.
func (t S3Target) StagingDir() string { return t.Staging }

// ValidatePrefix accepts "" or a slash-terminated prefix of safe characters (go-hallpass backup.ValidatePrefix).
func ValidatePrefix(p string) error { return artifacts.ValidatePrefix(p) }

// Store implements Target: every object of the backup is uploaded with its SHA-256 (checked by the service), the
// manifest files last.
func (t S3Target) Store(ctx context.Context, dir string) error {
	m, err := ReadManifest(dir)
	if err != nil {
		return err
	}
	base := t.Prefix + m.ID + "/"
	for _, rel := range objectPaths(m) {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := filepath.Join(dir, filepath.FromSlash(rel))
		got, err := hashFile(p)
		if err != nil {
			return err
		}
		f, err := openRegular(p)
		if err != nil {
			return err
		}
		err = t.Objects.Put(ctx, base+rel, io.LimitReader(f, got.size), got.size, got.sum)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("upload %s: %w", rel, err)
		}
	}
	return nil
}

// List implements Target.
func (t S3Target) List(ctx context.Context) ([]Entry, error) {
	objs, err := t.Objects.List(ctx, t.Prefix)
	if err != nil {
		return nil, err
	}
	by := map[string]*Entry{}
	for _, o := range objs {
		id, rel, ok := strings.Cut(strings.TrimPrefix(o.Key, t.Prefix), "/")
		if !ok || !ValidID(id) {
			continue
		}
		e := by[id]
		if e == nil {
			e = &Entry{ID: id, CreatedAt: o.LastModified.UTC()}
			by[id] = e
		}
		e.StoredBytes += o.Size
		if rel == ManifestSumFile {
			e.Complete = true
		}
	}
	out := make([]Entry, 0, len(by))
	for _, e := range by {
		if e.Complete {
			if m, err := t.manifest(ctx, e.ID); err == nil {
				e.Manifest, e.CreatedAt = m, m.CreatedAt
			} else {
				e.Complete = false
			}
		}
		out = append(out, *e)
	}
	sortEntries(out)
	return out, nil
}

func (t S3Target) manifest(ctx context.Context, id string) (*Manifest, error) {
	rc, _, err := t.Objects.Get(ctx, t.Prefix+id+"/"+ManifestFile)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 64<<20))
	if err != nil {
		return nil, err
	}
	m, err := parseManifest(b)
	if err != nil || m.ID != id {
		return nil, fmt.Errorf("%w: manifest of %s", ErrInvalid, id)
	}
	return m, nil
}

// Fetch implements Target: it downloads the backup's objects into a new directory below tmpRoot, which Verify and
// Restore then check like a local backup.
func (t S3Target) Fetch(ctx context.Context, id, tmpRoot string) (string, func(), error) {
	if !ValidID(id) {
		return "", nil, fmt.Errorf("invalid backup ID %q", id)
	}
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return "", nil, err
	}
	tmp, err := os.MkdirTemp(tmpRoot, ".fetch-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	dir := filepath.Join(tmp, id)
	m, err := t.manifest(ctx, id)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	for _, rel := range objectPaths(m) {
		if !validRel(strings.TrimSuffix(rel, SealedSuffix)) {
			cleanup()
			return "", nil, fmt.Errorf("%w: object %q", ErrInvalid, rel)
		}
		if err := t.download(ctx, t.Prefix+id+"/"+rel, filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return dir, cleanup, nil
}

func (t S3Target) download(ctx context.Context, key, dst string) error {
	rc, _, err := t.Objects.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("download %s: %w", path.Base(key), err)
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // inside our staging directory
	if err != nil {
		return err
	}
	// Objects are bounded by the S3 single-PUT limit.
	_, err = io.Copy(out, io.LimitReader(rc, s3client.MaxPutSize))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// Delete implements Target.
func (t S3Target) Delete(ctx context.Context, id string) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid backup ID %q", id)
	}
	objs, err := t.Objects.List(ctx, t.Prefix+id+"/")
	if err != nil {
		return err
	}
	sort.SliceStable(objs, func(i, j int) bool { return strings.HasSuffix(objs[i].Key, "/"+ManifestSumFile) })
	for _, o := range objs {
		if err := t.Objects.Delete(ctx, o.Key); err != nil {
			return err
		}
	}
	return nil
}
