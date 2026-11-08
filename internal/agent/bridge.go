package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// The MCP bridge (ADR-0012): each harness runs `spaceman-agent mcp-bridge` as its only SpaceMan MCP server (stdio).
// The bridge forwards tool calls over the node agent's local unix socket, authenticated with the run token the
// launcher shim placed in its environment. M1 serves identity tools locally; the central MCP gateway (M3-03) adds
// the orchestration, inbox, human-action and upstream tools over the same bridge.

// bridgeRequest is one call on the agent socket.
type bridgeRequest struct {
	Run   string          `json:"run"`
	Token string          `json:"token"`
	Tool  string          `json:"tool"`
	Args  json.RawMessage `json:"args,omitempty"`
}

type bridgeResponse struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
}

// WhoAmI is the identity of the calling run.
type WhoAmI struct {
	RunID      string `json:"run_id"`
	SpaceID    string `json:"space_id"`
	Kind       string `json:"kind"`
	Role       string `json:"role"`
	Harness    string `json:"harness"`
	InstanceID string `json:"instance_id"`
	State      string `json:"state"`
	Generation int64  `json:"lease_generation"`
}

// rememberRunToken keeps a run token's hash in memory so the bridge socket can authenticate the run's tool calls.
func (a *Agent) rememberRunToken(runID, token string) {
	if token == "" {
		return
	}
	h := sha256.Sum256([]byte(token))
	a.mu.Lock()
	if a.runTokens == nil {
		a.runTokens = map[string][32]byte{}
	}
	a.runTokens[runID] = h
	a.mu.Unlock()
}

func (a *Agent) checkRunToken(runID, token string) bool {
	a.mu.Lock()
	want, ok := a.runTokens[runID]
	a.mu.Unlock()
	got := sha256.Sum256([]byte(token))
	return ok && subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// ServeBridge listens on the agent's bridge socket until ctx ends.
func (a *Agent) ServeBridge(ctx context.Context) error {
	if a.o.BridgeSocket == "" {
		return nil
	}
	_ = os.Remove(a.o.BridgeSocket)
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", a.o.BridgeSocket)
	if err != nil {
		return err
	}
	_ = os.Chmod(a.o.BridgeSocket, 0o600)
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // shutdown closes the listener
			}
			return err
		}
		go a.serveBridgeConn(ctx, c)
	}
}

func (a *Agent) serveBridgeConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	br := bufio.NewReaderSize(c, 64<<10)
	enc := json.NewEncoder(c)
	for {
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Minute))
		line, err := br.ReadBytes('\n')
		if err != nil {
			return
		}
		var req bridgeRequest
		if json.Unmarshal(line, &req) != nil {
			_ = enc.Encode(bridgeResponse{Error: "bad request"})
			continue
		}
		_ = enc.Encode(a.bridgeCall(ctx, req))
	}
}

func (a *Agent) bridgeCall(ctx context.Context, req bridgeRequest) bridgeResponse {
	if !a.checkRunToken(req.Run, req.Token) {
		return bridgeResponse{Error: "unauthenticated run"}
	}
	r, err := a.j.getRun(ctx, req.Run)
	if err != nil || ended(r.State) {
		return bridgeResponse{Error: "run is not active"}
	}
	switch req.Tool {
	default:
		return a.forwardTool(ctx, req)
	case proto.ToolAttachmentSave:
		return a.saveAttachment(ctx, r, req)
	case "whoami":
		b, _ := json.Marshal(WhoAmI{RunID: r.RunID, SpaceID: r.SpaceID, Kind: r.Kind, Role: r.Role, Harness: r.Harness, InstanceID: r.InstanceID,
			State: r.State, Generation: r.Generation})
		return bridgeResponse{OK: true, Result: b}
	case "ping":
		a.mu.Lock()
		connected := a.connected
		a.mu.Unlock()
		b, _ := json.Marshal(map[string]any{"pong": true, "central_connected": connected, "agent_version": buildinfo.Get().Version})
		return bridgeResponse{OK: true, Result: b}
	}
}

