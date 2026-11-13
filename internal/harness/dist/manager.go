package dist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Defaults.
const (
	DefaultKeep            = 3
	DefaultRefresh         = 15 * time.Minute
	DefaultDownloadTimeout = 15 * time.Minute

	// pointerTimeout bounds one channel pointer or manifest request.
	pointerTimeout = 30 * time.Second
	// maxPointerBytes and maxManifestBytes cap the small metadata responses (the pointer is "2.1.322\n", a manifest
	// about 2 KB).
	maxPointerBytes  = 64
	maxManifestBytes = 1 << 20
	// maxBinarySize rejects a manifest that announces an absurd size (2.1.322 linux-x64 is about 340 MB).
	maxBinarySize = 4 << 30

	memoFile     = "channels.json"
	verifiedFile = "verified.json"
)

// platforms are the release platform keys this package downloads for.
var platforms = map[string]bool{
	"linux-x64": true, "linux-arm64": true, "linux-x64-musl": true, "linux-arm64-musl": true,
	"darwin-x64": true, "darwin-arm64": true,
}

// Options configure a Manager.
type Options struct {
	// Dir is the cache directory, normally <data_dir>/harness/claude-code (absolute; created 0700).
	Dir string
	// BaseURL is the release base (https only). Default DefaultBaseURL.
	BaseURL string
	// Platform overrides the detected release platform (tests). Default Platform().
	Platform string
	// Keep is how many of the newest versions GC keeps besides protected ones. Default DefaultKeep.
	Keep int
	// Refresh is how long a channel pointer is memoised and how often Run re-resolves. Default DefaultRefresh.
	Refresh time.Duration
	// DownloadTimeout bounds one version install (manifest and binary). Default DefaultDownloadTimeout.
	DownloadTimeout time.Duration
	// Client is the HTTP client (tests pass an httptest TLS client). Redirects are always restricted to https.
	Client *http.Client
	Log    *slog.Logger
	// Now is the clock (tests). Default time.Now.
	Now func() time.Time
}

// Installed is one verified cached version; it is also the content of <dir>/<version>/verified.json.
type Installed struct {
	Version        string    `json:"version"`
	Platform       string    `json:"platform"`
	SHA256         string    `json:"sha256"`
	Size           int64     `json:"size"`
	ManifestSHA256 string    `json:"manifest_sha256"`
	FetchedAt      time.Time `json:"fetched_at"`
	// Path is the absolute path of the cached binary.
	Path string `json:"-"`
}

// Resolution is the version a launch should run.
type Resolution struct {
	// Requested is what was asked for: a channel or an exact version.
	Requested string
	// Target is the version Requested resolves to now; empty when a channel pointer could not be fetched.
	Target string
	// Version and Path are what to run. They differ from Target only when the target is not cached yet (it is being
	// downloaded in the background) or the pointer was unavailable: the newest verified cached version runs instead.
	Version string
	Path    string
}

// Pending reports whether the run gets another version than the requested target.
func (r Resolution) Pending() bool { return r.Version != r.Target }

type memoEntry struct {
	Version    string    `json:"version"`
	ResolvedAt time.Time `json:"resolved_at"`
}

type call struct {
	done chan struct{}
	inst Installed
	err  error
}

// Manager resolves, installs and garbage-collects cached Claude Code versions. It is safe for concurrent use.
type Manager struct {
	dir      string
	base     *url.URL
	platform string
	keep     int
	refresh  time.Duration
	timeout  time.Duration
	client   *http.Client
	log      *slog.Logger
	now      func() time.Time

	ctx    context.Context // background work (downloads shared by callers, prefetches)
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	memo     map[string]memoEntry
	inflight map[string]*call
}

// New validates the options, creates the cache directory and loads the memoised channel pointers.
func New(o Options) (*Manager, error) {
	if !filepath.IsAbs(o.Dir) {
		return nil, fmt.Errorf("claude code: cache directory must be absolute, got %q", o.Dir)
	}
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	base, err := ParseBaseURL(o.BaseURL)
	if err != nil {
		return nil, err
	}
	if o.Platform == "" {
		if o.Platform, err = Platform(); err != nil {
			return nil, err
		}
	}
	if !platforms[o.Platform] {
		return nil, fmt.Errorf("claude code: unsupported platform %q", o.Platform)
	}
	if o.Keep <= 0 {
		o.Keep = DefaultKeep
	}
	if o.Refresh <= 0 {
		o.Refresh = DefaultRefresh
	}
	if o.DownloadTimeout <= 0 {
		o.DownloadTimeout = DefaultDownloadTimeout
	}
	client := &http.Client{}
	if o.Client != nil {
		c := *o.Client
		client = &c
	}
	client.CheckRedirect = httpsRedirectsOnly
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("claude code: create cache directory: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		dir: o.Dir, base: base, platform: o.Platform, keep: o.Keep, refresh: o.Refresh, timeout: o.DownloadTimeout,
		client: client, log: o.Log, now: o.Now, ctx: ctx, cancel: cancel,
		memo: map[string]memoEntry{}, inflight: map[string]*call{},
	}
	m.loadMemo()
	return m, nil
}

// ParseBaseURL accepts an absolute https URL without credentials, query or fragment.
func ParseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("claude code: base URL must be an absolute https URL, got %q", raw)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("claude code: base URL must use https, got %q", u.Scheme)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("claude code: base URL must not contain credentials, a query or a fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

func httpsRedirectsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("claude code: refusing a redirect to %s", req.URL.Scheme)
	}
	if len(via) >= 5 {
		return errors.New("claude code: too many redirects")
	}
	return nil
}

// Close stops background downloads and prefetches and waits for them.
func (m *Manager) Close() {
	m.mu.Lock() // start checks the context under mu before wg.Add
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
}

