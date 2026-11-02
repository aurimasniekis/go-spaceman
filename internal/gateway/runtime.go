package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

const (
	maxArgsBytes   = 256 << 10
	maxListedTools = 500
	listTimeout    = 8 * time.Second
)

// runtime holds the open upstream sessions. A session belongs to exactly one run and one server: upstream sessions may
// hold state (logins, cursors, open files), so they are never shared between runs or reused by another run.
type runtime struct {
	svc         *Service
	log         *slog.Logger
	maxSessions int
	idle        time.Duration

	mu       sync.Mutex
	sessions map[sessKey]*session
	sems     map[string]chan struct{} // server ID → in-flight call slots
	semSize  map[string]int64
}

type sessKey struct{ run, server string }

type session struct {
	key     sessKey
	spaceID string
	stamp   int64 // the server's version when opened; a changed server reopens the session
	ready   chan struct{}
	cs      *mcp.ClientSession
	tools   map[string]*mcp.Tool
	err     error
	timer   *time.Timer
}

func newRuntime(svc *Service, o Options) *runtime {
	if o.MaxSessions <= 0 {
		o.MaxSessions = 256
	}
	if o.IdleClose <= 0 {
		o.IdleClose = 10 * time.Minute
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &runtime{svc: svc, log: o.Log, maxSessions: o.MaxSessions, idle: o.IdleClose, sessions: map[sessKey]*session{},
		sems: map[string]chan struct{}{}, semSize: map[string]int64{}}
}

// open returns the run's session with the server, connecting on first use. Concurrent callers share one attempt.
func (rt *runtime) open(ctx context.Context, p *fleet.RunPrincipal, srv db.McpServer) (*session, error) {
	key := sessKey{p.RunID, srv.ID}
	rt.mu.Lock()
	s, ok := rt.sessions[key]
	if ok && s.stamp != srv.Version {
		rt.dropLocked(s)
		ok = false
	}
	if !ok {
		if len(rt.sessions) >= rt.maxSessions {
			rt.mu.Unlock()
			return nil, apperr.New(apperr.CodeRateLimited, "the MCP gateway has too many open upstream sessions; retry later")
		}
		s = &session{key: key, spaceID: p.SpaceID, stamp: srv.Version, ready: make(chan struct{})}
		rt.sessions[key] = s
		rt.mu.Unlock()
		s.cs, s.tools, s.err = rt.svc.connect(ctx, srv)
		if s.err == nil {
			s.timer = time.AfterFunc(rt.idle, func() { rt.drop(s) })
		}
		close(s.ready)
		if s.err != nil {
			rt.mu.Lock()
			if rt.sessions[key] == s {
				delete(rt.sessions, key)
			}
			rt.mu.Unlock()
			return nil, s.err
		}
		return s, nil
	}
	rt.mu.Unlock()
	select {
	case <-s.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	s.timer.Reset(rt.idle)
	return s, nil
}

func (rt *runtime) drop(s *session) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.dropLocked(s)
}

// dropLocked forgets a session and closes it in the background (Close may wait on the network).
func (rt *runtime) dropLocked(s *session) {
	if rt.sessions[s.key] == s {
		delete(rt.sessions, s.key)
	}
	go func() {
		<-s.ready
		if s.timer != nil {
			s.timer.Stop()
		}
		if s.cs != nil {
			_ = s.cs.Close()
		}
	}()
}

func (rt *runtime) closeWhere(match func(*session) bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, s := range rt.sessions {
		if match(s) {
			rt.dropLocked(s)
		}
	}
}

// EndRun closes a run's upstream sessions (the run ended).
func (svc *Service) EndRun(runID string) {
	svc.rt.closeWhere(func(s *session) bool { return s.key.run == runID })
}

func (rt *runtime) closeServer(serverID string) {
	rt.closeWhere(func(s *session) bool { return s.key.server == serverID })
}

func (rt *runtime) closeGrant(serverID, spaceID string) {
	rt.closeWhere(func(s *session) bool { return s.key.server == serverID && s.spaceID == spaceID })
}

// Close closes every session.
func (svc *Service) Close() { svc.rt.closeWhere(func(*session) bool { return true }) }

// Sessions returns the number of open sessions.
func (svc *Service) Sessions() int {
	svc.rt.mu.Lock()
	defer svc.rt.mu.Unlock()
	return len(svc.rt.sessions)
}

// slot takes one of the server's in-flight call slots, waiting until ctx is done.
func (rt *runtime) slot(ctx context.Context, srv db.McpServer) (func(), error) {
	rt.mu.Lock()
	ch, ok := rt.sems[srv.ID]
	if !ok || rt.semSize[srv.ID] != srv.MaxConcurrency {
		ch = make(chan struct{}, srv.MaxConcurrency)
		rt.sems[srv.ID], rt.semSize[srv.ID] = ch, srv.MaxConcurrency
	}
	rt.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, apperr.New(apperr.CodeRateLimited, "%s is at its limit of %d concurrent calls; retry later", srv.Slug, srv.MaxConcurrency)
	}
}

