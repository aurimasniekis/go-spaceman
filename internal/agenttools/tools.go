// Package agenttools is SpaceMan's built-in tool set for agents (SM-MCP-003, ADR-0012). Calls arrive over the node
// connection with a run credential that central authenticates itself; each tool states which run kinds may use it,
// and that grant is the only authority (agents never get Casbin user policy, and the model's word is never trusted:
// the requester, space and run come from the credential, not from arguments). Inputs are decoded strictly and
// validated by the same services the HTTP API uses.
package agenttools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

// Run kinds (fleet.LaunchInput.Kind).
const (
	KindControl    = "control"
	KindWorker     = "worker"
	KindKB         = "kb"
	KindDreamer    = "dreamer"
	KindDiagnostic = "diagnostic"
)

var (
	allKinds  = []string{KindControl, KindWorker, KindKB, KindDreamer, KindDiagnostic}
	producers = []string{KindControl, KindWorker, KindKB, KindDreamer}
	control   = []string{KindControl}
)

// tool is one registered tool.
type tool struct {
	info  proto.AgentTool
	kinds []string
	run   func(ctx context.Context, p *fleet.RunPrincipal, args json.RawMessage) (any, error)
}

// Deps are the services the tools use.
type Deps struct {
	Inbox       *inbox.Service
	Queue       *queue.Service
	Attachments *attachments.Service
	Human       *human.Service
	// Extra registers more tools (messages, orchestration) on the same registry.
	Extra []func(r *Registry)
	// Upstream serves external MCP servers' tools through the gateway (ADR-0023); nil serves none.
	Upstream Upstream
}

// Upstream is the MCP gateway as the registry sees it.
type Upstream interface {
	Catalog(ctx context.Context, p *fleet.RunPrincipal) []proto.AgentTool
	Owns(name string) bool
	Call(ctx context.Context, p *fleet.RunPrincipal, name string, args json.RawMessage) (json.RawMessage, error)
}

// Registry holds the tools.
type Registry struct {
	d     Deps
	tools map[string]tool
}

// New builds the registry with every built-in tool.
func New(d Deps) *Registry {
	r := &Registry{d: d, tools: map[string]tool{}}
	r.registerInbox()
	r.registerTasks()
	r.registerAttachments()
	r.registerHuman()
	for _, f := range d.Extra {
		f(r)
	}
	return r
}

// Add registers a tool whose input is T (decoded strictly) for the given run kinds.
func Add[T any](r *Registry, name, description string, kinds []string, readOnly bool, fn func(ctx context.Context, p *fleet.RunPrincipal, in T) (any, error)) {
	sch, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("agent tool %s: schema: %v", name, err))
	}
	raw, err := json.Marshal(sch)
	if err != nil {
		panic(fmt.Sprintf("agent tool %s: schema: %v", name, err))
	}
	r.tools[name] = tool{info: proto.AgentTool{Name: name, Description: description, InputSchema: raw, ReadOnly: readOnly}, kinds: kinds,
		run: func(ctx context.Context, p *fleet.RunPrincipal, args json.RawMessage) (any, error) {
			var in T
			if len(bytes.TrimSpace(args)) > 0 && string(bytes.TrimSpace(args)) != "null" {
				dec := json.NewDecoder(bytes.NewReader(args))
				dec.DisallowUnknownFields()
				if err := dec.Decode(&in); err != nil {
					return nil, apperr.New(apperr.CodeInvalid, "invalid arguments for %s: %v", name, err)
				}
			}
			return fn(ctx, p, in)
		}}
}

// Catalog lists the tools a run kind may use, by name.
func (r *Registry) Catalog(kind string) []proto.AgentTool {
	out := []proto.AgentTool{}
	for _, t := range r.tools {
		if slices.Contains(t.kinds, kind) {
			out = append(out, t.info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Dispatch runs one call for an authenticated run (fleet.ToolDispatcher).
func (r *Registry) Dispatch(ctx context.Context, p *fleet.RunPrincipal, name string, args json.RawMessage) (json.RawMessage, error) {
	if name == proto.ToolCatalog {
		list := r.Catalog(p.Kind)
		if r.d.Upstream != nil {
			list = append(list, r.d.Upstream.Catalog(ctx, p)...)
		}
		return json.Marshal(list)
	}
	if _, builtin := r.tools[name]; !builtin && r.d.Upstream != nil && r.d.Upstream.Owns(name) {
		return r.d.Upstream.Call(ctx, p, name, args)
	}
	t, ok := r.tools[name]
	if !ok {
		return nil, apperr.New(apperr.CodeNotFound, "unknown tool %q", name)
	}
	if !slices.Contains(t.kinds, p.Kind) {
		return nil, apperr.New(apperr.CodeForbidden, "%s runs may not use %s", p.Kind, name)
	}
	out, err := t.run(ctx, p, args)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// actor attributes an agent's action to its run in the audit log.
func actor(p *fleet.RunPrincipal) audit.Actor {
	return audit.Actor{Type: audit.ActorAgent, ID: p.RunID}
}

// label is how an agent appears as a source or requester.
func label(p *fleet.RunPrincipal) string { return p.Role + " (" + p.Kind + ")" }
