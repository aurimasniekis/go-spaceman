package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

type argKind int

const (
	aPlain argKind = iota
	aSpace
	aNode
)

// arg is a positional argument that fills the next path parameter of the operation.
type arg struct {
	name string
	kind argKind
}

func pos(names ...string) []arg {
	out := make([]arg, len(names))
	for i, n := range names {
		out[i] = arg{name: n}
	}
	return out
}

// qflag is a flag that becomes a query parameter.
type qflag struct {
	flag, param, usage string
	kind               fieldKind // kString, kBool or kInt
}

func qs(flag, param, usage string) qflag {
	return qflag{flag: flag, param: param, usage: usage, kind: kString}
}
func qb(flag, param, usage string) qflag {
	return qflag{flag: flag, param: param, usage: usage, kind: kBool}
}
func qi(flag, param, usage string) qflag {
	return qflag{flag: flag, param: param, usage: usage, kind: kInt}
}
func limitFlag() qflag { return qi("limit", "limit", "maximum number of items") }

// opts declares one command bound to an API operation.
type opts struct {
	use, short, long, example string
	op                        string
	// alias are extra names of the command.
	alias []string
	args  []arg
	// bodyArgs are positional arguments after args that fill body fields (a create's name or slug).
	bodyArgs []string
	// scoped commands need --space (or SPACEMAN_SPACE); with global set, a command without --space calls global.
	scoped bool
	global string
	form   func(*form)
	query  []qflag
	view   view
	// get is read first (same path parameters): its version becomes If-Match, and with merge the object is the base
	// of the request body (so only the flags given change).
	get   string
	merge bool
	// find reads the object from a list operation instead (resources without a get operation).
	find *finder
	// mergeKey merges the object at this key of the response read (a detail that wraps the object) instead of the whole.
	mergeKey string
	version  func(map[string]any) string
	// base derives the request body from the object read (instead of merge's projection).
	base func(map[string]any) map[string]any
	// drop removes fields of the object read from the merged body (fields that must only be sent on purpose).
	drop []string
	// getOptional treats a 404 from get as "not created yet": no base and no If-Match.
	getOptional bool
	// canonical replaces this path parameter with the ID of the object read (get accepts a slug, the change only an ID).
	canonical string
	// check inspects a successful response object (for example a failed verification) after it is printed.
	check func(map[string]any) error
	// anonymous commands work without a token.
	anonymous bool
	// optionalIfMatch sends If-Match only when the object read has a version (first saves have none).
	optionalIfMatch bool
	static          map[string]any
	// confirm guards irreversible operations with --yes.
	confirm string
	// secret is the response field holding a one-time secret (--secret-file writes it there instead of printing it).
	secret string
	// redact lists further response fields that reveal the secret; --secret-file removes them from the output too.
	redact []string
	// onRequest prints the one-time secret only with --show-secret; without it or --secret-file the call is refused.
	onRequest bool
	// globalHelp describes what a scoped command does without --space (with global set).
	globalHelp string
	// text is a response field printed verbatim (control characters removed) in table format, or written with --out.
	text  string
	paged bool
	idem  bool
	done  string
	// prepare adjusts the call after the body is built (validation, renamed operations).
	prepare func(ctx context.Context, cmd *cobra.Command, c *call, body map[string]any) error
	// run replaces the generic flow.
	run func(cmd *cobra.Command, args []string) error
}

// annotation keys.
const annOps = "spaceman.operations"