// Platform returns the release platform the manager installs.
func (m *Manager) Platform() string { return m.platform }

// Dir returns the cache directory.
func (m *Manager) Dir() string { return m.dir }

// Resolve picks the version to run for requested: a channel or an exact version ("" means ChannelLatest).
//
//   - An exact version is installed if needed (bounded by the download timeout); a version the release server does not
//     have fails with ErrNotFound.
//   - A channel resolves through its memoised pointer. When the target is cached it runs; when it is not and another
//     verified version is cached, the newest cached version runs and the target downloads in the background; when
//     nothing is cached the call waits for the target's install. When the pointer cannot be fetched, the last known
//     target (if cached) or the newest cached version runs.
func (m *Manager) Resolve(ctx context.Context, requested string) (Resolution, error) {
	if requested == "" {
		requested = ChannelLatest
	}
	if ValidVersion(requested) {
		inst, err := m.Ensure(ctx, requested)
		if err != nil {
			return Resolution{}, err
		}
		return Resolution{Requested: requested, Target: requested, Version: inst.Version, Path: inst.Path}, nil
	}
	if !ValidChannel(requested) {
		return Resolution{}, fmt.Errorf("%w: %q (want latest, stable or X.Y.Z)", ErrInvalidVersion, requested)
	}
	target, perr := m.Channel(ctx, requested)
	if perr != nil {
		inst, ok := m.fallback(requested)
		if !ok {
			return Resolution{}, fmt.Errorf("%w: %w", ErrNoVersion, perr)
		}
		m.log.Warn("claude code channel unavailable; using a cached version", slog.String("channel", requested),
			slog.String("version", inst.Version), slog.String("error", perr.Error()))
		return Resolution{Requested: requested, Version: inst.Version, Path: inst.Path}, nil
	}
	if inst, ok := m.lookup(target); ok {
		return Resolution{Requested: requested, Target: target, Version: inst.Version, Path: inst.Path}, nil
	}
	if cached := m.Installed(); len(cached) > 0 {
		m.Prefetch(target) //nolint:contextcheck // a background install outlives this call
		return Resolution{Requested: requested, Target: target, Version: cached[0].Version, Path: cached[0].Path}, nil
	}
	inst, err := m.Ensure(ctx, target)
	if err != nil {
		return Resolution{}, err
	}
	return Resolution{Requested: requested, Target: target, Version: inst.Version, Path: inst.Path}, nil
}

// fallback is the last known target of channel when it is cached, else the newest cached version.
func (m *Manager) fallback(channel string) (Installed, bool) {
	m.mu.Lock()
	e, ok := m.memo[channel]
	m.mu.Unlock()
	if ok {
		if inst, ok := m.lookup(e.Version); ok {
			return inst, true
		}
	}
	cached := m.Installed()
	if len(cached) == 0 {
		return Installed{}, false
	}
	return cached[0], true
}

// Channel returns the version channel points to, memoised for the refresh interval.
func (m *Manager) Channel(ctx context.Context, channel string) (string, error) {
	if !ValidChannel(channel) {
		return "", fmt.Errorf("%w: unknown channel %q", ErrInvalidVersion, channel)
	}
	m.mu.Lock()
	e, ok := m.memo[channel]
	m.mu.Unlock()
	if ok && m.now().Sub(e.ResolvedAt) < m.refresh && !e.ResolvedAt.After(m.now()) {
		return e.Version, nil
	}
	return m.refreshChannel(ctx, channel)
}

// Resolved returns the memoised channel targets (channel → version), fresh or not.
func (m *Manager) Resolved() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.memo))
	for c, e := range m.memo {
		out[c] = e.Version
	}
	return out
}

func (m *Manager) refreshChannel(ctx context.Context, channel string) (string, error) {
	body, err := m.get(ctx, m.base.JoinPath(channel), maxPointerBytes)
	if err != nil {
		return "", fmt.Errorf("claude code: resolve %s: %w", channel, err)
	}
	v := strings.TrimSpace(string(body))
	if !ValidVersion(v) {
		return "", fmt.Errorf("%w: the %s pointer is not a version", ErrInvalidVersion, channel)
	}
	m.mu.Lock()
	m.memo[channel] = memoEntry{Version: v, ResolvedAt: m.now().UTC()}
	snapshot := make(map[string]memoEntry, len(m.memo))
	for k, e := range m.memo {
		snapshot[k] = e
	}
	m.mu.Unlock()
	if err := writeFileAtomic(filepath.Join(m.dir, memoFile), snapshot); err != nil {
		m.log.Warn("claude code: could not persist channel pointers", slog.String("error", err.Error()))
	}
	return v, nil
}

func (m *Manager) loadMemo() {
	b, err := os.ReadFile(filepath.Join(m.dir, memoFile))
	if err != nil {
		return
	}
	var saved map[string]memoEntry
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	for c, e := range saved {
		if ValidChannel(c) && ValidVersion(e.Version) {
			m.memo[c] = e
		}
	}
}

// get fetches a small metadata document, bounded by pointerTimeout and limit bytes.
func (m *Manager) get(ctx context.Context, u *url.URL, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, pointerTimeout)
	defer cancel()
	resp, err := m.do(ctx, u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response larger than %d bytes", limit)
	}
	return b, nil
}

// do sends a GET and maps the status: 404 is ErrNotFound, anything but 200 an error.
func (m *Manager) do(ctx context.Context, u *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req) //nolint:gosec // G704: the URL is the operator's https base plus validated parts
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp, nil
	case http.StatusNotFound:
		_ = resp.Body.Close()
		return nil, ErrNotFound
	}
	_ = resp.Body.Close()
	return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
}