// connect opens an upstream session and lists its tools. The client declares no capabilities, so the upstream cannot
// ask the agent for sampling, elicitation or roots.
func (svc *Service) connect(ctx context.Context, srv db.McpServer) (*mcp.ClientSession, map[string]*mcp.Tool, error) {
	headers, err := svc.headers(srv)
	if err != nil {
		return nil, nil, fmt.Errorf("open headers of %s: %w", srv.Slug, err)
	}
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	tr := &mcp.StreamableClientTransport{Endpoint: srv.Url, HTTPClient: httpClient(srv.AllowPrivate == 1, headers, srv.MaxResultBytes+(64<<10)),
		MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: int(srv.MaxResultBytes) + (64 << 10)}
	c := mcp.NewClient(&mcp.Implementation{Name: "spaceman-gateway", Version: buildinfo.Get().Version}, nil)
	cs, err := c.Connect(ctx, tr, nil)
	if err != nil {
		return nil, nil, err
	}
	tools := map[string]*mcp.Tool{}
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			_ = cs.Close()
			return nil, nil, err
		}
		if len(tools) >= maxListedTools {
			break
		}
		tools[t.Name] = t
	}
	return cs, tools, nil
}

// connectError describes a failure to open a session: whatever the SDK reports, the server is unavailable (or refused
// by policy, or too slow).
func connectError(ctx context.Context, slug string, err error) error {
	if _, ok := answered(err); ok {
		return apperr.New(apperr.CodeUnavailable, "%s: the upstream server refused the session", slug)
	}
	return upstreamError(ctx, slug, err)
}

// upstreamError turns an upstream failure into what the agent sees: no addresses or internals, just which server and
// what kind of failure.
func upstreamError(ctx context.Context, slug string, err error) error {
	switch {
	case errors.Is(err, ErrEgressDenied):
		return apperr.New(apperr.CodeForbidden, "%s: the egress policy does not allow its address", slug)
	case errors.Is(err, ErrTooLarge):
		return apperr.New(apperr.CodeInvalid, "%s: the response is larger than the gateway allows", slug)
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
		return apperr.New(apperr.CodeUnavailable, "%s: the upstream server did not answer in time", slug)
	}
	if we, ok := answered(err); ok {
		return apperr.New(apperr.CodeInvalid, "%s: %s", slug, truncate(we.Message, 500))
	}
	return apperr.New(apperr.CodeUnavailable, "%s: the upstream server is unavailable", slug)
}

// codeRejected is the go-sdk's JSON-RPC code for a message its transport could not deliver.
const codeRejected = -32005

