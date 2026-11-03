package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/s3client"
)

// ObjectAPI is the object storage the S3 store uses (satisfied by *s3client.Client).
type ObjectAPI interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, payloadSHA256 string) error
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
	GetRange(ctx context.Context, key string, off int64) (io.ReadCloser, int64, error)
	Stat(ctx context.Context, key string) (s3client.Object, error)
	List(ctx context.Context, prefix string) ([]s3client.Object, error)
	Delete(ctx context.Context, key string) error
}

// S3 keeps artifacts in an S3-compatible bucket under <prefix><key>. Uploads are single PUTs whose payload SHA-256 the
// service verifies (so at most s3client.MaxPutSize per object); reads are GETs, and a seek turns the next read into a
// range request from the new offset.
type S3 struct {
	api    ObjectAPI
	bucket string
	prefix string
}

// NewS3 returns an S3 store; prefix is "" or ends with a slash.
func NewS3(api ObjectAPI, bucket, prefix string) *S3 {
	return &S3{api: api, bucket: bucket, prefix: prefix}
}

// Kind implements Store.
func (s *S3) Kind() string { return "s3" }

// Location implements Store.
func (s *S3) Location() string { return "s3://" + s.bucket + "/" + s.prefix }

func mapErr(key string, err error) error {
	if errors.Is(err, s3client.ErrNotFound) {
		return notExist(key)
	}
	return err
}

// Put implements Store.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, sum string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if !ValidSum(sum) {
		return fmt.Errorf("%s: an S3 upload needs the content's SHA-256", key)
	}
	return s.api.Put(ctx, s.prefix+key, io.LimitReader(r, size), size, sum)
}

// Stat implements Store.
func (s *S3) Stat(ctx context.Context, key string) (Info, error) {
	if err := checkKey(key); err != nil {
		return Info{}, err
	}
	o, err := s.api.Stat(ctx, s.prefix+key)
	if err != nil {
		return Info{}, mapErr(key, err)
	}
	return Info{Key: key, Size: o.Size, ModTime: o.LastModified}, nil
}

// Open implements Store: the object is described first (its size bounds every read), and its content is fetched on
// the first read.
func (s *S3) Open(ctx context.Context, key string) (Object, error) {
	info, err := s.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	return &s3Object{ctx: ctx, s: s, info: info}, nil
}

// List implements Store.
func (s *S3) List(ctx context.Context, prefix string) ([]Info, error) {
	objs, err := s.api.List(ctx, s.prefix+prefix)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(objs))
	for _, o := range objs {
		key := strings.TrimPrefix(o.Key, s.prefix)
		if !ValidKey(key) || !strings.HasPrefix(key, prefix) {
			continue // not an artifact (another tool's object under the prefix)
		}
		out = append(out, Info{Key: key, Size: o.Size, ModTime: o.LastModified.UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Delete implements Store.
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	return s.api.Delete(ctx, s.prefix+key)
}

// s3Object reads an object lazily: Read opens the body at the current offset (a range request unless it is 0), and a
// Seek to another offset closes it.
type s3Object struct {
	ctx  context.Context // an open object reads within the context it was opened with
	s    *S3
	info Info
	off  int64
	body io.ReadCloser
}

func (o *s3Object) Info() Info { return o.info }

func (o *s3Object) Read(p []byte) (int, error) {
	if o.off >= o.info.Size {
		return 0, io.EOF
	}
	if o.body == nil {
		var (
			body io.ReadCloser
			err  error
		)
		if o.off == 0 {
			body, _, err = o.s.api.Get(o.ctx, o.s.prefix+o.info.Key)
		} else {
			body, _, err = o.s.api.GetRange(o.ctx, o.s.prefix+o.info.Key, o.off)
		}
		if err != nil {
			return 0, mapErr(o.info.Key, err)
		}
		o.body = body
	}
	if rest := o.info.Size - o.off; int64(len(p)) > rest {
		p = p[:rest]
	}
	n, err := o.body.Read(p)
	o.off += int64(n)
	if errors.Is(err, io.EOF) && o.off < o.info.Size {
		return n, fmt.Errorf("%s: %w (the object changed or the connection ended)", o.info.Key, io.ErrUnexpectedEOF)
	}
	if errors.Is(err, io.EOF) && n > 0 {
		err = nil
	}
	return n, err
}

func (o *s3Object) Seek(off int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = off
	case io.SeekCurrent:
		abs = o.off + off
	case io.SeekEnd:
		abs = o.info.Size + off
	default:
		return 0, errors.New("invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("negative position")
	}
	if abs != o.off && o.body != nil {
		_ = o.body.Close()
		o.body = nil
	}
	o.off = abs
	return abs, nil
}

func (o *s3Object) Close() error {
	if o.body != nil {
		err := o.body.Close()
		o.body = nil
		return err
	}
	return nil
}
