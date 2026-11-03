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

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
)

// Blobs is content-addressed storage for attachment content (ADR-0013, ADR-0048).
type Blobs interface {
	// Write streams r into the store, at most max bytes, and returns its sha256 (hex), size and first bytes (for MIME
	// sniffing). Content that is already stored is not written twice.
	Write(ctx context.Context, r io.Reader, max int64) (sum string, size int64, head []byte, err error)
	// Open returns the content; a full sequential read of damaged content fails at its end (artifacts.Verified).
	Open(ctx context.Context, sum string) (io.ReadSeekCloser, error)
	// Remove deletes content not modified since before (a concurrent upload of the same content refreshes it).
	Remove(ctx context.Context, sum string, before time.Time) error
	// List returns every stored sum.
	List(ctx context.Context) ([]string, error)
}

// ErrTooLarge is returned when an upload exceeds the size limit.
var ErrTooLarge = errors.New("attachment exceeds the size limit")

var reSum = regexp.MustCompile(`^[0-9a-f]{64}$`)

// refreshAge is how old stored content may be before an upload of the same bytes stores it again, refreshing its
// modification time: garbage collection only removes unreferenced content older than an hour, so content an upload
// finds already stored is never removed under it.
const refreshAge = 30 * time.Minute

// StoreBlobs keeps attachment content in an artifact store under attachments/sha256/<aa>/<sum>. An upload is streamed
// into a spool file (hashing it on the way, never held in memory), then handed to the store: renamed into place on
// the local disk, or uploaded with its SHA-256, which S3 verifies.
type StoreBlobs struct {
	store artifacts.Store
	spool string
}

// LocalBlobs is the local-disk blob store (StoreBlobs over a local artifact store).
type LocalBlobs = StoreBlobs

// NewStore returns blobs in store, spooling uploads in spoolDir.
func NewStore(store artifacts.Store, spoolDir string) *StoreBlobs {
	return &StoreBlobs{store: store, spool: spoolDir}
}

// NewLocal returns a local blob store rooted at dir (content under dir/sha256/<aa>/<sum>, uploads spooled in dir/tmp).
func NewLocal(dir string) *LocalBlobs {
	return NewStore(artifacts.NewLocal(map[string]string{artifacts.NSAttachments: dir}), filepath.Join(dir, "tmp"))
}

func key(sum string) (string, error) {
	if !reSum.MatchString(sum) {
		return "", fmt.Errorf("invalid content address %q", sum)
	}
	return artifacts.AttachmentKey(sum), nil
}

// Write implements Blobs.
func (b *StoreBlobs) Write(ctx context.Context, r io.Reader, max int64) (string, int64, []byte, error) {
	if err := os.MkdirAll(b.spool, 0o700); err != nil {
		return "", 0, nil, err
	}
	f, err := os.CreateTemp(b.spool, "upload-*")
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
	k, _ := key(sum)
	if info, err := b.store.Stat(ctx, k); err == nil && info.Size == n && time.Since(info.ModTime) < refreshAge {
		return sum, n, head.b, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", 0, nil, err
	}
	if err := artifacts.PutFile(ctx, b.store, k, tmp, n, sum); err != nil {
		return "", 0, nil, err
	}
	return sum, n, head.b, nil
}

// Open implements Blobs.
func (b *StoreBlobs) Open(ctx context.Context, sum string) (io.ReadSeekCloser, error) {
	k, err := key(sum)
	if err != nil {
		return nil, err
	}
	o, err := b.store.Open(ctx, k)
	if err != nil {
		return nil, err
	}
	return artifacts.Verified(o, sum), nil
}

// Remove implements Blobs.
func (b *StoreBlobs) Remove(ctx context.Context, sum string, before time.Time) error {
	k, err := key(sum)
	if err != nil {
		return err
	}
	info, err := b.store.Stat(ctx, k)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if !info.ModTime.Before(before) {
		return nil
	}
	return b.store.Delete(ctx, k)
}

// List implements Blobs.
func (b *StoreBlobs) List(ctx context.Context) ([]string, error) {
	infos, err := b.store.List(ctx, artifacts.NSAttachments+"/sha256/")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		if sum, ok := artifacts.AttachmentSum(i.Key); ok {
			out = append(out, sum)
		}
	}
	return out, nil
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
