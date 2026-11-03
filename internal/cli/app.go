package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

// App holds the remote client's configuration: the global flags, the API document and the I/O streams.
type App struct {
	// SpecSource returns the OpenAPI document of this build (cmd/spaceman builds it in process).
	SpecSource func() ([]byte, error)
	// Getenv reads the environment (tests replace it).
	Getenv func(string) string
	// Stdin is standard input (tests replace it).
	Stdin io.Reader
	// Transport replaces the HTTP transport (tests).
	Transport http.RoundTripper
	// UserAgent is sent as User-Agent.
	UserAgent string
	// PollInterval is the pause between status polls of --wait (default 2 s).
	PollInterval time.Duration

	server    string
	tokenFile string
	insecure  bool
	output    string
	timeout   time.Duration

	specOnce sync.Once
	spec     *Spec
	specErr  error
	client   *Client
}

// Flag and environment names of the global options.
const (
	EnvServer    = "SPACEMAN_SERVER"
	EnvToken     = "SPACEMAN_TOKEN"
	EnvTokenFile = "SPACEMAN_TOKEN_FILE"
	EnvOutput    = "SPACEMAN_OUTPUT"
	EnvSpace     = "SPACEMAN_SPACE"
	EnvInsecure  = "SPACEMAN_ALLOW_INSECURE_HTTP"
)

func (a *App) getenv(k string) string {
	if a.Getenv != nil {
		return a.Getenv(k)
	}
	return os.Getenv(k)
}

func (a *App) stdin() io.Reader {
	if a.Stdin != nil {
		return a.Stdin
	}
	return os.Stdin
}

// BindFlags adds the global remote options to root (persistent, so every command accepts them).
func (a *App) BindFlags(root *cobra.Command) {
	pf := root.PersistentFlags()
	pf.StringVar(&a.server, "server", "", "central URL, e.g. https://spaceman.example.com (env "+EnvServer+")")
	pf.StringVar(&a.tokenFile, "token-file", "", "file holding the API token (env "+EnvTokenFile+"; or put the token itself in "+EnvToken+")")
	pf.BoolVar(&a.insecure, "allow-insecure-http", false, "allow plain http to a non-loopback server (the token crosses the network in clear text; env "+EnvInsecure+"=1)")
	pf.StringVarP(&a.output, "output", "o", "", "output format: table, json or yaml (env "+EnvOutput+"; default table)")
	pf.DurationVar(&a.timeout, "timeout", 60*time.Second, "timeout of one HTTP request")
}

func (a *App) format() (string, error) {
	f := a.output
	if f == "" {
		f = a.getenv(EnvOutput)
	}
	switch strings.ToLower(f) {
	case "", FormatTable:
		return FormatTable, nil
	case FormatJSON:
		return FormatJSON, nil
	case FormatYAML, "yml":
		return FormatYAML, nil
	}
	return "", usageErrorf("--output must be table, json or yaml, got %q", f)
}

func (a *App) loadSpec() (*Spec, error) {
	a.specOnce.Do(func() {
		if a.SpecSource == nil {
			a.specErr = errors.New("no API document")
			return
		}
		doc, err := a.SpecSource()
		if err != nil {
			a.specErr = err
			return
		}
		a.spec, a.specErr = ParseSpec(doc)
	})
	return a.spec, a.specErr
}

