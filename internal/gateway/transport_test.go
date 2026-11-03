package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// fakeTransport connects every session to an in-process MCP server over the go-sdk's in-memory transport: no HTTP,
// no network. It records the server and the headers each session was opened with.
type fakeTransport struct {
	t       *testing.T
	mu      sync.Mutex
	opened  []string
	headers []map[string]string
}

func (f *fakeTransport) Name() string { return "fake" }

func (f *fakeTransport) Client(srv db.McpServer, headers map[string]string) mcp.Transport {
	f.mu.Lock()
	f.opened = append(f.opened, srv.Slug)
	f.headers = append(f.headers, headers)
	f.mu.Unlock()
	s := mcp.NewServer(&mcp.Implementation{Name: "in-memory-upstream", Version: "1.0"}, nil)
	type in struct {
		Text string `json:"text"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "shout", Description: "Upper-case text.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, v in) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.ToUpper(v.Text)}}}, nil, nil
		})
	ct, st := mcp.NewInMemoryTransports()
	if _, err := s.Connect(context.Background(), st, nil); err != nil {
		f.t.Errorf("connect the in-memory upstream: %v", err)
	}
	return ct
}

// SM-ARCH-003: the gateway reaches upstream servers only through its Transport. With a fake transport in place of
// streamable HTTP, a granted run lists the upstream's tools, calls one over its own session (opened once, with the
// server's stored headers), and Check discovers the server; the default is streamable HTTP.
func TestGatewayUsesTransportInterface(t *testing.T) {
	if (StreamableHTTP{}).Name() != "streamable_http" {
		t.Fatal("default transport")
	}
	if New(nil, nil, nil, nil, Options{}).transport.Name() != "streamable_http" {
		t.Fatal("the default transport is not streamable HTTP")
	}
	base := newFx(t)
	tr := &fakeTransport{t: t}
	svc := New(base.s, base.svc.kr, base.svc.audit, base.svc.changes, Options{Transport: tr})
	t.Cleanup(svc.Close)
	f := &fx{t: t, ctx: base.ctx, s: base.s, svc: svc}
	// The URL is never dialed: the fake transport ignores it.
	s := f.server(Input{Slug: "mem", URL: "https://upstream.invalid/mcp", Headers: map[string]string{"Authorization": "Bearer t0ken"}})
	_, err := svc.AddGrant(f.ctx, admin, s.ID, "spc_a", "*", nil)
	must(t, err)
	p := principal("run_01aaaaaaaaaaaaaaaaaaaaaaaa", "spc_a", "implementor")
	cat := svc.Catalog(f.ctx, p)
	if len(cat) != 1 || cat[0].Name != "mem__shout" || !cat[0].ReadOnly || !cat[0].Upstream {
		t.Fatalf("catalog %+v", cat)
	}
	out, err := svc.Call(f.ctx, p, "mem__shout", json.RawMessage(`{"text":"over memory"}`))
	must(t, err)
	if !strings.Contains(string(out), "OVER MEMORY") {
		t.Fatalf("result %s", out)
	}
	if _, err := svc.Call(f.ctx, p, "mem__missing", nil); err == nil {
		t.Fatal("unknown upstream tool")
	}
	tr.mu.Lock()
	opened, hdr := len(tr.opened), tr.headers[0]["Authorization"]
	tr.mu.Unlock()
	if opened != 1 || hdr != "Bearer t0ken" {
		t.Fatalf("sessions opened %d, header %q", opened, hdr)
	}
	d, err := svc.Check(f.ctx, s.ID)
	must(t, err)
	if !d.OK || d.Server != "in-memory-upstream 1.0" || len(d.Tools) != 1 || !d.Tools[0].Usable {
		t.Fatalf("check %+v", d)
	}
}
