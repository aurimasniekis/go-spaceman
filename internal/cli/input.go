package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
)

type fieldKind int

const (
	kString fieldKind = iota
	kText             // --x VALUE or --x-file PATH
	kBool
	kInt
	kList    // repeated or comma-separated strings
	kMap     // repeated key=value
	kDoc     // --x-file PATH with a JSON or YAML value
	kSecret  // --x-file PATH or --x-stdin; never a flag value
	kSecDoc  // --x-file PATH with a JSON or YAML value that holds secrets
	kTextDoc // --x-file PATH read as text
)

type field struct {
	flag, key, usage string
	kind             fieldKind
	str              string
	b                bool
	n                int64
	list             []string
	file             string
	stdin            bool
}

// form collects a request body from flags: only flags that were set are sent, on top of an optional --file document
// and (for updates) the object as last read.
type form struct {
	cmd    *cobra.Command
	app    *App
	fields []*field
	file   string
}

func newForm(a *App, cmd *cobra.Command) *form {
	f := &form{cmd: cmd, app: a}
	cmd.Flags().StringVarP(&f.file, "file", "f", "", "request body as a JSON or YAML file (- reads standard input); flags override its fields")
	return f
}

func (f *form) add(fl *field) *form {
	f.fields = append(f.fields, fl)
	fs := f.cmd.Flags()
	switch fl.kind {
	case kString:
		fs.StringVar(&fl.str, fl.flag, "", fl.usage)
	case kText:
		fs.StringVar(&fl.str, fl.flag, "", fl.usage)
		fs.StringVar(&fl.file, fl.flag+"-file", "", "read --"+fl.flag+" from a file (- reads standard input)")
	case kBool:
		fs.BoolVar(&fl.b, fl.flag, false, fl.usage)
	case kInt:
		fs.Int64Var(&fl.n, fl.flag, 0, fl.usage)
	case kList, kMap:
		fs.StringArrayVar(&fl.list, fl.flag, nil, fl.usage)
	case kDoc, kSecDoc, kTextDoc:
		fs.StringVar(&fl.file, fl.flag+"-file", "", fl.usage)
	case kSecret:
		fs.StringVar(&fl.file, fl.flag+"-file", "", fl.usage+" (read from this file; never pass secrets as flag values)")
		fs.BoolVar(&fl.stdin, fl.flag+"-stdin", false, "read --"+fl.flag+"-file's value from standard input instead")
	}
	return f
}

func (f *form) str(flag, key, usage string) *form {
	return f.add(&field{flag: flag, key: key, usage: usage, kind: kString})
}
func (f *form) text(flag, key, usage string) *form {
	return f.add(&field{flag: flag, key: key, usage: usage, kind: kText})
}
func (f *form) boolean(flag, key, usage string) *form {
	return f.add(&field{flag: flag, key: key, usage: usage, kind: kBool})
}
func (f *form) integer(flag, key, usage string) *form {
	return f.add(&field{flag: flag, key: key, usage: usage, kind: kInt})
}
func (f *form) list(flag, key, usage string) *form {
	return f.add(&field{flag: flag, key: key, usage: usage + " (repeatable or comma-separated)", kind: kList})
}
func (f *form) kv(flag, key, usage string) *form {
	return f.add(&field{flag: flag, key: key, usage: usage + " (key=value, repeatable)", kind: kMap})
}
func (f *form) doc(flag, key, usage string) *form {
	return f.add(&field{flag: flag, key: key, usage: usage, kind: kDoc})
}
func (f *form) textFile(flag, key, usage string) *form {
	return f.add(&field{flag: flag, key: key, usage: usage, kind: kTextDoc})
}
func (f *form) secret(flag, key, usage string) *form {
	return f.add(&field{flag: flag, key: key, usage: usage, kind: kSecret})
}
func (f *form) secretDoc(flag, key, usage string) *form {
	return f.add(&field{flag: flag, key: key, usage: usage, kind: kSecDoc})
}

// set reports whether the user gave the field (flag or file).
func (f *form) set(fl *field) bool {
	fs := f.cmd.Flags()
	switch fl.kind {
	case kText:
		return fs.Changed(fl.flag) || fl.file != ""
	case kDoc, kSecDoc, kTextDoc:
		return fl.file != ""
	case kSecret:
		return fl.file != "" || fl.stdin
	}
	return fs.Changed(fl.flag)
}