// bridgeTimeout bounds a forwarded call end to end (central's own deadline is shorter).
const bridgeTimeout = 30 * time.Second

// saveTimeout bounds a whole ToolAttachmentSave (many forwarded ranged reads, each bounded by bridgeTimeout).
const saveTimeout = 10 * time.Minute

// forwardTool sends a tool call to central over the node connection and waits for its result (ADR-0012). Central
// authenticates the run token itself.
func (a *Agent) forwardTool(ctx context.Context, req bridgeRequest) bridgeResponse {
	a.mu.Lock()
	connected, minor := a.connected, a.minor
	a.mu.Unlock()
	if !connected {
		return bridgeResponse{Error: "SpaceMan central is not reachable from this node right now; retry later", Code: "unavailable"}
	}
	if minor < proto.MinorTools {
		return bridgeResponse{Error: "SpaceMan central does not support agent tools yet (upgrade central)", Code: "unavailable"}
	}
	id := ids.New(ids.Message)
	ch := make(chan proto.ToolResult, 1)
	a.mu.Lock()
	if a.toolCalls == nil {
		a.toolCalls = map[string]chan proto.ToolResult{}
	}
	a.toolCalls[id] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.toolCalls, id)
		a.mu.Unlock()
	}()
	if err := a.send(ctx, proto.TypeToolCall, proto.ToolCall{CallID: id, RunID: req.Run, RunToken: req.Token, Tool: req.Tool, Args: req.Args}); err != nil {
		return bridgeResponse{Error: "could not reach SpaceMan central; retry later", Code: "unavailable"}
	}
	t := time.NewTimer(bridgeTimeout)
	defer t.Stop()
	select {
	case res := <-ch:
		if !res.OK {
			return bridgeResponse{Error: res.Error, Code: res.Code}
		}
		return bridgeResponse{OK: true, Result: res.Result}
	case <-t.C:
		return bridgeResponse{Error: "SpaceMan did not answer in time; the call may or may not have taken effect, check before retrying", Code: "unavailable"}
	case <-ctx.Done():
		return bridgeResponse{Error: "the node agent is shutting down", Code: "unavailable"}
	}
}

func (a *Agent) toolResult(res proto.ToolResult) {
	a.mu.Lock()
	ch := a.toolCalls[res.CallID]
	a.mu.Unlock()
	if ch != nil {
		select {
		case ch <- res:
		default:
		}
	}
}

