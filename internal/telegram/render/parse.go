package render

import (
	"html"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var gm = goldmark.New(goldmark.WithExtensions(extension.GFM, extension.Footnote))

var (
	// entityRE matches an HTML character reference as CommonMark resolves them.
	entityRE    = regexp.MustCompile(`^&(?:#[0-9]{1,7}|#[xX][0-9a-fA-F]{1,6}|[A-Za-z][A-Za-z0-9]{1,31});`)
	tagRE       = regexp.MustCompile(`(?s)<!--.*?-->|<[^>]*>`)
	breakTagRE  = regexp.MustCompile(`(?i)^<br\s*/?>$`)
	detailsOpen = regexp.MustCompile(`(?i)<details[\s>]`)
	detailsEnd  = regexp.MustCompile(`(?i)</details\s*>`)
	summaryRE   = regexp.MustCompile(`(?is)<summary[^>]*>(.*?)</summary\s*>`)
)

// Parse parses a Markdown body into a Document. It never fails: any input becomes a Document, after the
// hostile-input caps of [Limits] are applied.
func Parse(source string) *Document {
	src, cut := sanitize(source)
	b := []byte(limitNesting(src))
	root := gm.Parser().Parse(text.NewReader(b))
	c := &converter{src: b, doc: &Document{Truncated: cut}, seen: map[string]bool{}, notes: map[int][]Block{}}
	c.doc.Blocks = c.blocks(children(root), 0)
	if len(c.notes) > 0 {
		keys := make([]int, 0, len(c.notes))
		for k := range c.notes {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		renum := make(map[int]int, len(keys))
		for i, k := range keys {
			renum[k] = i + 1
			c.doc.Footnotes = append(c.doc.Footnotes, c.notes[k])
		}
		c.renumber(renum)
	}
	return c.doc
}

type converter struct {
	src   []byte
	doc   *Document
	seen  map[string]bool
	notes map[int][]Block
	// refs are the footnote references created, renumbered once every definition is known.
	refs []*FootnoteRef
}

func children(n gast.Node) []gast.Node {
	var out []gast.Node
	for ch := n.FirstChild(); ch != nil; ch = ch.NextSibling() {
		out = append(out, ch)
	}
	return out
}

// blocks converts a run of sibling blocks. depth is the number of block containers above them.
func (c *converter) blocks(nodes []gast.Node, depth int) []Block {
	var out []Block
	for i := 0; i < len(nodes); i++ {
		if hb, ok := nodes[i].(*gast.HTMLBlock); ok {
			raw := c.htmlBlock(hb)
			// Past the depth cap a <details> is plain HTML: each level rescans the siblings that follow it.
			if depth < Limits.Depth && detailsOpen.MatchString(raw) {
				d, used := c.details(raw, nodes[i+1:], depth)
				i += used
				out = append(out, d)
				continue
			}
			if t := strings.TrimSpace(htmlText(raw)); t != "" {
				out = append(out, Paragraph{Inlines: []Inline{Text(t)}})
			}
			continue
		}
		out = append(out, c.block(nodes[i], depth)...)
	}
	return out
}

func (c *converter) htmlBlock(hb *gast.HTMLBlock) string {
	raw := c.lines(hb)
	if hb.HasClosure() {
		raw += string(hb.ClosureLine.Value(c.src))
	}
	return raw
}

// details builds a <details> element that opens in raw: its summary, and the siblings in rest up to the HTML block
// that closes it. It returns the element and how many siblings it consumed.
func (c *converter) details(raw string, rest []gast.Node, depth int) (Details, int) {
	d := Details{}
	if m := summaryRE.FindStringSubmatch(raw); m != nil {
		if s := strings.TrimSpace(htmlText(m[1])); s != "" {
			d.Summary = []Inline{Text(s)}
		}
		raw = strings.Replace(raw, m[0], "", 1)
	}
	open := balance(raw)
	if t := strings.TrimSpace(htmlText(raw)); t != "" {
		d.Blocks = append(d.Blocks, Paragraph{Inlines: []Inline{Text(t)}})
	}
	used, start := 0, 0
	for used < len(rest) && open > 0 {
		hb, ok := rest[used].(*gast.HTMLBlock)
		used++
		if !ok {
			continue
		}
		r := c.htmlBlock(hb)
		if open += balance(r); open > 0 {
			continue
		}
		// The closing block: everything before it is the content.
		d.Blocks = append(d.Blocks, c.blocks(rest[start:used-1], depth+1)...)
		start = used
		if t := strings.TrimSpace(htmlText(detailsEnd.ReplaceAllString(r, ""))); t != "" {
			d.Blocks = append(d.Blocks, Paragraph{Inlines: []Inline{Text(t)}})
		}
	}
	if start < used {
		// Never closed: the rest of the run is the content.
		d.Blocks = append(d.Blocks, c.blocks(rest[start:used], depth+1)...)
	}
	return d, used
}

func balance(s string) int {
	return len(detailsOpen.FindAllStringIndex(s, -1)) - len(detailsEnd.FindAllStringIndex(s, -1))
}

// appendBlocks appends b, or b's content when containers are too deep to keep.
func appendBlocks(out []Block, b Block, lift bool) []Block {
	if !lift {
		return append(out, b)
	}
	switch b := b.(type) {
	case Details:
		if len(b.Summary) > 0 {
			out = append(out, Paragraph{Inlines: b.Summary})
		}
		return append(out, b.Blocks...)
	case Quote:
		return append(out, b.Blocks...)
	case List:
		for _, it := range b.Items {
			out = append(out, it.Blocks...)
		}
		return out
	}
	return append(out, b)
}

func (c *converter) block(n gast.Node, depth int) []Block {
	lift := depth >= Limits.Depth
	switch n := n.(type) {
	case *gast.Paragraph, *gast.TextBlock:
		if in := c.inlines(n, 0); len(in) > 0 {
			return []Block{Paragraph{Inlines: in}}
		}
	case *gast.Heading:
		return []Block{Heading{Level: min(max(n.Level, 1), 6), Inlines: c.inlines(n, 0)}}
	case *gast.ThematicBreak:
		return []Block{Rule{}}
	case *gast.FencedCodeBlock:
		lang := ""
		if n.Info != nil {
			lang = codeLanguage(string(n.Info.Value(c.src)))
		}
		return []Block{CodeBlock{Lang: lang, Text: strings.TrimSuffix(c.lines(n), "\n")}}
	case *gast.CodeBlock:
		return []Block{CodeBlock{Text: strings.TrimSuffix(c.lines(n), "\n")}}
	case *gast.Blockquote:
		return appendBlocks(nil, Quote{Blocks: c.blocks(children(n), depth+1)}, lift)
	case *gast.List:
		l := List{Ordered: n.IsOrdered(), Start: max(n.Start, 0)}
		for it := n.FirstChild(); it != nil; it = it.NextSibling() {
			item := ListItem{}
			if fc := it.FirstChild(); fc != nil {
				if cb, ok := fc.FirstChild().(*east.TaskCheckBox); ok {
					item.Task, item.Checked = true, cb.IsChecked
				}
			}
			item.Blocks = c.blocks(children(it), depth+1)
			l.Items = append(l.Items, item)
		}
		return appendBlocks(nil, l, lift)
	case *east.Table:
		t := Table{}
		for _, a := range n.Alignments {
			t.Align = append(t.Align, align(a))
		}
		for r := n.FirstChild(); r != nil; r = r.NextSibling() {
			var row []Cell
			for cell := r.FirstChild(); cell != nil; cell = cell.NextSibling() {
				row = append(row, Cell(c.inlines(cell, 0)))
			}
			for len(row) < len(t.Align) {
				row = append(row, nil)
			}
			row = row[:len(t.Align)]
			if _, ok := r.(*east.TableHeader); ok {
				t.Header = row
			} else {
				t.Rows = append(t.Rows, row)
			}
		}
		return []Block{t}
	case *east.FootnoteList:
		for fn := n.FirstChild(); fn != nil; fn = fn.NextSibling() {
			if f, ok := fn.(*east.Footnote); ok {
				c.notes[f.Index] = c.blocks(children(f), depth+1)
			}
		}
	case *gast.HTMLBlock:
		return c.blocks([]gast.Node{n}, depth)
	}
	// LinkReferenceDefinition and anything unknown render as nothing.
	return nil
}

func align(a east.Alignment) Align {
	switch a {
	case east.AlignLeft:
		return AlignLeft
	case east.AlignCenter:
		return AlignCenter
	case east.AlignRight:
		return AlignRight
	}
	return AlignNone
}

// lines returns the raw source lines of a block.
func (c *converter) lines(n gast.Node) string {
	var b strings.Builder
	l := n.Lines()
	for i := range l.Len() {
		seg := l.At(i)
		b.Write(seg.Value(c.src))
	}
	return b.String()
}

// inlines converts the inline children of n. depth is the number of styles and links above them.
func (c *converter) inlines(n gast.Node, depth int) []Inline {
	var out []Inline
	for ch := n.FirstChild(); ch != nil; ch = ch.NextSibling() {
		out = c.inline(out, ch, depth)
	}
	return out
}

// styled wraps children in a style, or drops the style when styles nest too deeply.
func styled(out []Inline, s Style, kids []Inline, depth int) []Inline {
	if depth >= Limits.InlineDepth {
		return appendInlines(out, kids...)
	}
	if len(kids) == 0 {
		return out
	}
	return append(out, Styled{Style: s, Children: kids})
}

func (c *converter) inline(out []Inline, n gast.Node, depth int) []Inline {
	switch n := n.(type) {
	case *gast.Text:
		v := n.Segment.Value(c.src)
		if n.IsRaw() {
			out = appendInlines(out, Text(v))
		} else {
			out = appendInlines(out, Text(unescape(v)))
		}
		switch {
		case n.HardLineBreak():
			out = append(out, Break{})
		case n.SoftLineBreak():
			out = appendInlines(out, Text(" "))
		}
	case *gast.String:
		if n.IsRaw() || n.IsCode() {
			return appendInlines(out, Text(n.Value))
		}
		return appendInlines(out, Text(unescape(n.Value)))
	case *gast.CodeSpan:
		var b strings.Builder
		for ch := n.FirstChild(); ch != nil; ch = ch.NextSibling() {
			switch t := ch.(type) {
			case *gast.Text:
				v := string(t.Segment.Value(c.src))
				b.WriteString(strings.ReplaceAll(v, "\n", " "))
			case *gast.String:
				b.Write(t.Value)
			}
		}
		if b.Len() > 0 {
			out = append(out, Code(b.String()))
		}
	case *gast.Emphasis:
		s := Italic
		if n.Level >= 2 {
			s = Bold
		}
		return styled(out, s, c.inlines(n, depth+1), depth)
	case *east.Strikethrough:
		return styled(out, Strike, c.inlines(n, depth+1), depth)
	case *gast.Link:
		kids := c.inlines(n, depth+1)
		u, ok := AllowedURL(unescape(n.Destination))
		if !ok || depth >= Limits.InlineDepth {
			return appendInlines(out, kids...)
		}
		if len(kids) == 0 {
			kids = []Inline{Text(u)}
		}
		return append(out, Link{URL: u, Children: kids})
	case *gast.AutoLink:
		label := string(n.Label(c.src))
		raw := string(n.URL(c.src))
		if n.AutoLinkType == gast.AutoLinkEmail && !strings.HasPrefix(strings.ToLower(raw), "mailto:") {
			raw = "mailto:" + raw
		}
		if u, ok := AllowedURL(raw); ok && depth < Limits.InlineDepth {
			return append(out, Link{URL: u, Children: []Inline{Text(label)}})
		}
		return appendInlines(out, Text(label))
	case *gast.Image:
		alt := plain(c.inlines(n, depth+1))
		dest := unescape(n.Destination)
		if id, ok := attachmentID(dest); ok {
			c.attach(id)
			return append(out, Attachment{ID: id, Alt: alt})
		}
		if alt == "" {
			alt = "image"
		}
		if u, ok := AllowedURL(dest); ok && depth < Limits.InlineDepth {
			return append(out, Link{URL: u, Children: []Inline{Text(alt)}})
		}
		return appendInlines(out, Text(alt))
	case *gast.RawHTML:
		var b strings.Builder
		for i := range n.Segments.Len() {
			seg := n.Segments.At(i)
			b.Write(seg.Value(c.src))
		}
		if breakTagRE.MatchString(strings.TrimSpace(b.String())) {
			return append(out, Break{})
		}
		// Other tags are dropped; the text between them is ordinary text nodes.
	case *east.FootnoteLink:
		r := &FootnoteRef{N: n.Index}
		c.refs = append(c.refs, r)
		return append(out, r)
	case *east.FootnoteBacklink, *east.TaskCheckBox:
	default:
		return appendInlines(out, c.inlines(n, depth)...)
	}
	return out
}

func (c *converter) attach(id string) {
	if c.seen[id] || len(c.doc.Attachments) >= Limits.Attachments {
		return
	}
	c.seen[id] = true
	c.doc.Attachments = append(c.doc.Attachments, id)
}

// renumber points the footnote references at the footnotes' order in Document.Footnotes; a reference to a missing
// definition becomes 0 (shown as text).
func (c *converter) renumber(m map[int]int) {
	for _, r := range c.refs {
		r.N = m[r.N]
	}
}

// appendInlines appends inline nodes, merging adjacent text.
func appendInlines(out []Inline, in ...Inline) []Inline {
	for _, n := range in {
		if t, ok := n.(Text); ok {
			if t == "" {
				continue
			}
			if len(out) > 0 {
				if p, ok := out[len(out)-1].(Text); ok {
					out[len(out)-1] = p + t
					continue
				}
			}
		}
		out = append(out, n)
	}
	return out
}

// plain returns the text of inline content without styles.
func plain(in []Inline) string {
	var b strings.Builder
	var walk func([]Inline)
	walk = func(in []Inline) {
		for _, n := range in {
			switch n := n.(type) {
			case Text:
				b.WriteString(string(n))
			case Code:
				b.WriteString(string(n))
			case Break:
				b.WriteByte(' ')
			case Styled:
				walk(n.Children)
			case Link:
				walk(n.Children)
			case Attachment:
				b.WriteString(n.Alt)
			case *FootnoteRef:
				b.WriteString(footnoteMark(n.N))
			}
		}
	}
	walk(in)
	return b.String()
}

// unescape resolves backslash escapes and character references in one pass, as CommonMark does for text.
func unescape(v []byte) string {
	if !strings.ContainsAny(string(v), `\&`) {
		return string(v)
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '\\' && i+1 < len(v) && util.IsPunct(v[i+1]):
			b.WriteByte(v[i+1])
			i++
		case c == '&':
			if m := entityRE.Find(v[i:]); m != nil {
				r := html.UnescapeString(string(m))
				if strings.ContainsRune(r, 0) {
					r = "�"
				}
				b.WriteString(r)
				i += len(m) - 1
				continue
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return strings.ToValidUTF8(b.String(), "�")
}

// htmlText reduces raw HTML to its text: tags and comments are dropped and character references resolved.
func htmlText(raw string) string {
	t := tagRE.ReplaceAllStringFunc(raw, func(tag string) string {
		if breakTagRE.MatchString(tag) {
			return "\n"
		}
		return ""
	})
	t = html.UnescapeString(t)
	t, _ = sanitize(t)
	return t
}

// footnoteMark is how a footnote reference shows in text: "[1]", or "[?]" for a missing definition.
func footnoteMark(n int) string {
	if n <= 0 {
		return "[?]"
	}
	return "[" + strconv.Itoa(n) + "]"
}