func (a *App) cmd(o opts) *cobra.Command {
	names := []string{}
	for _, ar := range o.args {
		names = append(names, "<"+ar.name+">")
	}
	for _, b := range o.bodyArgs {
		names = append(names, "<"+b+">")
	}
	use := o.use
	if len(names) > 0 && !strings.Contains(use, " ") {
		use += " " + strings.Join(names, " ")
	}
	cmd := &cobra.Command{Use: use, Short: o.short, Long: o.long, Example: o.example, Aliases: o.alias, Args: cobra.ExactArgs(len(o.args) + len(o.bodyArgs))}
	ops := []string{}
	first, findGlobal := o.get, ""
	if o.find != nil {
		first, findGlobal = o.find.op, o.find.global
	}
	for _, id := range []string{first, o.op, o.global, findGlobal} {
		if id != "" && !contains(ops, id) {
			ops = append(ops, id)
		}
	}
	cmd.Annotations = map[string]string{annOps: strings.Join(ops, ",")}
	var space, ifMatch, secretFile, cursor, idemKey, textOut string
	var yes, all, showSecret, force bool
	if o.scoped {
		help := "space ID or slug (env " + EnvSpace + ")"
		if o.globalHelp != "" {
			help += "; " + o.globalHelp
		} else if o.global != "" {
			help += "; without it, every space you can see"
		}
		cmd.Flags().StringVar(&space, "space", "", help)
	}
	if o.get != "" || o.find != nil {
		cmd.Flags().StringVar(&ifMatch, "if-match", "", "expected object version (from an earlier read); default: the version read just before the change")
	}
	if o.confirm != "" {
		cmd.Flags().BoolVar(&yes, "yes", false, "confirm: "+o.confirm)
	}
	if o.secret != "" {
		cmd.Flags().StringVar(&secretFile, "secret-file", "", "write the one-time secret to this new file (mode 0600) instead of printing it")
		if o.onRequest {
			cmd.Flags().BoolVar(&showSecret, "show-secret", false, "print the one-time secret (shown once; without this or --secret-file the command refuses to run)")
		}
	}
	if o.text != "" {
		cmd.Flags().StringVar(&textOut, "out", "", "write the "+o.text+" to this file instead of printing it")
		cmd.Flags().BoolVar(&force, "force", false, "with --out: replace an existing file")
	}
	if o.paged {
		cmd.Flags().BoolVar(&all, "all", false, "follow next_cursor and print every page")
		cmd.Flags().StringVar(&cursor, "cursor", "", "continue from a previous page's next_cursor")
	}
	if o.idem {
		cmd.Flags().StringVar(&idemKey, "idempotency-key", "", "Idempotency-Key to send (default: a fresh random key); reuse it to retry this create safely")
	}
	qvals := make([]any, len(o.query))
	for i, q := range o.query {
		switch q.kind {
		case kBool:
			b := new(bool)
			cmd.Flags().BoolVar(b, q.flag, false, q.usage)
			qvals[i] = b
		case kInt:
			n := new(int)
			cmd.Flags().IntVar(n, q.flag, 0, q.usage)
			qvals[i] = n
		default:
			s := new(string)
			cmd.Flags().StringVar(s, q.flag, "", q.usage)
			qvals[i] = s
		}
	}
	var f *form
	if o.form != nil {
		f = newForm(a, cmd)
		o.form(f)
	}
	if o.run != nil {
		cmd.RunE = o.run
		return cmd
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		format, err := a.format()
		if err != nil {
			return err
		}
		c := call{op: o.op, params: map[string]string{}, query: url.Values{}, idemKey: idemKey}
		if o.scoped {
			ref := space
			if ref == "" {
				ref = a.getenv(EnvSpace)
			}
			if ref == "" && o.global != "" {
				c.op = o.global
			} else {
				id, err := a.resolveSpace(ctx, ref)
				if err != nil {
					return err
				}
				c.params["space_id"] = id
			}
		}
		if err := a.bindArgs(ctx, c.op, o.args, args[:len(o.args)], c.params); err != nil {
			return err
		}
		for i, q := range o.query {
			if !cmd.Flags().Changed(q.flag) {
				continue
			}
			switch v := qvals[i].(type) {
			case *bool:
				c.query.Set(q.param, strconv.FormatBool(*v))
			case *int:
				c.query.Set(q.param, strconv.Itoa(*v))
			case *string:
				c.query.Set(q.param, *v)
			}
		}
		if cursor != "" {
			c.query.Set("cursor", cursor)
		}
		c.anonymous = o.anonymous
		if o.secret != "" && o.onRequest && secretFile == "" && !showSecret {
			return usageErrorf("this returns a one-time secret: give --secret-file PATH to store it, or --show-secret to print it once")
		}
		if textOut != "" && textOut != "-" && !force {
			if _, err := os.Stat(textOut); err == nil {
				return usageErrorf("%s exists; pass --force to replace it", textOut)
			}
		}
		var base, obj map[string]any
		var res *Response
		have, versioned := false, true
		if o.get != "" {
			res, err = a.do(ctx, call{op: o.get, params: c.params})
			var ae *APIError
			if err != nil && o.getOptional && errors.As(err, &ae) && ae.Status == 404 {
				res, err = nil, nil
			}
			if err != nil {
				return err
			}
			if res != nil {
				if obj, err = res.Object(); err != nil {
					return err
				}
				have = true
			}
		} else if o.find != nil {
			if obj, versioned, err = a.findObject(ctx, o.find, c.op == o.global, c.params); err != nil {
				return err
			}
			have = true
		}
		if have {
			if o.version != nil {
				c.ifMatch = o.version(obj)
			} else {
				c.ifMatch = versionOf(res, obj)
			}
			if !versioned || (o.optionalIfMatch && (c.ifMatch == "" || c.ifMatch == "0")) {
				c.ifMatch = ""
			}
			if id, _ := obj["id"].(string); o.canonical != "" && id != "" {
				c.params[o.canonical] = id
			}
			if o.base != nil {
				base = o.base(obj)
			} else if o.merge {
				spec, _ := a.loadSpec()
				op, err := spec.Op(c.op)
				if err != nil {
					return err
				}
				src := obj
				if o.mergeKey != "" {
					src, _ = obj[o.mergeKey].(map[string]any)
				}
				base = op.project(src)
				for _, k := range o.drop {
					delete(base, k)
				}
			}
		}
		if ifMatch != "" {
			v := normalizeVersion(ifMatch)
			if _, ok := atoiPositive(v); !ok {
				return usageErrorf("--if-match must be a positive version number, got %q", ifMatch)
			}
			c.ifMatch = v
		}
		var body map[string]any
		if f != nil || o.static != nil || base != nil {
			if f != nil {
				if body, err = f.build(base); err != nil {
					return err
				}
			} else {
				body = base
				if body == nil {
					body = map[string]any{}
				}
			}
			for k, v := range o.static {
				setPath(body, k, v)
			}
		}
		for i, k := range o.bodyArgs {
			if body == nil {
				body = map[string]any{}
			}
			body[k] = args[len(o.args)+i]
		}
		if o.prepare != nil {
			if err := o.prepare(ctx, cmd, &c, body); err != nil {
				return err
			}
		}
		if body != nil {
			c.body = body
		}
		if o.confirm != "" && !yes {
			return usageErrorf("refusing to %s without --yes", o.confirm)
		}
		if o.paged && all {
			res, err = a.allPages(ctx, c)
		} else {
			res, err = a.do(ctx, c)
		}
		if err != nil {
			return err
		}
		out := res.Body
		if o.secret != "" && secretFile != "" {
			if out, err = writeSecret(out, o.secret, o.redact, secretFile); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "The one-time secret was written to %s.\n", secretFile)
		} else if o.secret != "" && format == FormatTable {
			fmt.Fprintf(cmd.ErrOrStderr(), "The %s below is shown only once; store it now.\n", o.secret)
		}
		if len(strings.TrimSpace(string(out))) == 0 {
			if o.done != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), o.done)
			}
			return nil
		}
		if o.text != "" && (textOut != "" || format == FormatTable) {
			obj, err := res.Object()
			if err != nil {
				return err
			}
			s, _ := obj[o.text].(string)
			if textOut != "" {
				n, err := a.save(cmd, textOut, force, strings.NewReader(s))
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Wrote %d bytes to %s.\n", n, textOut)
				return nil
			}
			_, err = io.WriteString(cmd.OutOrStdout(), printable(s))
			return err
		}
		if err := render(cmd.OutOrStdout(), format, out, o.view); err != nil {
			return err
		}
		if format == FormatTable {
			a.notes(cmd, out, o)
		}
		if o.check != nil {
			obj, err := res.Object()
			if err != nil {
				return err
			}
			return o.check(obj)
		}
		return nil
	}
	return cmd
}