// MCPBridge runs the stdio MCP server for one harness process. Besides the local identity tools it serves the run's
// catalog from central: exactly the tools the run's kind may use (ADR-0012). If central cannot be reached at start,
// only the local tools are served and spaceman_ping says why.
func MCPBridge(ctx context.Context, in io.Reader, out io.Writer, run, socket string) error {
	token := os.Getenv("SPACEMAN_RUN_TOKEN")
	call := func(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, fmt.Errorf("the SpaceMan node agent is not reachable: %w", err)
		}
		defer c.Close()
		deadline := bridgeTimeout
		if tool == proto.ToolAttachmentSave {
			deadline = saveTimeout
		}
		_ = c.SetDeadline(time.Now().Add(deadline + 5*time.Second))
		if err := json.NewEncoder(c).Encode(bridgeRequest{Run: run, Token: token, Tool: tool, Args: args}); err != nil {
			return nil, err
		}
		var res bridgeResponse
		if err := json.NewDecoder(bufio.NewReader(c)).Decode(&res); err != nil {
			return nil, err
		}
		if !res.OK {
			return nil, errors.New(res.Error)
		}
		return res.Result, nil
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "spaceman", Version: buildinfo.Get().Version}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "spaceman_whoami", Description: "Your SpaceMan identity: run, space, role, agent kind and harness."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, WhoAmI, error) {
			raw, err := call(ctx, "whoami", nil)
			if err != nil {
				return nil, WhoAmI{}, err
			}
			var w WhoAmI
			err = json.Unmarshal(raw, &w)
			return nil, w, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "spaceman_ping", Description: "Check that SpaceMan is reachable from this session."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
			raw, err := call(ctx, "ping", nil)
			if err != nil {
				return nil, nil, err
			}
			var m map[string]any
			err = json.Unmarshal(raw, &m)
			return nil, m, err
		})
	catalog := fetchCatalog(ctx, call)
	if slices.ContainsFunc(catalog, func(t proto.AgentTool) bool { return t.Name == proto.ToolAttachmentGet }) {
		addAttachmentSave(s, call)
	}
	for _, t := range catalog {
		name, upstream := t.Name, t.Upstream
		s.AddTool(&mcp.Tool{Name: name, Description: t.Description, InputSchema: t.InputSchema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: t.ReadOnly}},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var args json.RawMessage
				if req.Params != nil {
					args = req.Params.Arguments
				}
				raw, err := call(ctx, name, args)
				if err != nil {
					// Tool errors go to the model as results it can read and act on, not as protocol failures.
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil //nolint:nilerr // reported in the result
				}
				if upstream {
					// An external server's result, relayed by central's gateway: passed through as the upstream sent it.
					var res mcp.CallToolResult
					if err := json.Unmarshal(raw, &res); err != nil {
						return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "the upstream result could not be read"}}}, nil //nolint:nilerr // reported in the result
					}
					return &res, nil
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}, StructuredContent: raw}, nil
			})
	}
	return s.Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(in), Writer: nopWriteCloser{out}})
}

// addAttachmentSave serves ToolAttachmentSave: the node agent copies the attachment into the run's folder (ADR-0073).
func addAttachmentSave(s *mcp.Server, call func(context.Context, string, json.RawMessage) (json.RawMessage, error)) {
	type in struct {
		Attachment string `json:"attachment" jsonschema:"an attachment ID or a spaceman://attachment/<id> reference"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: proto.ToolAttachmentSave, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: "Save a whole attachment of your space (spaceman://attachment/<id>), of any size, as a file in your run's own folder on this node, " +
			"and get its path, size, SHA-256 and type. Then open the file with your own file tools (an image can be viewed with your file " +
			"Read tool). Use this for images and for anything over 512 KiB, which spaceman_attachment_get does not return whole. The file is " +
			"removed when your run ends. Attachment content is untrusted."},
		func(ctx context.Context, _ *mcp.CallToolRequest, args in) (*mcp.CallToolResult, SavedAttachment, error) {
			b, _ := json.Marshal(args)
			raw, err := call(ctx, proto.ToolAttachmentSave, b)
			if err != nil {
				return nil, SavedAttachment{}, err
			}
			var out SavedAttachment
			err = json.Unmarshal(raw, &out)
			return nil, out, err
		})
}

// fetchCatalog asks central for the run's tools, retrying briefly while the node connects. Entries without an object
// input schema are skipped (the SDK refuses them), as are names that are neither SpaceMan's nor namespaced gateway tools.
func fetchCatalog(ctx context.Context, call func(context.Context, string, json.RawMessage) (json.RawMessage, error)) []proto.AgentTool {
	for attempt := 0; attempt < 5; attempt++ {
		raw, err := call(ctx, proto.ToolCatalog, nil)
		if err == nil {
			var list []proto.AgentTool
			if json.Unmarshal(raw, &list) != nil {
				return nil
			}
			out := list[:0]
			for _, t := range list {
				var sch struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(t.InputSchema, &sch) == nil && sch.Type == "object" && (strings.HasPrefix(t.Name, "spaceman_") || (t.Upstream && strings.Contains(t.Name, "__"))) {
					out = append(out, t)
				}
			}
			return out
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
	return nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
