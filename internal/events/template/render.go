package template

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Context is what a template can see: the event envelope, its JSON payload and the subscription's variables. It never
// contains secrets (SM-EVT-009).
type Context struct {
	Event map[string]any // id, subject, type, source, time, content_type
	Data  any            // the decoded JSON payload
	Vars  map[string]string
}

func (c Context) root() map[string]any {
	vars := map[string]any{}
	for k, v := range c.Vars {
		vars[k] = v
	}
	return map[string]any{"event": c.Event, "data": c.Data, "vars": vars}
}

// placeholder: {{ path | filter arg | filter }}
var rePlaceholder = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)

var rePath = regexp.MustCompile(`^(event|data|vars)(\.[A-Za-z0-9_-]+)*$`)

// Filters a placeholder may use. They are pure functions of the value: templates cannot execute anything.
var filters = map[string]bool{"default": true, "truncate": true, "json": true, "upper": true, "lower": true, "oneline": true, "code": true}

type pipe struct {
	path    string
	filters [][]string // name, args…
}

func parsePlaceholder(expr string) (pipe, error) {
	parts := splitPipes(expr)
	p := pipe{path: strings.TrimSpace(parts[0])}
	if !rePath.MatchString(p.path) {
		return p, fmt.Errorf("{{%s}}: the path must start with event., data. or vars. and use letters, digits, _ and -", expr)
	}
	for _, f := range parts[1:] {
		args, err := splitArgs(strings.TrimSpace(f))
		if err != nil {
			return p, fmt.Errorf("{{%s}}: %w", expr, err)
		}
		if len(args) == 0 || !filters[args[0]] {
			return p, fmt.Errorf("{{%s}}: unknown filter %q", expr, f)
		}
		switch args[0] {
		case "default":
			if len(args) != 2 {
				return p, fmt.Errorf("{{%s}}: default needs one quoted value", expr)
			}
		case "truncate":
			if len(args) != 2 {
				return p, fmt.Errorf("{{%s}}: truncate needs a length", expr)
			}
			if n, err := strconv.Atoi(args[1]); err != nil || n < 1 || n > 100_000 {
				return p, fmt.Errorf("{{%s}}: truncate length must be 1-100000", expr)
			}
		default:
			if len(args) != 1 {
				return p, fmt.Errorf("{{%s}}: %s takes no arguments", expr, args[0])
			}
		}
		p.filters = append(p.filters, args)
	}
	return p, nil
}

// splitPipes splits on | outside double quotes.
func splitPipes(s string) []string {
	var out []string
	var b strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			b.WriteRune(r)
		case r == '|' && !quoted:
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	return append(out, b.String())
}

// splitArgs splits a filter call into its name and arguments (a quoted string or a bare word).
func splitArgs(s string) ([]string, error) {
	var out []string
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		if s[0] == '"' {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				return nil, fmt.Errorf("unterminated string")
			}
			out = append(out, s[1:end+1])
			s = s[end+2:]
			continue
		}
		i := strings.IndexAny(s, " \t")
		if i < 0 {
			i = len(s)
		}
		out = append(out, s[:i])
		s = s[i:]
	}
	return out, nil
}

// Check parses every placeholder of a template without rendering it.
func Check(tmpl string) error {
	for _, m := range rePlaceholder.FindAllStringSubmatch(tmpl, -1) {
		if _, err := parsePlaceholder(m[1]); err != nil {
			return err
		}
	}
	if strings.Count(tmpl, "{{") != len(rePlaceholder.FindAllString(tmpl, -1)) {
		return fmt.Errorf("unbalanced {{ }}")
	}
	return nil
}

// Lookup resolves a dotted path (event.id, data.repository.full_name, data.items.0.name) in the context.
func Lookup(root any, path string) (any, bool) {
	cur := root
	for _, seg := range strings.Split(path, ".") {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[seg]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(v) {
				return nil, false
			}
			cur = v[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// stringify renders a value: strings as-is, numbers and booleans as JSON, objects and arrays as compact JSON.
func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func apply(p pipe, root map[string]any) string {
	v, ok := Lookup(root, p.path)
	s := ""
	if ok {
		s = stringify(v)
	}
	for _, f := range p.filters {
		switch f[0] {
		case "default":
			if s == "" {
				s = f[1]
			}
		case "truncate":
			n, _ := strconv.Atoi(f[1])
			if utf8.RuneCountInString(s) > n {
				s = string([]rune(s)[:n]) + "…"
			}
		case "json":
			b, _ := json.Marshal(v)
			s = string(b)
		case "upper":
			s = strings.ToUpper(s)
		case "lower":
			s = strings.ToLower(s)
		case "oneline":
			s = strings.Join(strings.Fields(s), " ")
		case "code":
			// An inline code span that the value cannot break out of.
			fence := "`"
			for strings.Contains(s, fence) {
				fence += "`"
			}
			s = fence + " " + s + " " + fence
		}
	}
	return s
}

// Render substitutes the placeholders of a template, bounded to max runes (the rest is cut with a marker). Rendering is
// deterministic: the same event and variables always give the same text.
func Render(tmpl string, ctx Context, max int) (string, error) {
	root := ctx.root()
	var err error
	out := rePlaceholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		p, perr := parsePlaceholder(rePlaceholder.FindStringSubmatch(m)[1])
		if perr != nil {
			err = perr
			return ""
		}
		return apply(p, root)
	})
	if err != nil {
		return "", err
	}
	if max > 0 && utf8.RuneCountInString(out) > max {
		out = string([]rune(out)[:max]) + "\n\n… (truncated)"
	}
	return out, nil
}
