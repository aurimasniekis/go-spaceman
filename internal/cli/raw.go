package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
)

// annRaw names the raw route (outside the OpenAPI document) a command calls.
const annRaw = "spaceman.raw"

// finder reads the object an update is based on from a list operation, for resources that have no get operation.
type finder struct {
	// op lists the objects; global is the list without --space (for commands with opts.global).
	op, global string
	// items is the key of the array in the list response.
	items string
	// param is the path parameter compared with the item fields in keys (for example dream_id against id and slug).
	param string
	keys  []string
	// fallback is a second array searched when nothing in items matches: its object becomes the base, without a
	// version (the effective policy of a type that has no stored one). With fallbackFirst its first object is the base
	// when nothing in it matches either.
	fallback      string
	fallbackFirst bool
}

// findObject lists f.op (or f.global) with the call's path parameters and returns the matching item, and whether its
// version may be sent as If-Match.
func (a *App) findObject(ctx context.Context, f *finder, global bool, params map[string]string) (map[string]any, bool, error) {
	op := f.op
	if global {
		op = f.global
	}
	res, err := a.do(ctx, call{op: op, params: params})
	if err != nil {
		return nil, false, err
	}
	obj, err := res.Object()
	if err != nil {
		return nil, false, err
	}
	want := params[f.param]
	match := func(key string) map[string]any {
		list, _ := obj[key].([]any)
		for _, it := range list {
			m, _ := it.(map[string]any)
			for _, k := range f.keys {
				if s, _ := m[k].(string); s != "" && s == want {
					return m
				}
			}
		}
		return nil
	}
	if m := match(f.items); m != nil {
		return m, true, nil
	}
	if f.fallback != "" {
		if m := match(f.fallback); m != nil {
			return m, false, nil
		}
		if list, _ := obj[f.fallback].([]any); f.fallbackFirst && len(list) > 0 {
			if m, ok := list[0].(map[string]any); ok {
				return m, false, nil
			}
		}
	}
	return nil, false, &APIError{Status: http.StatusNotFound, Code: "not_found", Detail: fmt.Sprintf("no %s %q", strings.TrimSuffix(f.param, "_id"), want)}
}

// isTerminal reports whether w is a character device (an interactive terminal).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// save writes r to path: "-" is standard output (refused on a terminal), anything else a new file (mode 0600) that is
// written completely before it appears under its name, and that replaces an existing file only with force.
func (a *App) save(cmd *cobra.Command, path string, force bool, r io.Reader) (int64, error) {
	if path == "-" {
		if isTerminal(cmd.OutOrStdout()) {
			return 0, usageErrorf("refusing to write the content to a terminal; redirect standard output or give --out FILE")
		}
		return io.Copy(cmd.OutOrStdout(), r)
	}
	if !force {
		if _, err := os.Stat(path); err == nil {
			return 0, usageErrorf("%s exists; pass --force to replace it", path)
		}
	}
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+base+".part-*")
	if err != nil {
		return 0, &UsageError{Msg: err.Error()}
	}
	name := tmp.Name()
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(name)
		return n, err
	}
	if force {
		if err := os.Rename(name, path); err != nil {
			_ = os.Remove(name)
			return n, &UsageError{Msg: err.Error()}
		}
		return n, nil
	}
	// A hard link fails when the name exists, so a file created meanwhile is never overwritten.
	err = os.Link(name, path)
	_ = os.Remove(name)
	if errors.Is(err, os.ErrExist) {
		return n, usageErrorf("%s exists; pass --force to replace it", path)
	}
	if err != nil {
		return n, &UsageError{Msg: "write " + path + ": " + err.Error()}
	}
	return n, nil
}

// printable removes control characters other than line breaks and tabs, so untrusted text cannot drive the terminal.
func printable(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

// expandRaw substitutes path parameters into a raw route's path.
func expandRaw(path string, params map[string]string) (string, error) {
	op := &Operation{ID: path, Path: path}
	for _, m := range reParam.FindAllStringSubmatch(path, -1) {
		op.PathParams = append(op.PathParams, m[1])
	}
	return op.Expand(params)
}

// open calls a raw route (GET path relative to /api/v1) and returns the streamed response.
func (a *App) open(ctx context.Context, path string, params map[string]string, query url.Values, accept string) (*http.Response, error) {
	p, err := expandRaw(path, params)
	if err != nil {
		return nil, err
	}
	cl, err := a.apiClient(false)
	if err != nil {
		return nil, err
	}
	return cl.Open(ctx, Request{Method: http.MethodGet, Path: p, Query: query, Accept: accept})
}

// downloadCmd saves the content of a raw route of a space (attachment content, a transcript, a recording) to --out.
// Content is never written to a terminal: it may be binary, contain escape sequences or secrets.
type download struct {
	short, long, example string
	// route is the raw path (relative to /api/v1); param its path parameter after space_id, filled by the argument.
	route, param, arg string
	// related are API operations about the same content (for the reference).
	related []string
	// use replaces "download <arg>" (a route without a parameter after space_id takes no argument).
	use string
}

func (a *App) downloadCmd(o download) *cobra.Command {
	var space, out string
	var force bool
	related := o.related
	route, param := o.route, o.param
	use, nargs := "download <"+o.arg+">", 1
	if param == "" {
		use, nargs = "download", 0
	}
	if o.use != "" {
		use = o.use
	}
	cmd := &cobra.Command{Use: use, Short: o.short, Long: o.long, Args: cobra.ExactArgs(nargs), Example: o.example,
		Annotations: map[string]string{annRaw: "GET " + route}}
	if len(related) > 0 {
		cmd.Annotations[annOps] = strings.Join(related, ",")
	}
	cmd.Flags().StringVar(&space, "space", "", "space ID or slug (env "+EnvSpace+")")
	cmd.Flags().StringVar(&out, "out", "", "file to write (mode 0600; - writes to standard output when it is not a terminal; required)")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing file")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		if out == "" {
			return usageErrorf("--out is required: the content is written to a file (or - for a pipe), never to the terminal")
		}
		if out == "-" && isTerminal(cmd.OutOrStdout()) {
			return usageErrorf("refusing to write the content to a terminal; redirect standard output or give --out FILE")
		}
		if out != "-" && !force {
			if _, err := os.Stat(out); err == nil {
				return usageErrorf("%s exists; pass --force to replace it", out)
			}
		}
		ref := space
		if ref == "" {
			ref = a.getenv(EnvSpace)
		}
		id, err := a.resolveSpace(ctx, ref)
		if err != nil {
			return err
		}
		params := map[string]string{"space_id": id}
		if param != "" {
			params[param] = args[0]
		}
		resp, err := a.open(ctx, route, params, nil, "*/*")
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		n, err := a.save(cmd, out, force, resp.Body)
		if err != nil {
			return err
		}
		if out != "-" {
			fmt.Fprintf(cmd.ErrOrStderr(), "Wrote %d bytes to %s.\n", n, out)
		}
		return nil
	}
	return cmd
}

