// Derived from github.com/aurimasniekis/go-hallpass internal/s3client/s3test at bdd8329 (Apache-2.0, same author).

// Package s3test is an in-memory, path-style S3 server for tests: PUT, GET (whole or a "bytes=N-" / "bytes=N-M"
// range), HEAD, DELETE and ListObjectsV2. It checks that every request is signed for the expected access key and that
// a PUT body matches its x-amz-content-sha256. Objects carry a modification time (Last-Modified), which tests can set.
package s3test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Server is a fake bucket store.
type Server struct {
	*httptest.Server
	AccessKey string

	mu      sync.Mutex
	objects map[string][]byte    // "bucket/key"
	mod     map[string]time.Time // modification times, same keys
	// Fail, when set, is consulted first; a non-zero status is returned as an S3 error.
	Fail func(r *http.Request) int
	// PageSize bounds list pages (default 1000) to exercise continuation tokens.
	PageSize int
	Requests int
	// Ranges counts GET requests that carried a Range header.
	Ranges int
}

// New starts a TLS server; use CAPEM as the client's CA bundle.
func New(t testing.TB, accessKey string) *Server {
	s := &Server{AccessKey: accessKey, objects: map[string][]byte{}, mod: map[string]time.Time{}, PageSize: 1000}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// CAPEM returns the server certificate as a PEM bundle.
func (s *Server) CAPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))
}

// Keys lists stored object keys of bucket, sorted.
func (s *Server) Keys(bucket string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		if b, key, ok := strings.Cut(k, "/"); ok && b == bucket {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// Object returns a stored object.
func (s *Server) Object(bucket, key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[bucket+"/"+key]
	return b, ok
}

// SetObject stores or replaces an object directly (modified now).
func (s *Server) SetObject(bucket, key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[bucket+"/"+key] = data
	s.mod[bucket+"/"+key] = time.Now().UTC()
}

// SetModTime changes an object's modification time (to age it in tests).
func (s *Server) SetModTime(bucket, key string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mod[bucket+"/"+key] = t.UTC()
}

type s3err struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(s3err{Code: code, Message: msg})
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.Requests++
	s.mu.Unlock()
	if s.Fail != nil {
		if st := s.Fail(r); st != 0 {
			writeErr(w, st, "InternalError", "injected failure")
			return
		}
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential="+s.AccessKey+"/") || r.Header.Get("X-Amz-Date") == "" {
		writeErr(w, http.StatusForbidden, "InvalidAccessKeyId", "unsigned or wrong access key")
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket == "" {
		writeErr(w, http.StatusBadRequest, "InvalidBucketName", "no bucket")
		return
	}
	full := bucket + "/" + key
	switch {
	case r.Method == http.MethodGet && key == "" && r.URL.Query().Get("list-type") == "2":
		s.list(w, r, bucket)
	case r.Method == http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "IncompleteBody", err.Error())
			return
		}
		sum := sha256.Sum256(body)
		if r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
			writeErr(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch", "payload hash mismatch")
			return
		}
		s.mu.Lock()
		s.objects[full] = body
		s.mod[full] = time.Now().UTC()
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		s.mu.Lock()
		b, ok := s.objects[full]
		mod := s.mod[full]
		s.mu.Unlock()
		if !ok {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeErr(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		if !mod.IsZero() {
			w.Header().Set("Last-Modified", mod.Format(http.TimeFormat))
		}
		if rg := r.Header.Get("Range"); rg != "" && r.Method == http.MethodGet {
			s.mu.Lock()
			s.Ranges++
			s.mu.Unlock()
			start, end, ok := parseRange(rg, int64(len(b)))
			if !ok {
				writeErr(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable")
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(b)))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(b[start : end+1])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(b)
		}
	case r.Method == http.MethodDelete:
		s.mu.Lock()
		delete(s.objects, full)
		delete(s.mod, full)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

// parseRange accepts one "bytes=N-" or "bytes=N-M" range within an object of size n.
func parseRange(h string, n int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= n {
		return 0, 0, false
	}
	end = n - 1
	if b != "" {
		e, err := strconv.ParseInt(b, 10, 64)
		if err != nil || e < start {
			return 0, 0, false
		}
		end = min(e, n-1)
	}
	return start, end, true
}

type listContent struct {
	Key          string `xml:"Key"`
	Size         int    `xml:"Size"`
	LastModified string `xml:"LastModified"`
}

type listResult struct {
	XMLName               xml.Name      `xml:"ListBucketResult"`
	Contents              []listContent `xml:"Contents"`
	IsTruncated           bool          `xml:"IsTruncated"`
	NextContinuationToken string        `xml:"NextContinuationToken,omitempty"`
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, bucket string) {
	prefix := r.URL.Query().Get("prefix")
	start := r.URL.Query().Get("continuation-token")
	var keys []string
	for _, k := range s.Keys(bucket) {
		if strings.HasPrefix(k, prefix) && k > start {
			keys = append(keys, k)
		}
	}
	res := listResult{}
	if len(keys) > s.PageSize {
		keys = keys[:s.PageSize]
		res.IsTruncated, res.NextContinuationToken = true, keys[len(keys)-1]
	}
	for _, k := range keys {
		b, _ := s.Object(bucket, k)
		s.mu.Lock()
		mod := s.mod[bucket+"/"+k]
		s.mu.Unlock()
		if mod.IsZero() {
			mod = time.Now().UTC()
		}
		res.Contents = append(res.Contents, listContent{Key: k, Size: len(b), LastModified: mod.Format(time.RFC3339)})
	}
	w.Header().Set("Content-Type", "application/xml")
	if err := xml.NewEncoder(w).Encode(res); err != nil {
		panic(fmt.Sprint("encode list: ", err))
	}
}
