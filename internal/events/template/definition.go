// Package template is SpaceMan's event template engine (SM-EVT-004…010): a definition says which Pulse subjects to
// subscribe to, which events to accept (payload filters, an optional JSON Schema) and how to present an accepted event as
// an inbox item (title, body, instructions for control, correlation, urgency). Templates are data, never code: placeholders
// are dotted paths with a few pure filters, so rendering is deterministic, bounded and cannot execute anything.
package template

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/aurimasniekis/go-pulse/pkg/pulse/envelope"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/aurimasniekis/go-spaceman/internal/schemautil"
)

// Limits on rendered output.
const (
	MaxTitle        = 200
	MaxBody         = 50_000
	MaxInstructions = 20_000
)

// Variable is a typed subscription parameter.
type Variable struct {
	Name        string `json:"name" pattern:"^[a-z][a-z0-9_]{0,31}$"`
	Description string `json:"description,omitempty" maxLength:"500"`
	// Pattern is a regular expression every value must match (anchored).
	Pattern  string `json:"pattern,omitempty" maxLength:"500"`
	Required bool   `json:"required,omitempty"`
	Default  string `json:"default,omitempty" maxLength:"200"`
	Example  string `json:"example,omitempty" maxLength:"200"`
}

// Filter accepts or rejects an event by a payload value. Value and Values may contain {{vars.x}} placeholders only.
type Filter struct {
	Path   string   `json:"path" doc:"Dotted path into the event, e.g. data.repository.full_name"`
	Op     string   `json:"op" enum:"eq,ne,in,exists,prefix"`
	Value  string   `json:"value,omitempty"`
	Values []string `json:"values,omitempty"`
}

// Definition is a template version's content, or a subscription's custom definition.
type Definition struct {
	// Subject is a Pulse filter under evt.> in which whole tokens may be {{vars.x}} placeholders; * and a final >
	// are allowed only as literal tokens of the definition, never through variables (SM-EVT-010).
	Subject   string     `json:"subject" maxLength:"256"`
	Variables []Variable `json:"variables"`
	Filters   []Filter   `json:"filters"`
	// InputSchema is an optional JSON Schema (2020-12) the payload must satisfy; failures go to quarantine.
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	Title        string          `json:"title" maxLength:"500"`
	Body         string          `json:"body" maxLength:"20000"`
	Instructions string          `json:"instructions,omitempty" maxLength:"20000" doc:"Rendered guidance for control, stored with the item"`
	Correlation  string          `json:"correlation,omitempty" maxLength:"500"`
	Urgency      string          `json:"urgency,omitempty" enum:"low,normal,high,urgent"`
}

var (
	reVarName   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	reToken     = regexp.MustCompile(`^[a-z0-9_-]+$`)
	reVarsOnly  = regexp.MustCompile(`\{\{\s*vars\.([a-z][a-z0-9_]{0,31})\s*\}\}`)
	filterOps   = map[string]bool{"eq": true, "ne": true, "in": true, "exists": true, "prefix": true}
	urgencyVals = map[string]bool{"": true, "low": true, "normal": true, "high": true, "urgent": true}
)

// Validate checks a definition (SM-EVT-006: before activation). Errors name the field.
func (d *Definition) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	vars := map[string]Variable{}
	for _, v := range d.Variables {
		if !reVarName.MatchString(v.Name) {
			add("variables: %q is not a valid name", v.Name)
			continue
		}
		if _, dup := vars[v.Name]; dup {
			add("variables: %q is defined twice", v.Name)
		}
		if v.Pattern != "" {
			if _, err := regexp.Compile("^(?:" + v.Pattern + ")$"); err != nil {
				add("variables.%s.pattern: %v", v.Name, err)
			}
		}
		vars[v.Name] = v
	}
	toks := subjectTokens(d.Subject)
	probe := make([]string, len(toks))
	for i, t := range toks {
		if t.isVar {
			if _, ok := vars[t.text]; !ok {
				add("subject: variable %q is not defined", t.text)
			}
			probe[i] = "x"
			continue
		}
		if strings.ContainsAny(t.text, "{}\x00") {
			add("subject: a placeholder must be a whole token ({{vars.name}})")
		}
		probe[i] = t.text
	}
	if err := envelope.ValidateFilter(strings.Join(probe, ".")); err != nil {
		add("subject: %v", err)
	}
	for i, f := range d.Filters {
		if !rePath.MatchString(f.Path) || strings.HasPrefix(f.Path, "vars") {
			add("filters[%d].path: must be event.… or data.…", i)
		}
		if !filterOps[f.Op] {
			add("filters[%d].op: must be eq, ne, in, exists or prefix", i)
		}
		for _, val := range append([]string{f.Value}, f.Values...) {
			if strings.Count(val, "{{") != len(reVarsOnly.FindAllString(val, -1)) {
				add("filters[%d]: values may only use {{vars.name}} placeholders", i)
			}
			for _, m := range reVarsOnly.FindAllStringSubmatch(val, -1) {
				if _, ok := vars[m[1]]; !ok {
					add("filters[%d]: variable %q is not defined", i, m[1])
				}
			}
		}
		if f.Op == "in" && len(f.Values) == 0 {
			add("filters[%d]: in needs values", i)
		}
	}
	// Instructions become trusted metadata of the inbox item, so they may only come from the template and its
	// variables, never from the event (SM-INBOX-004, SM-EVT-008; security review SR-L08).
	if strings.Count(d.Instructions, "{{") != len(reVarsOnly.FindAllString(d.Instructions, -1)) {
		add("instructions: may only use {{vars.name}} placeholders; event content is untrusted and cannot become instructions")
	}
	for name, tmpl := range map[string]string{"title": d.Title, "body": d.Body, "instructions": d.Instructions, "correlation": d.Correlation} {
		if err := Check(tmpl); err != nil {
			add("%s: %v", name, err)
		}
	}
	if strings.TrimSpace(d.Title) == "" {
		add("title: is required")
	}
	if !urgencyVals[d.Urgency] {
		add("urgency: must be low, normal, high or urgent")
	}
	if len(d.InputSchema) > 0 {
		if _, err := compileSchema(d.InputSchema); err != nil {
			add("input_schema: %v", err)
		}
	}
	return errors.Join(errs...)
}