// token reads the API token: --token-file, then SPACEMAN_TOKEN_FILE, then SPACEMAN_TOKEN. It is never a flag value.
func (a *App) token() (string, error) {
	path := a.tokenFile
	if path == "" {
		path = a.getenv(EnvTokenFile)
	}
	if path != "" {
		if path != "-" && runtime.GOOS != "windows" {
			if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
				return "", &AuthError{Msg: fmt.Sprintf("token file %s is readable by other users (mode %04o); chmod 600 it", path, st.Mode().Perm())}
			}
		}
		b, err := a.readFile(path)
		if err != nil {
			return "", &AuthError{Msg: "read token file: " + err.Error()}
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(a.getenv(EnvToken)), nil
}

func (a *App) apiClient(anonymous bool) (*Client, error) {
	if a.client != nil {
		return a.client, nil
	}
	server := a.server
	if server == "" {
		server = a.getenv(EnvServer)
	}
	tok, err := a.token()
	if err != nil {
		return nil, err
	}
	insecure := a.insecure || a.getenv(EnvInsecure) == "1" || strings.EqualFold(a.getenv(EnvInsecure), "true")
	ua := a.UserAgent
	cl, err := NewClient(server, tok, ClientOptions{AllowInsecureHTTP: insecure, Timeout: a.timeout, Transport: a.Transport, UserAgent: ua,
		Anonymous: anonymous})
	if err != nil {
		return nil, err
	}
	if tok != "" {
		a.client = cl
	}
	return cl, nil
}

// call is one API call by operation ID.
type call struct {
	op      string
	params  map[string]string
	query   url.Values
	body    any
	ifMatch string
	idemKey string
	// raw replaces the JSON body (a multipart upload) and is sent with rawType.
	raw     []byte
	rawType string
	// anonymous calls may go without a token (public operations).
	anonymous bool
}

// do performs a call: it resolves the operation, substitutes path parameters, adds an Idempotency-Key to creates that
// accept one and If-Match where given.
func (a *App) do(ctx context.Context, c call) (*Response, error) {
	spec, err := a.loadSpec()
	if err != nil {
		return nil, err
	}
	op, err := spec.Op(c.op)
	if err != nil {
		return nil, err
	}
	path, err := op.Expand(c.params)
	if err != nil {
		return nil, err
	}
	cl, err := a.apiClient(c.anonymous)
	if err != nil {
		return nil, err
	}
	r := Request{Method: op.Method, Path: path, Query: c.query, Body: c.body, Op: op.ID, Raw: c.raw, ContentType: c.rawType}
	if op.Headers["Idempotency-Key"] {
		r.IdemKey = c.idemKey
		if r.IdemKey == "" {
			r.IdemKey = NewIdempotencyKey()
		}
	}
	if c.ifMatch != "" {
		if !op.Headers["If-Match"] {
			return nil, fmt.Errorf("operation %s takes no If-Match", op.ID)
		}
		r.IfMatch = c.ifMatch
	}
	return cl.Do(ctx, r)
}

var (
	reSpaceID = regexp.MustCompile(`^spc_[0-9a-z]{26}$`)
	reNodeID  = regexp.MustCompile(`^nod_[0-9a-z]{26}$`)
)

// resolveSpace accepts a space ID or slug.
func (a *App) resolveSpace(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", usageErrorf("--space is required (a space ID or slug; env %s)", EnvSpace)
	}
	if reSpaceID.MatchString(ref) {
		return ref, nil
	}
	return a.resolveIn(ctx, "list-spaces", ref, "slug", "space")
}

// resolveNode accepts a node ID or name.
func (a *App) resolveNode(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || reNodeID.MatchString(ref) {
		return ref, nil
	}
	return a.resolveIn(ctx, "list-nodes", ref, "name", "node")
}

func (a *App) resolveIn(ctx context.Context, op, ref, key, what string) (string, error) {
	res, err := a.do(ctx, call{op: op})
	if err != nil {
		return "", err
	}
	obj, err := res.Object()
	if err != nil {
		return "", err
	}
	items, _ := obj["items"].([]any)
	var found []string
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m[key] == ref {
			found = append(found, fmt.Sprint(m["id"]))
		}
	}
	switch len(found) {
	case 0:
		return "", &APIError{Status: http.StatusNotFound, Code: "not_found", Detail: fmt.Sprintf("no %s with %s %q", what, key, ref)}
	case 1:
		return found[0], nil
	}
	return "", usageErrorf("%d %ss are named %q; use the ID", len(found), what, ref)
}
