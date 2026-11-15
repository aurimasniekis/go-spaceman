package spacemanmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/gateway"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Tokens resolves a server's internal token (auth.Service).
type Tokens interface {
	InternalPrincipal(ctx context.Context, tokenID, runID string) (*auth.Principal, error)
	GetAPIToken(ctx context.Context, id string) (*auth.APIToken, error)
	Get(ctx context.Context, userID string) (*auth.User, error)
}

// Backend serves SpaceMan MCP servers to the MCP gateway (gateway.SpacemanBackend, ADR-0063): the catalog a server's
// token can use, each call's class, and the call itself through the API's in-process handler with the token's
// principal. The gateway keeps grants, run kinds, limits, approvals and audit.
type Backend struct {
	Cat *Catalog
	// Handler is api.Deps.InProcessHandler().
	Handler http.Handler
	Tokens  Tokens
}

var _ gateway.SpacemanBackend = (*Backend)(nil)

// Validate checks a server's preset and named tools: an unknown or excluded tool is refused.
func (b *Backend) Validate(s gateway.SpacemanSettings) error {
	_, err := b.Cat.Select(Selection{Preset: Preset(s.Preset), Tools: s.Tools}, nil)
	return err
}

// Tools lists every tool a server can offer, marked with what settings s and the grants make of it (the UI's tool
// picker). An unknown preset or tool name fails like Validate.
func (b *Backend) Tools(s gateway.SpacemanSettings, grants []authz.Grant) ([]gateway.SpacemanTool, error) {
	if err := b.Validate(s); err != nil {
		return nil, err
	}
	preset := Preset(s.Preset)
	if preset == "" {
		preset = DefaultPreset
	}
	all := b.Cat.Tools()
	out := make([]gateway.SpacemanTool, 0, len(all))
	for _, t := range all {
		out = append(out, gateway.SpacemanTool{Name: t.Name, Class: string(t.Class), Tags: append([]string{}, t.Tags...), Summary: t.Summary,
			InPreset: slices.Contains(preset.Classes(), t.Class), Selected: selected(s, t), Reachable: Reachable(t, grants)})
	}
	return out, nil
}

// selected reports whether a server offers a tool (before the grant filter): its preset's classes or its named tools.
func selected(s gateway.SpacemanSettings, t *Tool) bool {
	preset := Preset(s.Preset)
	if preset == "" {
		preset = DefaultPreset
	}
	return slices.Contains(preset.Classes(), t.Class) || slices.Contains(s.Tools, t.Name)
}

// usable returns the token's principal for runID and its grants, or why the server can no longer act.
func (b *Backend) usable(ctx context.Context, t gateway.SpacemanTarget, runID string) (*auth.Principal, []authz.Grant, error) {
	p, err := b.Tokens.InternalPrincipal(ctx, t.TokenID, runID)
	if err != nil {
		if !errors.Is(err, auth.ErrUnauthorized) {
			return nil, nil, err
		}
		return nil, nil, apperr.New(apperr.CodeForbidden, "the SpaceMan MCP server %q can no longer act: %s; an administrator must re-issue it",
			t.Slug, b.refusal(ctx, t.TokenID))
	}
	tok, err := b.Tokens.GetAPIToken(ctx, t.TokenID)
	if err != nil {
		return nil, nil, err
	}
	return p, tok.Grants, nil
}

// refusal names why a server's token is refused.
func (b *Backend) refusal(ctx context.Context, tokenID string) string {
	tok, err := b.Tokens.GetAPIToken(ctx, tokenID)
	switch {
	case err != nil:
		return "its token no longer exists"
	case tok.RevokedAt != nil:
		return "its token was revoked"
	}
	if u, err := b.Tokens.Get(ctx, tok.UserID); err != nil || !u.Enabled {
		return "the administrator who owns its token is disabled"
	}
	return "its token expired"
}

// Catalog lists the tools a server offers runs: its selection, less the tools its token's grants can never reach. It
// is empty when the token can no longer be used.
func (b *Backend) Catalog(ctx context.Context, t gateway.SpacemanTarget) []proto.AgentTool {
	_, grants, err := b.usable(ctx, t, "")
	if err != nil {
		return nil
	}
	tools, err := b.Cat.Select(Selection{Preset: Preset(t.Settings.Preset), Tools: t.Settings.Tools}, grants)
	if err != nil {
		return nil
	}
	out := make([]proto.AgentTool, 0, len(tools))
	for _, tl := range tools {
		desc := "[" + t.Name + "] " + tl.Description
		if tl.ApprovalRequired() {
			desc += " Every call needs a person's approval."
		}
		out = append(out, proto.AgentTool{Name: tl.FullName(t.Slug), Description: desc, InputSchema: tl.InputSchema, ReadOnly: tl.ReadOnly()})
	}
	return out
}

// Class returns the class of a call: the tool's, except that a diagnostic launch is dangerous (it runs a harness with
// diagnostic powers on a node). False when the server does not offer the tool.
func (b *Backend) Class(t gateway.SpacemanTarget, tool string, args json.RawMessage) (string, bool) {
	tl, ok := b.Cat.Tool(tool)
	if !ok || !selected(t.Settings, tl) {
		return "", false
	}
	if tl.OperationID == "launch-run" && diagnosticLaunch(args) {
		return string(Dangerous), true
	}
	return string(tl.Class), true
}

// diagnosticLaunch reports whether launch_run arguments ask for a diagnostic run. Kind values are compared the way the
// API reads them (exact and trimmed lowercase both count, so no spelling slips past the gate).
func diagnosticLaunch(args json.RawMessage) bool {
	var a struct {
		Body struct {
			Kind string `json:"kind"`
		} `json:"body"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return false // the executor refuses arguments that are not an object
	}
	return strings.EqualFold(strings.TrimSpace(a.Body.Kind), "diagnostic")
}

// Call runs one tool call with the server's token for the run. A refused token, an unknown tool or a tool the server
// does not offer is an error; API problems are tool results with isError.
func (b *Backend) Call(ctx context.Context, t gateway.SpacemanTarget, runID, tool string, args json.RawMessage) (json.RawMessage, error) {
	tl, ok := b.Cat.Tool(tool)
	if !ok || !selected(t.Settings, tl) {
		return nil, apperr.New(apperr.CodeNotFound, "%s does not offer %s", t.Slug, tool)
	}
	p, grants, err := b.usable(ctx, t, runID)
	if err != nil {
		return nil, err
	}
	if !Reachable(tl, grants) {
		return nil, apperr.New(apperr.CodeForbidden, "the SpaceMan MCP server %q's token cannot use %s", t.Slug, tool)
	}
	ex := &Executor{Handler: b.Handler, MaxResultBytes: int(t.MaxResultBytes)}
	res, err := ex.Execute(ctx, Call{Principal: p, Tool: tl, Args: args, DefaultSpaceID: defaultSpace(grants)})
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

// defaultSpace is the space a call's missing space_id means: the one space the token's grants name, if exactly one.
func defaultSpace(grants []authz.Grant) string {
	space := ""
	for _, g := range grants {
		if g.Domain == authz.Global || (space != "" && g.Domain != space) {
			return ""
		}
		space = g.Domain
	}
	return space
}
