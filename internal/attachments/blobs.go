package attachments

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
	"regexp"
	"time"
)

// Blobs is content-addressed storage for attachment content (ADR-0013). The local backend is implemented; S3 lands with
// backups (M4) and is refused at startup until then.
type Blobs interface {
	// Write streams r into the store, at most max bytes, and returns its sha256 (hex), size and first bytes (for MIME
	// sniffing). Content that is already stored is not written twice.
	Write(ctx context.Context, r io.Reader, max int64) (sum string, size int64, head []byte, err error)
	Open(ctx context.Context, sum string) (io.ReadSeekCloser, error)
	// Remove deletes content not modified since before (a concurrent upload of the same content refreshes it).
	Remove(ctx context.Context, sum string, before time.Time) error
	// List returns every stored sum.
	List(ctx context.Context) ([]string, error)
}

// ErrTooLarge is returned when an upload exceeds the size limit.
var ErrTooLarge = errors.New("attachment exceeds the size limit")

var reSum = regexp.MustCompile(`^[0-9a-f]{64}$`)

// LocalBlobs stores content under dir/sha256/<aa>/<sum>.
type LocalBlobs struct{ dir string }

// NewLocal returns a local blob store rooted at dir (created on first write).
func NewLocal(dir string) *LocalBlobs { return &LocalBlobs{dir: dir} }

func (b *LocalBlobs) path(sum string) (string, error) {
	if !reSum.MatchString(sum) {
		return "", fmt.Errorf("invalid content address %q", sum)
	}
	return filepath.Join(b.dir, "sha256", sum[:2], sum), nil
}

// Write implements Blobs: the content is written to a temporary file, then renamed into place.
func (b *LocalBlobs) Write(_ context.Context, r io.Reader, max int64) (string, int64, []byte, error) {
	tmpDir := filepath.Join(b.dir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return "", 0, nil, err
	}
	f, err := os.CreateTemp(tmpDir, "upload-*")
	if err != nil {
		return "", 0, nil, err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	h := sha256.New()
	head := &headBuffer{max: 512}
	n, err := io.Copy(io.MultiWriter(f, h, head), io.LimitReader(r, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, nil, err
	}
	if n > max {
		return "", 0, nil, ErrTooLarge
	}
	sum := hex.EncodeToString(h.Sum(nil))
	dst, _ := b.path(sum)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", 0, nil, err
	}
	if _, err := os.Stat(dst); err == nil {
		now := time.Now()
		_ = os.Chtimes(dst, now, now) // keep it from a concurrent garbage collection
		return sum, n, head.b, nil
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", 0, nil, err
	}
	return sum, n, head.b, nil
}

// Open implements Blobs.
func (b *LocalBlobs) Open(_ context.Context, sum string) (io.ReadSeekCloser, error) {
	p, err := b.path(sum)
	if err != nil {
		return nil, err
	}
	return os.Open(p) //nolint:gosec // content-addressed path validated above
}

// Remove implements Blobs.
func (b *LocalBlobs) Remove(_ context.Context, sum string, before time.Time) error {
	p, err := b.path(sum)
	if err != nil {
		return err
	}
	fi, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if !fi.ModTime().Before(before) {
		return nil
	}
	return os.Remove(p)
}

// List implements Blobs.
func (b *LocalBlobs) List(_ context.Context) ([]string, error) {
	var out []string
	root := filepath.Join(b.dir, "sha256")
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return fs.SkipAll
		}
		if err != nil {
			return err
		}
		if !d.IsDir() && reSum.MatchString(d.Name()) {
			out = append(out, d.Name())
		}
		return nil
	})
	return out, err
}

type headBuffer struct {
	b   []byte
	max int
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := h.max - len(h.b); room > 0 {
		h.b = append(h.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
