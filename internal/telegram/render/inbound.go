package render

import (
	"cmp"
	"slices"
	"strings"
	"unicode/utf16"
)

// maxInboundEntities is the number of entities of one inbound message that are read; the rest are ignored.
const maxInboundEntities = 1000

// Markdown converts an inbound Telegram text with entities (offsets in UTF-16 code units) into Markdown. Plain runs
// have their Markdown metacharacters escaped, so the text reads back as it was sent. Bold, italic, strikethrough,
// code, pre (with language), text links (allowlisted URLs only), URLs, e-mail addresses, mentions and block quotes
// keep their meaning; spoiler, underline and every other entity become plain text, and a custom emoji is its alt text
// (which is already the entity's text).
//
// Entities are untrusted: out-of-range, empty, overlapping and illegally nested entities are ignored, and offsets
// that fall inside a surrogate pair are widened to whole characters. It never panics.
func Markdown(text string, entities []Entity) string {
	text = strings.ToValidUTF8(text, "�")
	u := utf16.Encode([]rune(text))
	for i, c := range u {
		if (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f {
			u[i] = ' '
		}
	}
	w := &mdWriter{u: u, lineStart: true}
	root := &inode{end: len(u)}
	root.kids = tree(u, entities)
	w.node(root)
	return strings.TrimRight(w.out.String(), "\n")
}

type inode struct {
	e          Entity
	start, end int
	kids       []*inode
}

// inline entity kinds that can be part of others; leaf kinds contain nothing; block kinds start lines.
func entityClass(t string) (known, leaf, block bool) {
	switch t {
	case EntityBold, EntityItalic, EntityStrikethrough:
		return true, false, false
	case EntityTextLink:
		return true, false, false
	case EntityCode, "url", "email", "mention":
		return true, true, false
	case EntityPre:
		return true, true, true
	case EntityBlockquote, EntityExpandable:
		return true, false, true
	}
	return false, false, false
}

func isLinkish(t string) bool {
	switch t {
	case EntityTextLink, "url", "email", "mention", EntityCode, EntityPre:
		return true
	}
	return false
}

// tree validates the entities and nests them. Entities that partly overlap an earlier one, or that break Telegram's
// nesting rules, are dropped.
func tree(u []uint16, ents []Entity) []*inode {
	n := len(u)
	var nodes []*inode
	for i, e := range ents {
		if i >= maxInboundEntities {
			break
		}
		if known, _, _ := entityClass(e.Type); !known || e.Offset < 0 || e.Length <= 0 || e.Offset >= n {
			continue
		}
		start, end := e.Offset, e.Offset+min(e.Length, n-e.Offset)
		if start > 0 && utf16.IsSurrogate(rune(u[start])) && u[start] >= 0xdc00 {
			start--
		}
		if end < n && utf16.IsSurrogate(rune(u[end])) && u[end] >= 0xdc00 {
			end++
		}
		nodes = append(nodes, &inode{e: e, start: start, end: end})
	}
	slices.SortStableFunc(nodes, func(a, b *inode) int {
		return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(b.end, a.end), cmp.Compare(rank(a.e.Type), rank(b.e.Type)))
	})
	root := &inode{end: n}
	stack := []*inode{root}
	for _, nd := range nodes {
		for len(stack) > 1 && stack[len(stack)-1].end <= nd.start {
			stack = stack[:len(stack)-1]
		}
		top := stack[len(stack)-1]
		if nd.end > top.end || !canContain(stack, nd.e.Type) {
			continue
		}
		top.kids = append(top.kids, nd)
		stack = append(stack, nd)
	}
	return root.kids
}

// rank orders entities with the same range: blocks outside, links next, styles inside.
func rank(t string) int {
	switch t {
	case EntityBlockquote, EntityExpandable:
		return 0
	case EntityPre:
		return 1
	case EntityTextLink, "url", "email", "mention", EntityCode:
		return 2
	}
	return 3
}

