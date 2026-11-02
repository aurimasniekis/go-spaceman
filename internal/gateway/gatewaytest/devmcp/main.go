// Command devmcp is a small MCP server for the local development stack (scripts/dev-stack.sh): a fake ticket tracker
// served over streamable HTTP on 127.0.0.1, so the gateway can be shown and tested without any external service.
// It requires "Authorization: Bearer <token>" when -token is set. Not part of any release.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ticket struct{ ID, Title, State string }

var tickets = []ticket{
	{"PAY-412", "Refunds stuck in pending after the bank window", "open"},
	{"PAY-409", "Settlement retry budget exceeded on weekends", "in progress"},
	{"PAY-388", "Export refunds as CSV for finance", "done"},
}

func main() {
	port := flag.Int("port", 38431, "listen port on 127.0.0.1")
	token := flag.String("token", "", "required bearer token (empty: none)")
	urlFile := flag.String("url-file", "", "write the endpoint URL here once listening")
	flag.Parse()

	s := mcp.NewServer(&mcp.Implementation{Name: "dev-tickets", Version: "0.1.0"}, nil)
	type getIn struct {
		ID string `json:"id" jsonschema:"ticket ID such as PAY-412"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "ticket_get", Description: "Read one ticket by ID.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, in getIn) (*mcp.CallToolResult, any, error) {
			for _, t := range tickets {
				if strings.EqualFold(t.ID, in.ID) {
					return text(fmt.Sprintf("%s [%s] %s", t.ID, t.State, t.Title)), nil, nil
				}
			}
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "no ticket " + in.ID}}}, nil, nil
		})
	type searchIn struct {
		Query string `json:"query" jsonschema:"words to look for in titles"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "ticket_search", Description: "Search tickets by title.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, any, error) {
			var out []string
			for _, t := range tickets {
				if strings.Contains(strings.ToLower(t.Title), strings.ToLower(in.Query)) {
					out = append(out, fmt.Sprintf("%s [%s] %s", t.ID, t.State, t.Title))
				}
			}
			return text(strings.Join(out, "\n")), nil, nil
		})
	type commentIn struct {
		ID   string `json:"id"`
		Body string `json:"body"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "ticket_comment", Description: "Add a comment to a ticket."},
		func(_ context.Context, _ *mcp.CallToolRequest, in commentIn) (*mcp.CallToolResult, any, error) {
			return text("commented on " + in.ID), nil, nil
		})

	h := http.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	if *token != "" {
		inner := h
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+*token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			inner.ServeHTTP(w, r)
		})
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fail(err)
	}
	if *urlFile != "" {
		if err := os.WriteFile(*urlFile, []byte("http://"+ln.Addr().String()+"/mcp\n"), 0o600); err != nil {
			fail(err)
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", h)
	fail(http.Serve(ln, mux)) //nolint:gosec // development tool on loopback
}

func fail(err error) {
	slog.Error("devmcp", slog.Any("error", err))
	os.Exit(1)
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}
