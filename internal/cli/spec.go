package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Operation is what the CLI needs to know about one API operation, read from the OpenAPI document.
type Operation struct {
	ID         string
	Method     string
	Path       string // /spaces/{space_id}/…, relative to /api/v1
	Summary    string
	PathParams []string
	Query      map[string]bool
	Headers    map[string]bool // If-Match, Idempotency-Key
	// Body maps the request body's property names to their JSON types; Required lists the required ones.
	Body     map[string]string
	Required []string
}

// Spec indexes an OpenAPI document by operation ID.
type Spec struct {
	ops map[string]*Operation
}

var reParam = regexp.MustCompile(`\{([a-z_]+)\}`)

// ParseSpec reads an OpenAPI 3.1 document (the one `spaceman openapi` prints).
func ParseSpec(doc []byte) (*Spec, error) {
	var d struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]schema `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("parse OpenAPI document: %w", err)
	}
	s := &Spec{ops: map[string]*Operation{}}
	for path, methods := range d.Paths {
		for method, raw := range methods {
			switch method {
			case "get", "post", "put", "patch", "delete":
			default:
				continue
			}
			var o struct {
				OperationID string `json:"operationId"`
				Summary     string `json:"summary"`
				Parameters  []struct {
					In   string `json:"in"`
					Name string `json:"name"`
				} `json:"parameters"`
				RequestBody *struct {
					Content map[string]struct {
						Schema schema `json:"schema"`
					} `json:"content"`
				} `json:"requestBody"`
			}
			if err := json.Unmarshal(raw, &o); err != nil {
				return nil, fmt.Errorf("parse operation %s %s: %w", method, path, err)
			}
			op := &Operation{ID: o.OperationID, Method: strings.ToUpper(method), Path: path, Summary: o.Summary,
				Query: map[string]bool{}, Headers: map[string]bool{}}
			for _, m := range reParam.FindAllStringSubmatch(path, -1) {
				op.PathParams = append(op.PathParams, m[1])
			}
			for _, p := range o.Parameters {
				switch p.In {
				case "query":
					op.Query[p.Name] = true
				case "header":
					op.Headers[p.Name] = true
				}
			}
			if o.RequestBody != nil {
				if c, ok := o.RequestBody.Content["application/json"]; ok {
					sc := c.Schema.resolve(d.Components.Schemas)
					op.Body = map[string]string{}
					for k, v := range sc.Properties {
						if k == "$schema" {
							continue
						}
						op.Body[k] = v.resolve(d.Components.Schemas).typeName()
					}
					op.Required = append(op.Required, sc.Required...)
					sort.Strings(op.Required)
				}
			}
			s.ops[op.ID] = op
		}
	}
	return s, nil
}

type schema struct {
	Ref        string            `json:"$ref"`
	Type       any               `json:"type"`
	Properties map[string]schema `json:"properties"`
	Required   []string          `json:"required"`
}

func (s schema) resolve(all map[string]schema) schema {
	for i := 0; i < 8 && s.Ref != ""; i++ {
		s = all[strings.TrimPrefix(s.Ref, "#/components/schemas/")]
	}
	return s
}

func (s schema) typeName() string {
	switch t := s.Type.(type) {
	case string:
		return t
	case []any:
		for _, x := range t {
			if str, ok := x.(string); ok && str != "null" {
				return str
			}
		}
	}
	if s.Properties != nil {
		return "object"
	}
	return ""
}

// Op returns an operation by ID.
func (s *Spec) Op(id string) (*Operation, error) {
	op, ok := s.ops[id]
	if !ok {
		return nil, fmt.Errorf("operation %q is not in this build's API document", id)
	}
	return op, nil
}

// IDs returns every operation ID, sorted.
func (s *Spec) IDs() []string {
	out := make([]string, 0, len(s.ops))
	for id := range s.ops {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Expand substitutes path parameters (escaped) into the operation's path.
func (op *Operation) Expand(params map[string]string) (string, error) {
	var missing []string
	p := reParam.ReplaceAllStringFunc(op.Path, func(m string) string {
		name := m[1 : len(m)-1]
		v, ok := params[name]
		if !ok || v == "" {
			missing = append(missing, name)
			return m
		}
		return url.PathEscape(v)
	})
	if len(missing) > 0 {
		return "", usageErrorf("%s needs %s", op.ID, strings.Join(missing, ", "))
	}
	return p, nil
}

// project keeps only the keys the operation's request body accepts, and fills required ones that are missing with the
// zero value of their type (a GET omits empty fields that an update must send).
func (op *Operation) project(obj map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range obj {
		if _, ok := op.Body[k]; ok && v != nil {
			out[k] = v
		}
	}
	for _, k := range op.Required {
		if _, ok := out[k]; ok {
			continue
		}
		switch op.Body[k] {
		case "string":
			out[k] = ""
		case "boolean":
			out[k] = false
		case "integer", "number":
			out[k] = 0
		case "array":
			out[k] = []any{}
		case "object":
			out[k] = map[string]any{}
		}
	}
	return out
}