// canContain reports whether an entity of type t may open inside the open entities of stack.
func canContain(stack []*inode, t string) bool {
	_, _, block := entityClass(t)
	for _, p := range stack[1:] {
		pt := p.e.Type
		if _, leaf, _ := entityClass(pt); leaf {
			return false
		}
		if block && (rank(pt) >= 2 || pt == EntityBlockquote || pt == EntityExpandable) {
			// A quote or pre starts lines: not inside a link or style, and quotes don't nest.
			if t != EntityPre || (pt != EntityBlockquote && pt != EntityExpandable) {
				return false
			}
		}
		if isLinkish(t) && pt == EntityTextLink {
			return false
		}
	}
	return true
}

// mdWriter writes Markdown, tracking line starts (for escaping) and block quote prefixes.
type mdWriter struct {
	u   []uint16
	out strings.Builder
	// lineStart is set at the start of a line, after any quote prefix.
	lineStart bool
	// quote is set inside a block quote: every new line starts with "> ".
	quote bool
	// pending is the number of line breaks not yet written.
	pending int
	// afterBlock is set after a block (code fence, quote) ended, so the next break is a plain newline.
	afterBlock bool
	// inline counts open inline spans, inside which line breaks are hard breaks.
	inline int
}

func (w *mdWriter) newline() {
	w.out.WriteByte('\n')
	if w.quote {
		w.out.WriteString("> ")
	}
	w.lineStart = true
}

// flush writes the pending line breaks before content; block says the content starts a block.
func (w *mdWriter) flush(block bool) {
	n := w.pending
	w.pending = 0
	if w.out.Len() == 0 {
		w.afterBlock = false
		return
	}
	if (w.afterBlock || block) && n == 0 && !w.lineStart {
		n = 1
	}
	switch {
	case n == 0:
	case n >= 2 && w.inline == 0:
		w.newline()
		w.newline()
	case w.afterBlock || block:
		w.newline()
	default:
		w.out.WriteByte('\\')
		w.newline()
	}
	w.afterBlock = false
}

func (w *mdWriter) node(nd *inode) {
	pos := nd.start
	for _, k := range nd.kids {
		w.plain(pos, k.start)
		w.entity(k)
		pos = k.end
	}
	w.plain(pos, nd.end)
}

// text returns the units [a, b) as a string.
func (w *mdWriter) text(a, b int) string { return utf16Decode(w.u[a:b]) }

// plain writes a plain run, escaping Markdown metacharacters.
func (w *mdWriter) plain(a, b int) {
	rs := []rune(w.text(a, b))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == '\n' {
			w.pending++
			continue
		}
		w.flush(false)
		if w.lineStart {
			// Leading whitespace of four columns or a tab would start an indented code block.
			j := i
			for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
				j++
			}
			if j > i {
				ws := rs[i:j]
				// CommonMark strips leading whitespace (or makes code of it): keep it as character references.
				{
					for _, c := range ws {
						if c == '\t' {
							w.out.WriteString("&#9;")
						} else {
							w.out.WriteString("&#32;")
						}
					}
				}
				i = j - 1
				continue
			}
			w.lineStart = false
			switch {
			case strings.ContainsRune("#-+=", r):
				w.out.WriteByte('\\')
			case r >= '0' && r <= '9':
				k := i
				for k < len(rs) && k-i < 10 && rs[k] >= '0' && rs[k] <= '9' {
					k++
				}
				if k < len(rs) && (rs[k] == '.' || rs[k] == ')') {
					w.out.WriteString(string(rs[i:k]))
					w.out.WriteByte('\\')
					w.out.WriteRune(rs[k])
					i = k
					continue
				}
			}
		}
		if strings.ContainsRune("\\`*_[]<>~|&", r) {
			w.out.WriteByte('\\')
		}
		w.out.WriteRune(r)
	}
}