// uploadPrepare turns the positional file argument into a multipart body with one "file" field.
func (a *App) uploadPrepare(_ context.Context, _ *cobra.Command, c *call, body map[string]any) error {
	path, _ := body["file"].(string)
	delete(body, "file")
	b, err := a.readFile(path)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	name := filepath.Base(path)
	if path == "-" {
		name = "stdin"
	}
	w, err := mw.CreateFormFile("file", name)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	c.raw, c.rawType = buf.Bytes(), mw.FormDataContentType()
	return nil
}

// watchCmd prints the change feed (GET /watch, Server-Sent Events) until interrupted or --count changes.
func (a *App) watchCmd() *cobra.Command {
	var after int64
	var count int
	cmd := &cobra.Command{Use: "watch", Short: "Follow the change feed: one line per change you may see, until interrupted",
		Long: "Streams GET /api/v1/watch (Server-Sent Events). Each change names a resource and its new version, never its content; " +
			"read the object with its get command. -o json prints JSON Lines. A gap (retention pruned changes after --after) is " +
			"reported on standard error; re-list what you follow.",
		Example: "spaceman watch --after 1200 -o json | jq -c 'select(.resource_type == \"task\")'",
		Args:    cobra.NoArgs, Annotations: map[string]string{annRaw: "GET /watch"}}
	cmd.Flags().Int64Var(&after, "after", 0, "start after this change sequence (0: from the oldest retained change)")
	cmd.Flags().IntVar(&count, "count", 0, "stop after this many changes (0: follow until interrupted)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		format, err := a.format()
		if err != nil {
			return err
		}
		q := url.Values{}
		if after > 0 {
			q.Set("after", strconv.FormatInt(after, 10))
		}
		resp, err := a.open(cmd.Context(), "/watch", nil, q, "text/event-stream")
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		seen := 0
		return readSSE(resp.Body, func(event, data string) (bool, error) {
			switch event {
			case "change":
				if err := writeChange(cmd.OutOrStdout(), format, data); err != nil {
					return false, err
				}
				seen++
				return count > 0 && seen >= count, nil
			case "gap":
				fmt.Fprintf(cmd.ErrOrStderr(), "gap in the change feed: %s; re-list what you follow\n", strings.TrimSpace(printable(data)))
			case "end":
				return true, &StateError{Msg: "the server ended the stream: " + strings.TrimSpace(data)}
			}
			return false, nil
		})
	}
	return cmd
}

// readSSE calls fn for each event of a Server-Sent Events stream until fn says stop or the stream ends.
func readSSE(r io.Reader, fn func(event, data string) (bool, error)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	event, data := "", []string{}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if event != "" || len(data) > 0 {
				if event == "" {
					event = "message"
				}
				stop, err := fn(event, strings.Join(data, "\n"))
				if err != nil || stop {
					return err
				}
			}
			event, data = "", data[:0]
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	return sc.Err()
}

func writeChange(w io.Writer, format, data string) error {
	var m map[string]any
	d := json.NewDecoder(strings.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&m); err != nil {
		return fmt.Errorf("decode change: %w", err)
	}
	switch format {
	case FormatJSON:
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "%s\n", b)
		return err
	case FormatYAML:
		b, _ := json.Marshal(m)
		if _, err := io.WriteString(w, "---\n"); err != nil {
			return err
		}
		return writeYAML(w, b)
	}
	space := cell(m["space_id"])
	if space == "" {
		space = "-"
	}
	_, err := fmt.Fprintf(w, "%s  %s  %s  %s %s  %s  v%s\n", cell(m["seq"]), microsAt("at")(m), space, cell(m["resource_type"]),
		cell(m["resource_id"]), cell(m["action"]), cell(m["version"]))
	return err
}
