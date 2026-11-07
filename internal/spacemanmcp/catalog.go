// Package spacemanmcp is the SpaceMan admin MCP (ADR-0063): SpaceMan's own API offered to a space's runs as MCP
// tools, one per API operation, executed in process with a secretless internal token's principal.
//
// The catalog is generated from the build's in-memory OpenAPI document and the API's Rule table. A reviewed
// classification (Table) puts every operation in a class; operations that need a person at the keyboard are excluded
// automatically from their rule (AutoExcluded). TestEveryOperationIsClassified keeps the table complete.
package spacemanmcp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/api"
)

const (
	// MaxSlug is the longest server slug the gateway accepts (internal/gateway reSlug).
	MaxSlug = 31
	// Separator joins a server's slug and a tool name, as the gateway does.
	Separator = "__"
	// MaxToolName is the longest namespaced tool name: model APIs accept up to 64 characters.
	MaxToolName = 64

	// ArgBody and ArgIfMatch are the tool arguments that carry the request body and the If-Match version.
	ArgBody    = "body"
	ArgIfMatch = "if_match"

	maxDescription = 1024
)

var reToolName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Tool is one API operation as an MCP tool.
type Tool struct {
	// Name is the tool name without the server's slug: the operation ID with "-" replaced by "_".
	Name        string
	OperationID string
	Method      string
	// Path is the operation's path below the API prefix, with {param} placeholders.
	Path   string
	Class  Class
	Reason string
	Rule   api.Rule
	// Description is the operation's summary and description, with the class.
	Description string
	// InputSchema is a self-contained JSON Schema object: path and query parameters, body and if_match.
	InputSchema json.RawMessage
	PathParams  []string
	QueryParams []string
	// Body is set when the operation takes a JSON body; BodyRequired when it must.
	Body, BodyRequired bool
	// IfMatch is set when the operation takes If-Match; IfMatchRequired when it must.
	IfMatch, IfMatchRequired bool
	// Idempotent operations take an Idempotency-Key, which the executor derives from the call.
	Idempotent bool
}

// ReadOnly reports whether the tool only reads.
func (t *Tool) ReadOnly() bool { return t.Class == Read || t.Class == SensitiveRead }

// ApprovalRequired reports whether every call needs a person's approval, whatever the server's approval setting:
// dangerous tools always do (ADR-0063 §Decision 7; enforced by the gateway in AM-5).
func (t *Tool) ApprovalRequired() bool { return t.Class == Dangerous }

// FullName is the tool's name as a run sees it on the server with this slug.
func (t *Tool) FullName(slug string) string { return slug + Separator + t.Name }

// Catalog is every operation the SpaceMan MCP may offer, and why the others are excluded.
type Catalog struct {
	tools    []*Tool
	byName   map[string]*Tool
	excluded map[string]string
}

// RuleFunc returns an operation's authorization rule (api.Deps.RuleFor).
type RuleFunc func(op string) (api.Rule, bool)