// notes prints hints for a table: an empty list, or more pages.
func (a *App) notes(cmd *cobra.Command, body []byte, o opts) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return
	}
	if o.view.items != "" {
		if items, _ := m[o.view.items].([]any); len(items) == 0 && o.view.empty != "" {
			fmt.Fprintln(cmd.ErrOrStderr(), o.view.empty)
		}
	}
	if nc, _ := m["next_cursor"].(string); nc != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "More results: --cursor %s, or --all for every page.\n", nc)
	}
}

// bindArgs fills the operation's path parameters (other than space_id when it is already set) from positional
// arguments, resolving space slugs and node names.
func (a *App) bindArgs(ctx context.Context, opID string, specs []arg, args []string, params map[string]string) error {
	if len(specs) == 0 {
		return nil
	}
	spec, err := a.loadSpec()
	if err != nil {
		return err
	}
	op, err := spec.Op(opID)
	if err != nil {
		return err
	}
	free := []string{}
	for _, p := range op.PathParams {
		if _, ok := params[p]; !ok {
			free = append(free, p)
		}
	}
	if len(free) < len(specs) {
		return fmt.Errorf("operation %s has %d free path parameters, the command %d arguments", op.ID, len(free), len(specs))
	}
	for i, s := range specs {
		v := args[i]
		switch s.kind {
		case aSpace:
			if v, err = a.resolveSpace(ctx, v); err != nil {
				return err
			}
		case aNode:
			if v, err = a.resolveNode(ctx, v); err != nil {
				return err
			}
		}
		params[free[i]] = v
	}
	return nil
}

