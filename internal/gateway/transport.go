package gateway

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Transport is how the gateway reaches an upstream MCP server (SM-ARCH-003): it turns a registered server and its
// opened headers into a go-sdk client transport, over which the runtime opens one session per run and server.
// Streamable HTTP is the only transport compiled in: servers are registered with a streamable HTTP endpoint, and the
// gateway speaks no stdio or SSE-only transport (LIMITATIONS).
type Transport interface {
	// Name identifies the transport.
	Name() string
	// Client returns a transport for one session with srv that sends headers with every request. It must dial only
	// what the egress policy allows and bound responses by the server's result limit.
	Client(srv db.McpServer, headers map[string]string) mcp.Transport
}

// StreamableHTTP is the MCP streamable HTTP transport (the go-sdk's StreamableClientTransport) through the gateway's
// egress-checked HTTP client, without the standalone SSE stream and without SDK retries.
type StreamableHTTP struct{}

// Name implements Transport.
func (StreamableHTTP) Name() string { return "streamable_http" }

// Client implements Transport.
func (StreamableHTTP) Client(srv db.McpServer, headers map[string]string) mcp.Transport {
	return &mcp.StreamableClientTransport{Endpoint: srv.Url, HTTPClient: httpClient(srv.AllowPrivate == 1, headers, srv.MaxResultBytes+(64<<10)),
		MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: int(srv.MaxResultBytes) + (64 << 10)}
}