// Bound is a definition with variable values applied: the effective subject and filters a subscription consumes.
type Bound struct {
	Def     *Definition
	Vars    map[string]string
	Subject string
	Filters []Filter
	schema  *jsonschema.Schema
}

// Bind applies variable values. A value used in the subject must be a single subject token ([a-z0-9_-]+), so no
// variable can add a wildcard or another token and widen the subscription (SM-EVT-010).
func (d *Definition) Bind(values map[string]string) (*Bound, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	vals := map[string]string{}
	var errs []error
	known := map[string]bool{}
	for _, v := range d.Variables {
		known[v.Name] = true
		val, ok := values[v.Name]
		if !ok || val == "" {
			val = v.Default
		}
		if val == "" {
			if v.Required {
				errs = append(errs, fmt.Errorf("vars.%s: is required", v.Name))
			}
			vals[v.Name] = ""
			continue
		}
		if strings.Contains(val, "{{") || strings.ContainsAny(val, "*>\x00") {
			errs = append(errs, fmt.Errorf("vars.%s: may not contain wildcards or placeholders", v.Name))
			continue
		}
		if v.Pattern != "" && !regexp.MustCompile("^(?:"+v.Pattern+")$").MatchString(val) {
			errs = append(errs, fmt.Errorf("vars.%s: %q does not match %s", v.Name, val, v.Pattern))
			continue
		}
		vals[v.Name] = val
	}
	for k := range values {
		if !known[k] {
			errs = append(errs, fmt.Errorf("vars.%s: is not a variable of this template", k))
		}
	}
	parts := []string{}
	for _, t := range subjectTokens(d.Subject) {
		if t.isVar {
			v := vals[t.text]
			if !reToken.MatchString(v) {
				errs = append(errs, fmt.Errorf("vars.%s: %q cannot be used as a subject token ([a-z0-9_-]+)", t.text, v))
			}
			parts = append(parts, v)
			continue
		}
		parts = append(parts, t.text)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	b := &Bound{Def: d, Vars: vals, Subject: strings.Join(parts, ".")}
	if err := envelope.ValidateFilter(b.Subject); err != nil {
		return nil, err
	}
	sub := func(s string) string {
		return reVarsOnly.ReplaceAllStringFunc(s, func(m string) string { return vals[reVarsOnly.FindStringSubmatch(m)[1]] })
	}
	for _, f := range d.Filters {
		nf := Filter{Path: f.Path, Op: f.Op, Value: sub(f.Value)}
		for _, v := range f.Values {
			nf.Values = append(nf.Values, sub(v))
		}
		b.Filters = append(b.Filters, nf)
	}
	if len(d.InputSchema) > 0 {
		s, err := compileSchema(d.InputSchema)
		if err != nil {
			return nil, err
		}
		b.schema = s
	}
	return b, nil
}

type subjectToken struct {
	text  string // the literal token, or the variable name
	isVar bool
}

// subjectTokens splits a subject template on dots outside {{ }}; a token that is exactly one {{vars.name}} placeholder
// is a variable, and anything else keeps its text (a partial placeholder then fails validation).
func subjectTokens(s string) []subjectToken {
	const mark = "\x00"
	masked := reVarsOnly.ReplaceAllStringFunc(s, func(m string) string { return mark + reVarsOnly.FindStringSubmatch(m)[1] + mark })
	var out []subjectToken
	for _, t := range strings.Split(masked, ".") {
		if strings.HasPrefix(t, mark) && strings.HasSuffix(t, mark) && strings.Count(t, mark) == 2 && len(t) > 2 {
			out = append(out, subjectToken{text: strings.Trim(t, mark), isVar: true})
			continue
		}
		out = append(out, subjectToken{text: t})
	}
	return out
}

func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) { return schemautil.Compile(raw) }