// allPages follows next_cursor and returns one {"items": [...]} body.
func (a *App) allPages(ctx context.Context, c call) (*Response, error) {
	var items []any
	var last *Response
	seen := map[string]bool{}
	for page := 0; page < 10000; page++ {
		res, err := a.do(ctx, c)
		if err != nil {
			return nil, err
		}
		last = res
		obj, err := res.Object()
		if err != nil {
			return nil, err
		}
		got, _ := obj["items"].([]any)
		items = append(items, got...)
		nc, _ := obj["next_cursor"].(string)
		if nc == "" || seen[nc] {
			break
		}
		seen[nc] = true
		q := url.Values{}
		for k, v := range c.query {
			q[k] = v
		}
		q.Set("cursor", nc)
		c.query = q
	}
	if items == nil {
		items = []any{}
	}
	b, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		return nil, err
	}
	return &Response{Status: last.Status, Header: last.Header, Body: b}, nil
}

// writeSecret writes the one-time secret at key (top level; a string, or a list written one per line) to a new 0600
// file and returns the body without it and without the redact fields that reveal it.
func writeSecret(body []byte, key string, redact []string, path string) ([]byte, error) {
	var m map[string]any
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.UseNumber()
	if err := d.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	s, _ := m[key].(string)
	if l, ok := m[key].([]any); ok {
		lines := make([]string, 0, len(l))
		for _, v := range l {
			lines = append(lines, scalar(v))
		}
		s = strings.Join(lines, "\n")
	}
	if s == "" {
		return nil, errors.New("the response holds no secret")
	}
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator names the file for the secret
	if err != nil {
		return nil, fmt.Errorf("write secret (the secret is lost; issue a new one): %w", err)
	}
	if _, err := fh.WriteString(s + "\n"); err != nil {
		_ = fh.Close()
		return nil, fmt.Errorf("write secret (the secret is lost; issue a new one): %w", err)
	}
	if err := fh.Close(); err != nil {
		return nil, err
	}
	m[key] = "(written to " + path + ")"
	for _, k := range redact {
		if _, ok := m[k]; ok {
			m[k] = "(written to " + path + ")"
		}
	}
	return json.Marshal(m)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// group returns a parent command.
func group(use, short string, aliases []string, children ...*cobra.Command) *cobra.Command {
	g := &cobra.Command{Use: use, Short: short, Aliases: aliases}
	g.AddCommand(children...)
	return g
}
