// Package artifacts is SpaceMan's artifact store (SM-STORE-002, SM-ARCH-003, ADR-0048): attachment content, central's
// copies of terminal-recording segments and run transcripts, kept as immutable objects under slash-separated keys in
// three namespaces:
//
//	attachments/sha256/<aa>/<sha256>          content-addressed attachment content
//	recordings/<run>/seg-NNNNNN.smrec.zst     one segment of a run's recording
//	transcripts/<run>.jsonl                   a run's native transcript
//
// Two stores are compiled in: Local (directories on central's disk, the default, in the layout SpaceMan has always
// used) and S3 (an S3-compatible bucket through internal/s3client). Objects are written whole with a known size and
// SHA-256, never appended to: a recording that is still being shipped and a transcript that is still being received
// live in local spool files and are handed to the store once complete (PutFile). Reads stream and seek (S3 range
// requests); nothing buffers a whole object in memory.
//
// Every call may do file or network I/O, so none may run inside a store write transaction (the txscope analyzer
// enforces this for this package).
package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"regexp"
	"time"
)

// Namespaces (the first key segment).
const (
	NSAttachments = "attachments"
	NSRecordings  = "recordings"
	NSTranscripts = "transcripts"
)

// Namespaces lists every namespace, in the order backups and migrations walk them.
var Namespaces = []string{NSAttachments, NSRecordings, NSTranscripts}