// build returns the request body: base, then the --file document, then every flag that was set.
func (f *form) build(base map[string]any) (map[string]any, error) {
	body := map[string]any{}
	for k, v := range base {
		body[k] = v
	}
	if f.file != "" {
		doc, err := f.app.readDoc(f.file)
		if err != nil {
			return nil, err
		}
		m, ok := doc.(map[string]any)
		if !ok {
			return nil, usageErrorf("--file must hold an object")
		}
		for k, v := range m {
			body[k] = v
		}
	}
	stdinUsers := 0
	if f.file == "-" {
		stdinUsers++
	}
	for _, fl := range f.fields {
		if !f.set(fl) {
			continue
		}
		var v any
		switch fl.kind {
		case kString:
			v = fl.str
		case kText:
			if f.cmd.Flags().Changed(fl.flag) && fl.file != "" {
				return nil, usageErrorf("give --%s or --%s-file, not both", fl.flag, fl.flag)
			}
			if fl.file == "" {
				v = fl.str
				break
			}
			if fl.file == "-" {
				stdinUsers++
			}
			b, err := f.app.readFile(fl.file)
			if err != nil {
				return nil, err
			}
			v = string(b)
		case kBool:
			v = fl.b
		case kInt:
			v = fl.n
		case kList:
			out := []any{}
			for _, s := range fl.list {
				for _, p := range strings.Split(s, ",") {
					if p = strings.TrimSpace(p); p != "" {
						out = append(out, p)
					}
				}
			}
			v = out
		case kMap:
			m := map[string]any{}
			for _, s := range fl.list {
				k, val, ok := strings.Cut(s, "=")
				if !ok || strings.TrimSpace(k) == "" {
					return nil, usageErrorf("--%s takes key=value, got %q", fl.flag, s)
				}
				m[strings.TrimSpace(k)] = val
			}
			v = m
		case kDoc, kSecDoc:
			if fl.file == "-" {
				stdinUsers++
			}
			d, err := f.app.readDoc(fl.file)
			if err != nil {
				return nil, err
			}
			v = d
		case kTextDoc:
			if fl.file == "-" {
				stdinUsers++
			}
			b, err := f.app.readFile(fl.file)
			if err != nil {
				return nil, err
			}
			v = string(b)
		case kSecret:
			if fl.stdin && fl.file != "" {
				return nil, usageErrorf("give --%s-file or --%s-stdin, not both", fl.flag, fl.flag)
			}
			src := fl.file
			if fl.stdin {
				src = "-"
			}
			if src == "-" {
				stdinUsers++
			}
			s, err := f.app.readSecret(src)
			if err != nil {
				return nil, fmt.Errorf("--%s: %w", fl.flag, err)
			}
			v = s
		}
		setPath(body, fl.key, v)
	}
	if stdinUsers > 1 {
		return nil, usageErrorf("only one input can come from standard input")
	}
	return body, nil
}

// setPath sets a dotted key, creating nested objects.
func setPath(m map[string]any, key string, v any) {
	parts := strings.Split(key, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
}

// readFile reads a file, or standard input for "-" (at most 32 MiB).
func (a *App) readFile(path string) ([]byte, error) {
	var r io.Reader
	if path == "-" {
		r = a.stdin()
	} else {
		fh, err := os.Open(path) //nolint:gosec // an input file the operator names
		if err != nil {
			return nil, &UsageError{Msg: err.Error()}
		}
		defer func() { _ = fh.Close() }()
		r = fh
	}
	b, err := io.ReadAll(io.LimitReader(r, 32<<20+1))
	if err != nil {
		return nil, &UsageError{Msg: err.Error()}
	}
	if len(b) > 32<<20 {
		return nil, usageErrorf("%s is larger than 32 MiB", path)
	}
	return b, nil
}

// readDoc reads a JSON or YAML document.
func (a *App) readDoc(path string) (any, error) {
	b, err := a.readFile(path)
	if err != nil {
		return nil, err
	}
	return parseDoc(b)
}

func parseDoc(b []byte) (any, error) {
	t := bytes.TrimSpace(b)
	if len(t) > 0 && (t[0] == '{' || t[0] == '[') {
		var v any
		d := json.NewDecoder(bytes.NewReader(t))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			return nil, usageErrorf("invalid JSON: %v", err)
		}
		return v, nil
	}
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, usageErrorf("invalid YAML: %v", err)
	}
	return normalizeYAML(v), nil
}

// normalizeYAML turns YAML's map[any]any (non-string keys) into JSON-compatible values.
func normalizeYAML(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = normalizeYAML(e)
		}
		return x
	case map[any]any:
		m := map[string]any{}
		for k, e := range x {
			m[fmt.Sprint(k)] = normalizeYAML(e)
		}
		return m
	case []any:
		for i, e := range x {
			x[i] = normalizeYAML(e)
		}
		return x
	}
	return v
}

// readSecret reads a secret from a file or standard input ("-"), without the trailing line break. The value is never
// printed or logged.
func (a *App) readSecret(path string) (string, error) {
	b, err := a.readFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimRight(string(b), "\r\n")
	if strings.TrimSpace(s) == "" {
		return "", usageErrorf("the secret is empty")
	}
	return s, nil
}

func atoiPositive(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n, err == nil && n > 0
}