// Build generates the catalog from the API document and its rules. It fails when an operation has no rule or no
// classification, or when a tool cannot be expressed (an unknown parameter kind, a name clash, a schema cycle).
func Build(spec *huma.OpenAPI, rules RuleFunc) (*Catalog, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	c := &Catalog{byName: map[string]*Tool{}, excluded: map[string]string{}}
	var problems []string
	for _, path := range sortedKeys(doc.Paths) {
		for _, method := range []string{"get", "post", "put", "patch", "delete"} {
			opRaw, ok := doc.Paths[path][method]
			if !ok {
				continue
			}
			var op operation
			if err := json.Unmarshal(opRaw, &op); err != nil {
				return nil, fmt.Errorf("%s %s: %w", method, path, err)
			}
			rule, ok := rules(op.OperationID)
			if !ok {
				problems = append(problems, op.OperationID+": no authorization rule")
				continue
			}
			e, ok := Classify(op.OperationID, rule)
			switch {
			case !ok:
				problems = append(problems, op.OperationID+": not classified")
				continue
			case e.Class == Excluded:
				c.excluded[op.OperationID] = e.Reason
				continue
			}
			t, err := newTool(op, strings.ToUpper(method), path, rule, e, doc.Components.Schemas)
			if err != nil {
				problems = append(problems, op.OperationID+": "+err.Error())
				continue
			}
			if prev, dup := c.byName[t.Name]; dup {
				problems = append(problems, fmt.Sprintf("%s: tool name %s already used by %s", op.OperationID, t.Name, prev.OperationID))
				continue
			}
			c.byName[t.Name] = t
			c.tools = append(c.tools, t)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("spacemanmcp: %s", strings.Join(problems, "; "))
	}
	sort.Slice(c.tools, func(i, j int) bool { return c.tools[i].Name < c.tools[j].Name })
	return c, nil
}

// Tools returns every offerable tool, sorted by name.
func (c *Catalog) Tools() []*Tool { return append([]*Tool(nil), c.tools...) }

// Tool returns the tool with this name (without the slug).
func (c *Catalog) Tool(name string) (*Tool, bool) {
	t, ok := c.byName[name]
	return t, ok
}

// Excluded returns the excluded operation IDs with the reason.
func (c *Catalog) Excluded() map[string]string {
	out := make(map[string]string, len(c.excluded))
	for k, v := range c.excluded {
		out[k] = v
	}
	return out
}

type operation struct {
	OperationID string `json:"operationId"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Parameters  []struct {
		Name        string `json:"name"`
		In          string `json:"in"`
		Description string `json:"description"`
		Required    bool   `json:"required"`
		Schema      any    `json:"schema"`
	} `json:"parameters"`
	RequestBody *struct {
		Required bool                      `json:"required"`
		Content  map[string]map[string]any `json:"content"`
	} `json:"requestBody"`
}

func newTool(op operation, method, path string, rule api.Rule, e Entry, components map[string]any) (*Tool, error) {
	t := &Tool{Name: strings.ReplaceAll(op.OperationID, "-", "_"), OperationID: op.OperationID, Method: method, Path: path,
		Class: e.Class, Reason: e.Reason, Rule: rule}
	if !reToolName.MatchString(t.Name) {
		return nil, fmt.Errorf("tool name %q is not [a-z][a-z0-9_]*", t.Name)
	}
	if len(t.FullName(strings.Repeat("s", MaxSlug))) > MaxToolName {
		return nil, fmt.Errorf("tool name %q is too long for a %d-character slug", t.Name, MaxSlug)
	}
	props := map[string]any{}
	var required []string
	add := func(name string, schema any, req bool) error {
		if _, dup := props[name]; dup {
			return fmt.Errorf("argument %q appears twice", name)
		}
		props[name] = schema
		if req {
			required = append(required, name)
		}
		return nil
	}
	for _, p := range op.Parameters {
		switch p.In {
		case "path", "query":
			s, err := inline(p.Schema, components, 0)
			if err != nil {
				return nil, err
			}
			m, _ := s.(map[string]any)
			if m == nil {
				m = map[string]any{}
			}
			if p.Description != "" {
				m["description"] = p.Description
			}
			if p.Name == ArgBody || p.Name == ArgIfMatch {
				return nil, fmt.Errorf("parameter %q clashes with a tool argument", p.Name)
			}
			if err := add(p.Name, m, p.In == "path" || p.Required); err != nil {
				return nil, err
			}
			if p.In == "path" {
				t.PathParams = append(t.PathParams, p.Name)
			} else {
				t.QueryParams = append(t.QueryParams, p.Name)
			}
		case "header":
			switch p.Name {
			case "If-Match":
				t.IfMatch, t.IfMatchRequired = true, p.Required
				if err := add(ArgIfMatch, map[string]any{"type": "string",
					"description": "The resource's current version (its version field or ETag, e.g. 3); a stale version is a conflict"}, p.Required); err != nil {
					return nil, err
				}
			case "Idempotency-Key":
				t.Idempotent = true // derived by the executor, not an argument
			default:
				return nil, fmt.Errorf("header parameter %q is not supported", p.Name)
			}
		default:
			return nil, fmt.Errorf("%s parameter %q is not supported", p.In, p.Name)
		}
	}
	if rb := op.RequestBody; rb != nil {
		media, ok := rb.Content["application/json"]
		if !ok || len(rb.Content) != 1 {
			return nil, fmt.Errorf("request body is not JSON only")
		}
		s, err := inline(media["schema"], components, 0)
		if err != nil {
			return nil, err
		}
		t.Body, t.BodyRequired = true, rb.Required
		if err := add(ArgBody, s, rb.Required); err != nil {
			return nil, err
		}
	}
	sort.Strings(required)
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	var err error
	if t.InputSchema, err = json.Marshal(schema); err != nil {
		return nil, err
	}
	t.Description = describe(op, e)
	return t, nil
}

func describe(op operation, e Entry) string {
	d := strings.TrimSpace(op.Summary)
	if desc := strings.TrimSpace(op.Description); desc != "" {
		if d != "" {
			d += ". "
		}
		d += desc
	}
	if d == "" {
		d = op.OperationID
	}
	prefix := "[SpaceMan · " + string(e.Class) + "] "
	if len(prefix)+len(d) > maxDescription {
		d = truncateUTF8(d, maxDescription-len(prefix)-len("…")) + "…"
	}
	return prefix + d
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

const maxSchemaDepth = 64

// inline returns schema with every "$ref" to a component replaced by the component itself, without huma's read-only
// "$schema" link property. It fails on a reference cycle (a tool's schema must be self-contained).
func inline(schema any, components map[string]any, depth int) (any, error) {
	if depth > maxSchemaDepth {
		return nil, fmt.Errorf("schema nests deeper than %d levels (a reference cycle?)", maxSchemaDepth)
	}
	switch v := schema.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			name, found := strings.CutPrefix(ref, "#/components/schemas/")
			target, exists := components[name]
			if !found || !exists {
				return nil, fmt.Errorf("unresolvable schema reference %q", ref)
			}
			return inline(target, components, depth+1)
		}
		out := make(map[string]any, len(v))
		for k, x := range v {
			y, err := inline(x, components, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = y
		}
		if props, ok := out["properties"].(map[string]any); ok {
			if _, has := props["$schema"]; has {
				delete(props, "$schema")
				if req, ok := out["required"].([]any); ok {
					kept := req[:0:0]
					for _, r := range req {
						if r != "$schema" {
							kept = append(kept, r)
						}
					}
					out["required"] = kept
				}
			}
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			y, err := inline(x, components, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = y
		}
		return out, nil
	}
	return schema, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