// Info describes a stored object.
type Info struct {
	Key     string    `json:"key"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// Object is an open stored object: a seekable reader over its content.
type Object interface {
	io.ReadSeekCloser
	// Info returns the object's key, size and modification time.
	Info() Info
}

// Store keeps artifacts. Implementations: Local and S3.
type Store interface {
	// Kind is "local" or "s3".
	Kind() string
	// Location describes where objects go (directories, or a bucket and prefix) for display. It never holds a secret.
	Location() string
	// Put stores size bytes read from r as key, replacing an existing object. sha256 is the content's hex SHA-256; the
	// content is checked against it (S3 checks it server side) and nothing is stored when it does not match.
	Put(ctx context.Context, key string, r io.Reader, size int64, sha256 string) error
	// Open opens key for reading. A missing key is an error wrapping fs.ErrNotExist.
	Open(ctx context.Context, key string) (Object, error)
	// Stat describes key. A missing key is an error wrapping fs.ErrNotExist.
	Stat(ctx context.Context, key string) (Info, error)
	// List returns every stored artifact whose key starts with prefix, in key order.
	List(ctx context.Context, prefix string) ([]Info, error)
	// Delete removes key. Removing a missing key succeeds.
	Delete(ctx context.Context, key string) error
}

// FileMover is implemented by stores that can take over a local file without copying it (Local, by renaming it).
type FileMover interface {
	MoveFile(ctx context.Context, key, path string, size int64, sha256 string) error
}

// ErrChecksum means stored or received content does not match its SHA-256.
var ErrChecksum = errors.New("artifact content does not match its SHA-256")

var (
	reAttachmentKey = regexp.MustCompile(`^attachments/sha256/([0-9a-f]{2})/([0-9a-f]{64})$`)
	reRecordingKey  = regexp.MustCompile(`^recordings/run_[0-9a-z]{26}/seg-[0-9]{6}\.smrec\.zst$`)
	reTranscriptKey = regexp.MustCompile(`^transcripts/run_[0-9a-z]{26}\.jsonl$`)
	reSum           = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidKey reports whether key names an artifact: an attachment (whose directory matches its sum), a recording
// segment or a transcript. Anything else (temporary files, foreign objects under the prefix) is not an artifact.
func ValidKey(key string) bool {
	if m := reAttachmentKey.FindStringSubmatch(key); m != nil {
		return m[2][:2] == m[1]
	}
	return reRecordingKey.MatchString(key) || reTranscriptKey.MatchString(key)
}

func checkKey(key string) error {
	if !ValidKey(key) {
		return fmt.Errorf("invalid artifact key %q", key)
	}
	return nil
}

// AttachmentKey returns the key of attachment content with the given SHA-256 (hex).
func AttachmentKey(sum string) string {
	return NSAttachments + "/sha256/" + sum[:min(2, len(sum))] + "/" + sum
}

// AttachmentSum returns the SHA-256 an attachment key names.
func AttachmentSum(key string) (string, bool) {
	if m := reAttachmentKey.FindStringSubmatch(key); m != nil && m[2][:2] == m[1] {
		return m[2], true
	}
	return "", false
}

// RecordingKey returns the key of one segment (by file name) of a run's recording.
func RecordingKey(runID, segment string) string { return NSRecordings + "/" + runID + "/" + segment }

// RecordingPrefix is the prefix of every segment of a run's recording.
func RecordingPrefix(runID string) string { return NSRecordings + "/" + runID + "/" }

// TranscriptKey returns the key of a run's transcript.
func TranscriptKey(runID string) string { return NSTranscripts + "/" + runID + ".jsonl" }

// ValidSum reports whether s is a hex SHA-256.
func ValidSum(s string) bool { return reSum.MatchString(s) }

// FileSum returns the size and SHA-256 of a local file.
func FileSum(path string) (int64, string, error) {
	f, err := os.Open(path) //nolint:gosec // callers pass their own spool files
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// PutFile hands a complete local file to the store as key. A store that can take the file over (Local) renames it;
// any other store uploads it, and the file is removed afterwards. On success the file at path is gone or is the
// stored object itself; on failure it is left where it was. sha256 "" computes it.
func PutFile(ctx context.Context, s Store, key, path string, size int64, sum string) error {
	if sum == "" {
		var err error
		if size, sum, err = FileSum(path); err != nil {
			return err
		}
	}
	if m, ok := s.(FileMover); ok {
		return m.MoveFile(ctx, key, path, size, sum)
	}
	f, err := os.Open(path) //nolint:gosec // callers pass their own spool files
	if err != nil {
		return err
	}
	err = s.Put(ctx, key, f, size, sum)
	_ = f.Close()
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// Verified wraps an object so that reading it sequentially from its start to its end fails with ErrChecksum on the
// read that reaches the end (that read's bytes are withheld) when the content does not hash to sum. Reads after a seek to another offset (range requests) cannot be
// checked and are passed through.
func Verified(o Object, sum string) Object {
	return &verified{Object: o, sum: sum, h: sha256.New(), size: o.Info().Size}
}

type verified struct {
	Object
	sum      string
	h        hash.Hash
	size     int64
	pos      int64
	checking bool
	started  bool
}

func (v *verified) Read(p []byte) (int, error) {
	if !v.started {
		v.started, v.checking = true, v.pos == 0
	}
	n, err := v.Object.Read(p)
	if v.checking {
		v.h.Write(p[:n])
	}
	v.pos += int64(n)
	// Checked on reaching the end, not on EOF: readers bounded by the size (http.ServeContent) never see EOF.
	if v.checking && v.pos >= v.size {
		v.checking = false
		if hex.EncodeToString(v.h.Sum(nil)) != v.sum {
			// The last bytes are withheld, so a download of damaged content ends short (the client sees an error)
			// instead of completing.
			return 0, fmt.Errorf("%w: %s", ErrChecksum, v.Info().Key)
		}
	}
	return n, err
}

func (v *verified) Seek(off int64, whence int) (int64, error) {
	p, err := v.Object.Seek(off, whence)
	if err != nil {
		return p, err
	}
	if p != v.pos {
		v.started = false
		if p == 0 {
			v.h.Reset()
		}
	}
	v.pos = p
	return p, nil
}

// copySpool streams r into a new file in dir, checking its size and SHA-256 (when sum is set), and returns the path,
// the size and the SHA-256.
func copySpool(dir string, r io.Reader, size int64, sum string) (string, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	f, err := os.CreateTemp(dir, ".spool-*")
	if err != nil {
		return "", "", err
	}
	tmp := f.Name()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	got := hex.EncodeToString(h.Sum(nil))
	switch {
	case err != nil:
	case n != size:
		err = fmt.Errorf("artifact is %d bytes, expected %d", n, size)
	case sum != "" && got != sum:
		err = ErrChecksum
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", "", err
	}
	return tmp, got, nil
}

// Copy copies key from src to dst through a spool file in tmpDir, checking its size and, for attachment content,
// its content address.
func Copy(ctx context.Context, src, dst Store, key, tmpDir string) (Info, error) {
	o, err := src.Open(ctx, key)
	if err != nil {
		return Info{}, err
	}
	info := o.Info()
	want, _ := AttachmentSum(key)
	tmp, sum, err := copySpool(tmpDir, o, info.Size, want)
	_ = o.Close()
	if err != nil {
		return Info{}, fmt.Errorf("%s: %w", key, err)
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := PutFile(ctx, dst, key, tmp, info.Size, sum); err != nil {
		return Info{}, fmt.Errorf("%s: %w", key, err)
	}
	return info, nil
}

// NotExist wraps fs.ErrNotExist for key.
func notExist(key string) error { return fmt.Errorf("%w: %s", fs.ErrNotExist, key) }