func (w *mdWriter) entity(nd *inode) {
	switch t := nd.e.Type; t {
	case EntityBold:
		w.span("**", nd)
	case EntityItalic:
		w.span("*", nd)
	case EntityStrikethrough:
		w.span("~~", nd)
	case EntityCode:
		w.flush(false)
		w.lineStart = false
		w.out.WriteString(codeSpan(strings.ReplaceAll(w.text(nd.start, nd.end), "\n", " ")))
	case EntityPre:
		w.pre(nd)
	case EntityBlockquote, EntityExpandable:
		w.blockquote(nd)
	case EntityTextLink:
		u, ok := AllowedURL(nd.e.URL)
		if !ok {
			w.node(nd)
			return
		}
		w.link(nd, u)
	case "url":
		s := w.text(nd.start, nd.end)
		l := strings.ToLower(s)
		if u, ok := AllowedURL(s); ok && (strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")) {
			w.link(nd, u)
			return
		}
		w.node(nd)
	case "email":
		s := w.text(nd.start, nd.end)
		if u, ok := AllowedURL("mailto:" + s); ok {
			w.link(nd, u)
			return
		}
		w.node(nd)
	case "mention":
		s := w.text(nd.start, nd.end)
		if usernameRE.MatchString(s) {
			w.link(nd, "https://t.me/"+s[1:])
			return
		}
		w.node(nd)
	default:
		w.node(nd)
	}
}

// span writes an emphasis span. Leading and trailing whitespace goes outside the delimiters, where CommonMark needs
// it; a span of only whitespace is written plainly.
func (w *mdWriter) span(delim string, nd *inode) {
	s, e := nd.start, nd.end
	for s < e && isSpace(w.u[s]) {
		s++
	}
	for e > s && isSpace(w.u[e-1]) {
		e--
	}
	if s == e {
		w.node(nd)
		return
	}
	inner := &inode{start: s, end: e}
	for _, k := range nd.kids {
		if k.start >= s && k.end <= e {
			inner.kids = append(inner.kids, k)
		}
	}
	w.plain(nd.start, s)
	w.flush(false)
	w.lineStart = false
	w.out.WriteString(delim)
	w.inline++
	w.node(inner)
	w.inline--
	w.pending = min(w.pending, 1)
	w.flush(false)
	w.out.WriteString(delim)
	w.plain(e, nd.end)
}

func (w *mdWriter) link(nd *inode, u string) {
	w.flush(false)
	w.lineStart = false
	w.out.WriteByte('[')
	w.inline++
	w.node(nd)
	w.inline--
	w.pending = min(w.pending, 1)
	w.flush(false)
	r := strings.NewReplacer("<", "%3C", ">", "%3E", "\\", "%5C", "\n", "")
	w.out.WriteString("](<" + r.Replace(u) + ">)")
}

func (w *mdWriter) pre(nd *inode) {
	w.flush(true)
	if !w.lineStart {
		w.newline()
	}
	body := w.text(nd.start, nd.end)
	fence := strings.Repeat("`", max(3, longestRun(body, '`')+1))
	w.out.WriteString(fence + codeLanguage(nd.e.Language))
	for line := range strings.SplitSeq(body, "\n") {
		w.newline()
		w.out.WriteString(line)
	}
	w.newline()
	w.out.WriteString(fence)
	w.lineStart = false
	w.afterBlock = true
}

func (w *mdWriter) blockquote(nd *inode) {
	w.flush(true)
	if !w.lineStart {
		w.newline()
	}
	w.quote = true
	w.out.WriteString("> ")
	w.lineStart = true
	s, e := nd.start, nd.end
	for s < e && w.u[s] == '\n' {
		s++
	}
	inner := &inode{start: s, end: e}
	for _, k := range nd.kids {
		if k.start >= s {
			inner.kids = append(inner.kids, k)
		}
	}
	w.node(inner)
	w.pending = 0
	w.quote = false
	w.afterBlock = false
	w.pending = 2
}

// codeSpan wraps s in a backtick run longer than any inside it, padding with spaces where CommonMark would otherwise
// strip or join them.
func codeSpan(s string) string {
	fence := strings.Repeat("`", longestRun(s, '`')+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") ||
		(strings.HasPrefix(s, " ") && strings.HasSuffix(s, " ") && strings.Trim(s, " ") != "") {
		s = " " + s + " "
	}
	return fence + s + fence
}

func longestRun(s string, c byte) int {
	best, n := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			n++
			best = max(best, n)
		} else {
			n = 0
		}
	}
	return best
}
