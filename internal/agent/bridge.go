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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
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
	return bridgeResponse{Error: "unknown tool"}
}

// MCPBridge runs the stdio MCP server for one harness process.
func MCPBridge(ctx context.Context, in io.Reader, out io.Writer, run, socket string) error {
	token := os.Getenv("SPACEMAN_RUN_TOKEN")
	call := func(ctx context.Context, tool string) (json.RawMessage, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, fmt.Errorf("the SpaceMan node agent is not reachable: %w", err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(30 * time.Second))
		if err := json.NewEncoder(c).Encode(bridgeRequest{Run: run, Token: token, Tool: tool}); err != nil {
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
			raw, err := call(ctx, "whoami")
			if err != nil {
				return nil, WhoAmI{}, err
			}
			var w WhoAmI
			err = json.Unmarshal(raw, &w)
			return nil, w, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "spaceman_ping", Description: "Check that SpaceMan is reachable from this session."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
			raw, err := call(ctx, "ping")
			if err != nil {
				return nil, nil, err
			}
			var m map[string]any
			err = json.Unmarshal(raw, &m)
			return nil, m, err
		})
	return s.Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(in), Writer: nopWriteCloser{out}})
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
