// Package disttest is an httptest stand-in for the Claude Code release host, for tests of code that uses
// internal/harness/dist (the node agent's launches). It serves {base}/{channel}, {base}/{v}/manifest.json and
// {base}/{v}/{platform}/claude over TLS. It is a test double: the real host is never contacted.
package disttest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/harness/dist"
)

// Platform is the release platform the fake serves and Manager installs.
const Platform = "linux-x64"

// basePath is the release base's path on the fake server.
const basePath = "/claude-code-releases"

// Server is a fake release host.
type Server struct {
	srv *httptest.Server

	mu        sync.Mutex
	pointers  map[string]string
	manifests map[string][]byte
	binaries  map[string][]byte
	hold      chan struct{}
	// holdOnly limits Hold to these versions (empty: every version).
	holdOnly map[string]bool

	// BinaryHits counts binary downloads.
	BinaryHits atomic.Int32
}

// New starts a fake release host, closed with the test.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{pointers: map[string]string{}, manifests: map[string][]byte{}, binaries: map[string][]byte{}}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError) // handshakes cut short by a closing test
	s.srv.StartTLS()
	t.Cleanup(func() {
		s.Release()
		s.srv.Close()
	})
	return s
}

// BaseURL is the release base on the fake host.
func (s *Server) BaseURL() string { return s.srv.URL + basePath }

// Client trusts the fake host's certificate.
func (s *Server) Client() *http.Client { return s.srv.Client() }

// Add publishes version with body as its linux-x64 binary and a matching manifest.
func (s *Server) Add(version string, body []byte) {
	sum := sha256.Sum256(body)
	raw, _ := json.Marshal(map[string]any{
		"version": version,
		"platforms": map[string]any{
			Platform: map[string]any{"binary": dist.BinaryName, "checksum": hex.EncodeToString(sum[:]), "size": len(body)},
		},
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manifests[version], s.binaries[version] = raw, body
}

// SetPointer points channel at version.
func (s *Server) SetPointer(channel, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pointers[channel] = version
}

// Hold makes binary downloads of versions (every version when none is given) wait until Release (or the request ends).
func (s *Server) Hold(versions ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hold == nil {
		s.hold = make(chan struct{})
	}
	s.holdOnly = map[string]bool{}
	for _, v := range versions {
		s.holdOnly[v] = true
	}
}

// Release lets held binary downloads go on.
func (s *Server) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hold != nil {
		close(s.hold)
		s.hold = nil
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, basePath+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	s.mu.Lock()
	var body []byte
	var found bool
	var hold chan struct{}
	switch {
	case len(parts) == 1:
		var v string
		v, found = s.pointers[parts[0]]
		body = []byte(v + "\n")
	case len(parts) == 2 && parts[1] == "manifest.json":
		body, found = s.manifests[parts[0]]
	case len(parts) == 3 && parts[1] == Platform && parts[2] == dist.BinaryName:
		body, found = s.binaries[parts[0]]
		if len(s.holdOnly) == 0 || s.holdOnly[parts[0]] {
			hold = s.hold
		}
		s.BinaryHits.Add(1)
	}
	s.mu.Unlock()
	if !found {
		http.NotFound(w, r)
		return
	}
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	_, _ = w.Write(body)
}

// Manager opens a dist.Manager on dir against the fake host. edit may change the options. The cache's read-only
// directories are made writable again when the test ends, so the temporary directory can be removed.
func (s *Server) Manager(t testing.TB, dir string, edit func(*dist.Options)) *dist.Manager {
	t.Helper()
	o := dist.Options{Dir: dir, BaseURL: s.BaseURL(), Platform: Platform, Client: s.Client()}
	if edit != nil {
		edit(&o)
	}
	t.Cleanup(func() { MakeWritable(dir) }) // runs after the manager's Close
	m, err := dist.New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

// MakeWritable makes every directory under dir writable again (verified version directories are 0500).
func MakeWritable(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) //nolint:gosec // test cleanup of a test's own cache
		}
		return nil
	})
}

// StubBinary is a stub "claude" executable that prints version like Claude Code's --version.
func StubBinary(version string) []byte {
	return []byte("#!/bin/sh\necho '" + version + " (Claude Code)'\n")
}