// answered returns the JSON-RPC error the upstream server itself sent; transport failures the SDK reports as JSON-RPC
// errors do not count.
func answered(err error) (*jsonrpc.Error, bool) {
	var we *jsonrpc.Error
	if errors.As(err, &we) && we.Code != codeRejected {
		return we, true
	}
	return nil, false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Catalog lists the upstream tools a run may use, namespaced "<slug>__<tool>". A server that cannot be reached is left
// out (and logged); the run's built-in tools are unaffected.
func (svc *Service) Catalog(ctx context.Context, p *fleet.RunPrincipal) []proto.AgentTool {
	gs, err := grantsFor(ctx, svc.s.R(), p.SpaceID, p.Role)
	if err != nil || len(gs) == 0 {
		return nil
	}
	type res struct {
		i     int
		tools []proto.AgentTool
	}
	ch := make(chan res, len(gs))
	for i, g := range gs {
		go func() {
			s, err := svc.rt.open(ctx, p, g.server)
			if err != nil {
				svc.rt.log.Warn("MCP server unavailable for a run", slog.String("mcp.server", g.server.Slug), slog.String("run.id", p.RunID), slog.Any("error", err))
				ch <- res{i: i}
				return
			}
			var out []proto.AgentTool
			for name, t := range s.tools {
				full := g.server.Slug + Separator + name
				if !g.allows(name) || !reToolName.MatchString(name) || len(full) > MaxToolName {
					continue
				}
				schema, err := json.Marshal(t.InputSchema)
				if err != nil {
					continue
				}
				desc := t.Description
				if t.Title != "" && desc == "" {
					desc = t.Title
				}
				ro := t.Annotations != nil && t.Annotations.ReadOnlyHint
				out = append(out, proto.AgentTool{Name: full, Description: "[" + g.server.Name + "] " + truncate(desc, 2000), InputSchema: schema,
					ReadOnly: ro, Upstream: true})
			}
			ch <- res{i: i, tools: out}
		}()
	}
	parts := make([][]proto.AgentTool, len(gs))
	for range gs {
		r := <-ch
		parts[r.i] = r.tools
	}
	var all []proto.AgentTool
	for _, p := range parts {
		all = append(all, p...)
	}
	return all
}

// Owns reports whether a tool name is a gateway tool.
func (svc *Service) Owns(name string) bool { return Owns(name) }

// Owns reports whether a tool name is a gateway tool.
func Owns(name string) bool {
	return strings.Contains(name, Separator) && !strings.HasPrefix(name, "spaceman_")
}

// Call runs one upstream tool call for a run. The grant is checked again on every call (it may have been revoked since
// the run listed its tools); the run's own session is used; time, concurrency and size are bounded. Each call is
// audited without its arguments or result.
func (svc *Service) Call(ctx context.Context, p *fleet.RunPrincipal, name string, args json.RawMessage) (json.RawMessage, error) {
	slug, tool, ok := strings.Cut(name, Separator)
	if !ok || !reSlug.MatchString(slug) || !reToolName.MatchString(tool) {
		return nil, apperr.New(apperr.CodeNotFound, "unknown tool %q", name)
	}
	if len(args) > maxArgsBytes {
		return nil, apperr.New(apperr.CodeInvalid, "arguments larger than %d bytes", maxArgsBytes)
	}
	gs, err := grantsFor(ctx, svc.s.R(), p.SpaceID, p.Role)
	if err != nil {
		return nil, err
	}
	var g *granted
	for i := range gs {
		if gs[i].server.Slug == slug {
			g = &gs[i]
		}
	}
	if g == nil || !g.allows(tool) {
		return nil, apperr.New(apperr.CodeForbidden, "this run is not granted %s", name)
	}
	srv := g.server
	ctx, cancel := context.WithTimeout(ctx, time.Duration(srv.CallTimeoutMs)*time.Millisecond)
	defer cancel()
	release, err := svc.rt.slot(ctx, srv)
	if err != nil {
		return nil, err
	}
	defer release()
	start := time.Now()
	out, err := svc.call(ctx, p, srv, tool, args)
	outcome, result := "success", "ok"
	if err != nil {
		outcome, result = "failure", string(apperr.CodeOf(err))
	}
	svc.rt.log.Info("MCP gateway call", slog.String("run.id", p.RunID), slog.String("mcp.server", srv.Slug), slog.String("mcp.tool", tool),
		slog.String("result", result), slog.Duration("duration", time.Since(start)))
	// Audited after the call: no I/O happens inside the write transaction.
	aerr := svc.s.W().TxNamed(context.WithoutCancel(ctx), "mcp.call", func(tx *store.Tx) error {
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorAgent, ID: p.RunID}, Action: "mcp_server.tool_called",
			Target: audit.Target{Type: "mcp_server", ID: srv.ID}, Outcome: outcome,
			Details: map[string]any{"space_id": p.SpaceID, "tool": tool, "result": result, "duration_ms": time.Since(start).Milliseconds()}})
		return err
	})
	if aerr != nil {
		svc.rt.log.Error("audit MCP gateway call", slog.Any("error", aerr))
	}
	return out, err
}

