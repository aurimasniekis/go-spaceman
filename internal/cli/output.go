package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"go.yaml.in/yaml/v3"
)

// Output formats.
const (
	FormatTable = "table"
	FormatJSON  = "json"
	FormatYAML  = "yaml"
)

// col is one table column: a header and a dotted path into the item (or a function).
type col struct {
	head string
	path string
	fn   func(map[string]any) string
}

func c(head, path string) col { return col{head: head, path: path} }

// view says how a response is shown as a table.
type view struct {
	// items is the key of the array to tabulate ("" shows a single object as FIELD VALUE lines).
	items string
	cols  []col
	// first lists the fields a single object shows first (the rest follow alphabetically).
	first []string
	// after prints extra sections below a single object (relocation issues).
	after func(w io.Writer, obj map[string]any)
	// empty is printed (to stderr) when a list has no items.
	empty string
}

// render writes a JSON body in the chosen format.
func render(w io.Writer, format string, body []byte, v view) error {
	body = bytes.TrimSpace(body)
	switch format {
	case FormatJSON:
		if len(body) == 0 {
			return nil
		}
		var buf bytes.Buffer
		if err := json.Indent(&buf, body, "", "  "); err != nil {
			return fmt.Errorf("format response: %w", err)
		}
		buf.WriteByte('\n')
		_, err := w.Write(buf.Bytes())
		return err
	case FormatYAML:
		if len(body) == 0 {
			return nil
		}
		return writeYAML(w, body)
	}
	if len(body) == 0 {
		return nil
	}
	var data any
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&data); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	obj, _ := data.(map[string]any)
	if v.items != "" {
		var items []any
		if obj != nil {
			items, _ = obj[v.items].([]any)
		} else if arr, ok := data.([]any); ok {
			items = arr
		}
		return writeTable(w, items, v.cols)
	}
	if obj == nil {
		_, err := fmt.Fprintln(w, scalar(data))
		return err
	}
	return writeObject(w, obj, v)
}

func writeYAML(w io.Writer, body []byte) error {
	var n yaml.Node
	if err := yaml.Unmarshal(body, &n); err != nil {
		return fmt.Errorf("format response: %w", err)
	}
	blockStyle(&n)
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return err
	}
	return enc.Close()
}

// blockStyle turns the flow style of parsed JSON into ordinary block YAML (strings stay quoted only where needed).
func blockStyle(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		n.Style = 0
		if strings.Contains(n.Value, "\n") {
			n.Style = yaml.LiteralStyle
		}
	} else {
		n.Style = 0
	}
	for _, ch := range n.Content {
		blockStyle(ch)
	}
}

func writeTable(w io.Writer, items []any, cols []col) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	heads := make([]string, len(cols))
	for i, cl := range cols {
		heads[i] = cl.head
	}
	fmt.Fprintln(tw, strings.Join(heads, "\t"))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			// A list of scalars (file paths): one value per row.
			fmt.Fprintln(tw, cell(it))
			continue
		}
		cells := make([]string, len(cols))
		for i, cl := range cols {
			if cl.fn != nil {
				cells[i] = cl.fn(m)
			} else {
				cells[i] = cell(lookup(m, cl.path))
			}
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

func writeObject(w io.Writer, obj map[string]any, v view) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	seen := map[string]bool{}
	order := []string{}
	for _, k := range append(append([]string{}, v.first...), "id", "slug", "name", "title", "state", "status", "version") {
		if _, ok := obj[k]; ok && !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
	}
	rest := []string{}
	for k := range obj {
		if !seen[k] && k != "$schema" {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range append(order, rest...) {
		val := obj[k]
		if val == nil {
			continue
		}
		fmt.Fprintf(tw, "%s:\t%s\n", k, cell(val))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if v.after != nil {
		v.after(w, obj)
	}
	return nil
}

// lookup follows a dotted path through nested objects.
func lookup(m map[string]any, path string) any {
	var cur any = m
	for _, p := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

const maxCell = 100

// cell formats a value for a table: times shortened, lists joined, objects as compact JSON, long text cut.
func cell(v any) string {
	var s string
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil && len(x) >= 20 {
			s = t.UTC().Format("2006-01-02 15:04:05Z")
		} else {
			s = x
		}
	case []any:
		parts := make([]string, 0, len(x))
		simple := true
		for _, e := range x {
			switch e.(type) {
			case map[string]any, []any:
				simple = false
			}
			parts = append(parts, scalar(e))
		}
		if simple {
			s = strings.Join(parts, ",")
		} else {
			b, _ := json.Marshal(x)
			s = string(b)
		}
	case map[string]any:
		if len(x) == 0 {
			return ""
		}
		b, _ := json.Marshal(x)
		s = string(b)
	default:
		s = scalar(x)
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	if r := []rune(s); len(r) > maxCell {
		s = string(r[:maxCell-1]) + "…"
	}
	return s
}

func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	case map[string]any, []any:
		b, _ := json.Marshal(x)
		return string(b)
	}
	return fmt.Sprint(v)
}