func (svc *Service) call(ctx context.Context, p *fleet.RunPrincipal, srv db.McpServer, tool string, args json.RawMessage) (json.RawMessage, error) {
	s, err := svc.rt.open(ctx, p, srv)
	if err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			return nil, err
		}
		return nil, connectError(ctx, srv.Slug, err)
	}
	if _, ok := s.tools[tool]; !ok {
		return nil, apperr.New(apperr.CodeNotFound, "%s does not offer %s", srv.Slug, tool)
	}
	var a any
	if len(args) > 0 {
		a = args
	}
	res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: a})
	if err != nil {
		if _, ok := answered(err); !ok {
			// A transport failure, timeout or oversized response may leave the session unusable: the next call opens a
			// new one.
			svc.rt.drop(s)
		}
		return nil, upstreamError(ctx, srv.Slug, err)
	}
	out, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > srv.MaxResultBytes {
		return nil, apperr.New(apperr.CodeInvalid, "%s: the result (%d bytes) is larger than the %d bytes this server allows", srv.Slug, len(out), srv.MaxResultBytes)
	}
	return out, nil
}

// Discovered is what a connection check found.
type Discovered struct {
	OK      bool             `json:"ok"`
	Error   string           `json:"error,omitempty"`
	Server  string           `json:"server,omitempty" doc:"The upstream's name and version"`
	Tools   []DiscoveredTool `json:"tools"`
	Elapsed int64            `json:"elapsed_ms"`
}

// DiscoveredTool is one upstream tool.
type DiscoveredTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ReadOnly    bool   `json:"read_only"`
	Usable      bool   `json:"usable" doc:"False when the namespaced name would be too long or has characters models refuse"`
}

// Check connects to a server with a throwaway session, lists its tools and closes the session.
func (svc *Service) Check(ctx context.Context, id string) (*Discovered, error) {
	srv, err := svc.s.R().GetMCPServer(ctx, id)
	if err != nil {
		return nil, errNotFound
	}
	start := time.Now()
	out := &Discovered{Tools: []DiscoveredTool{}}
	cs, tools, err := svc.connect(ctx, srv)
	out.Elapsed = time.Since(start).Milliseconds()
	if err != nil {
		out.Error = connectError(ctx, srv.Slug, err).Error()
		return out, nil
	}
	defer cs.Close()
	out.OK = true
	if r := cs.InitializeResult(); r != nil && r.ServerInfo != nil {
		out.Server = strings.TrimSpace(r.ServerInfo.Name + " " + r.ServerInfo.Version)
	}
	for name, t := range tools {
		out.Tools = append(out.Tools, DiscoveredTool{Name: name, Description: truncate(t.Description, 500), ReadOnly: t.Annotations != nil && t.Annotations.ReadOnlyHint,
			Usable: reToolName.MatchString(name) && len(srv.Slug+Separator+name) <= MaxToolName})
	}
	sort.Slice(out.Tools, func(i, j int) bool { return out.Tools[i].Name < out.Tools[j].Name })
	return out, nil
}
