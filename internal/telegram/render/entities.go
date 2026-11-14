package render

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

// Entity types SpaceMan sends and reads (https://core.telegram.org/bots/api#messageentity).
const (
	EntityBold          = "bold"
	EntityItalic        = "italic"
	EntityUnderline     = "underline"
	EntityStrikethrough = "strikethrough"
	EntitySpoiler       = "spoiler"
	EntityCode          = "code"
	EntityPre           = "pre"
	EntityTextLink      = "text_link"
	EntityBlockquote    = "blockquote"
	EntityExpandable    = "expandable_blockquote"
)

// Entity is a Telegram MessageEntity. Offset and Length count UTF-16 code units. Inbound entities may carry other
// types and fields; those SpaceMan doesn't need are not decoded.
type Entity struct {
	Type     string `json:"type"`
	Offset   int    `json:"offset"`
	Length   int    `json:"length"`
	URL      string `json:"url,omitempty"`
	Language string `json:"language,omitempty"`
	// CustomEmojiID is set on inbound custom_emoji entities; never sent.
	CustomEmojiID string `json:"custom_emoji_id,omitempty"`
}

// Button is an inline keyboard button that opens the Mini App at WebAppURL (web_app buttons work in private chats
// only).
type Button struct {
	Text      string
	WebAppURL string
}

// Part is one message: plain text, its entities, and the Mini App buttons for the diagrams it shows.
type Part struct {
	Text     string
	Entities []Entity
	Buttons  []Button
}

// Message is a body rendered as entity messages.
type Message struct {
	Parts []Part
	// Truncated reports that the body didn't fit in Options.MaxParts parts (or was longer than Limits.Source); the
	// last part ends with a "continued in the app" line and, when Options.ContinueURL is set, a button.
	Truncated bool
}

// Options control rendering.
type Options struct {
	// Limit is the length of one part in UTF-16 code units; 0 means Limits.Text (4096), and values below 64 count as 64.
	// Use Limits.Caption for captions.
	Limit int
	// MaxParts is the number of parts at most; 0 means Limits.Parts (4).
	MaxParts int
	// DiagramURL returns the Mini App URL that shows the n-th Mermaid diagram of the body (from 0). Nil means no
	// "View diagram" buttons (for example in group chats, where web_app buttons don't work).
	DiagramURL func(n int) string
	// ContinueURL is the Mini App URL of the whole body, offered when it is truncated. Empty means no button.
	ContinueURL string
}

func (o Options) limit() int {
	if o.Limit <= 0 {
		return Limits.Text
	}
	return max(o.Limit, 64)
}

func (o Options) maxParts() int {
	if o.MaxParts <= 0 {
		return Limits.Parts
	}
	return o.MaxParts
}

// Text strings of the renderer.
const (
	bullet         = "•"
	boxOpen        = "☐"
	boxChecked     = "☑"
	ruleText       = "──────────"
	tableInApp     = "[table — open in app]"
	continuedInApp = "… continued in the app"
	viewDiagram    = "View diagram"
	openInApp      = "Open in app"
	imageMark      = "🖼"
)

// expandableAt is the length (UTF-16 units) or line count from which a quote is sent as an expandable blockquote.
const (
	expandableAt      = 400
	expandableAtLines = 6
)

// builder accumulates text in UTF-16 units with entities. Open entities form a stack, so entities always nest.
type builder struct {
	buf   []uint16
	ents  []Entity
	stack []Entity
	// breaks are offsets where a split is preferred, with their priority (higher is better).
	breaks []brk
	// diagrams are the offsets of the Mermaid blocks, in order.
	diagrams []int
	// quote is set while a blockquote entity is open (blockquotes can't nest).
	quote bool
}

type brk struct{ at, prio int }

const (
	prioLine  = 1
	prioItem  = 2
	prioBlock = 3
)

func (b *builder) write(s string) {
	for _, r := range s {
		b.buf = utf16.AppendRune(b.buf, r)
	}
}

func (b *builder) push(e Entity) {
	e.Offset = len(b.buf)
	b.stack = append(b.stack, e)
}

func (b *builder) pop() {
	e := b.stack[len(b.stack)-1]
	b.stack = b.stack[:len(b.stack)-1]
	if e.Length = len(b.buf) - e.Offset; e.Length > 0 {
		b.ents = append(b.ents, e)
	}
}

// has reports whether an entity of type t is open.
func (b *builder) has(t string) bool {
	return slices.ContainsFunc(b.stack, func(e Entity) bool { return e.Type == t })
}

// suspend closes the open entities above index from (top first) and returns them, to reopen with resume.
func (b *builder) suspend(from int) []Entity {
	saved := slices.Clone(b.stack[from:])
	for len(b.stack) > from {
		b.pop()
	}
	return saved
}

func (b *builder) resume(saved []Entity) {
	for _, e := range saved {
		e.Length = 0
		b.push(e)
	}
}

// styleBase returns the stack index above which only style entities are open.
func (b *builder) styleBase() int {
	i := len(b.stack)
	for i > 0 && isStyle(b.stack[i-1].Type) {
		i--
	}
	return i
}

func isStyle(t string) bool {
	switch t {
	case EntityBold, EntityItalic, EntityUnderline, EntityStrikethrough, EntitySpoiler:
		return true
	}
	return false
}

// code writes an inline code span. Styles can't contain code, so open styles are closed around it; a link can't contain
// code either, so inside a link the span is plain text.
func (b *builder) code(s string) {
	if b.has(EntityTextLink) {
		b.write(s)
		return
	}
	saved := b.suspend(b.styleBase())
	b.push(Entity{Type: EntityCode})
	b.write(s)
	b.pop()
	b.resume(saved)
}

// Entities renders the document as entity messages.
func (d *Document) Entities(o Options) Message {
	b := &builder{}
	r := &entityRenderer{b: b, o: o}
	r.blocks(d.Blocks, 0, true)
	if len(d.Footnotes) > 0 {
		r.sep(prioBlock)
		b.write(ruleText)
		for i, fn := range d.Footnotes {
			b.breaks = append(b.breaks, brk{len(b.buf), prioItem})
			b.write("\n" + footnoteMark(i+1) + " ")
			r.blocks(fn, 1, false)
		}
	}
	for len(b.stack) > 0 {
		b.pop()
	}
	b.tidy()
	return split(b, o, d.Truncated)
}

type entityRenderer struct {
	b *builder
	o Options
}

// sep separates the next block from the previous one: a blank line between top-level blocks, a line break inside
// containers.
func (r *entityRenderer) sep(prio int) {
	r.b.breaks = append(r.b.breaks, brk{len(r.b.buf), prio})
	if prio == prioBlock {
		r.b.write("\n\n")
	} else {
		r.b.write("\n")
	}
}

// blocks renders blocks; indent is the list nesting level, top says whether they are top-level blocks.
func (r *entityRenderer) blocks(bs []Block, indent int, top bool) {
	prio := prioItem
	if top {
		prio = prioBlock
	}
	for i, bl := range bs {
		if i > 0 {
			r.sep(prio)
		}
		r.block(bl, indent)
	}
}

func (r *entityRenderer) block(bl Block, indent int) {
	b := r.b
	switch bl := bl.(type) {
	case Paragraph:
		r.inlines(bl.Inlines)
	case Heading:
		saved := b.suspend(b.styleBase())
		b.push(Entity{Type: EntityBold})
		r.inlines(bl.Inlines)
		b.pop()
		b.resume(saved)
	case CodeBlock:
		r.pre(bl)
	case Rule:
		b.write(ruleText)
	case Quote:
		r.quote(func() { r.blocks(bl.Blocks, indent, false) }, false)
	case Details:
		if len(bl.Summary) > 0 {
			b.push(Entity{Type: EntityBold})
			r.inlines(bl.Summary)
			b.pop()
			r.sep(prioItem)
		}
		r.quote(func() { r.blocks(bl.Blocks, indent, false) }, true)
	case List:
		for i, it := range bl.Items {
			if i > 0 {
				r.sep(prioItem)
			}
			b.write(strings.Repeat("  ", indent))
			switch {
			case it.Task && it.Checked:
				b.write(boxChecked + " ")
			case it.Task:
				b.write(boxOpen + " ")
			case bl.Ordered:
				b.write(strconv.Itoa(bl.Start+i) + ". ")
			default:
				b.write(bullet + " ")
			}
			r.item(it.Blocks, indent+1)
		}
	case Table:
		r.table(bl)
	}
}

// item renders a list item's blocks: the first follows the marker, the rest go on their own lines.
func (r *entityRenderer) item(bs []Block, indent int) {
	for i, bl := range bs {
		if i > 0 {
			r.sep(prioLine)
			if _, ok := bl.(List); !ok {
				r.b.write(strings.Repeat("  ", indent))
			}
		}
		r.block(bl, indent)
	}
}

// quote renders content as a blockquote, or plainly inside another quote (blockquotes can't nest). Long content, and
// details content, becomes an expandable blockquote.
func (r *entityRenderer) quote(content func(), expandable bool) {
	b := r.b
	if b.quote {
		content()
		return
	}
	b.quote = true
	start, n := len(b.buf), len(b.ents)
	b.push(Entity{Type: EntityBlockquote})
	idx := len(b.stack) - 1
	content()
	for len(b.stack) > idx+1 {
		b.pop()
	}
	q := b.stack[idx]
	b.pop()
	b.quote = false
	if q.Offset < start || len(b.buf) == start {
		return
	}
	// The quote may have been closed and reopened around code blocks: mark the pieces expandable when the whole is long.
	lines := strings.Count(utf16Decode(b.buf[start:]), "\n") + 1
	if expandable || len(b.buf)-start >= expandableAt || lines >= expandableAtLines {
		for i := n; i < len(b.ents); i++ {
			if b.ents[i].Type == EntityBlockquote {
				b.ents[i].Type = EntityExpandable
			}
		}
	}
}

// pre writes a code block. A pre entity can't sit inside a quote or style, so those are closed around it.
func (r *entityRenderer) pre(cb CodeBlock) {
	b := r.b
	saved := b.suspend(0)
	if cb.IsDiagram() {
		b.diagrams = append(b.diagrams, len(b.buf))
	}
	b.push(Entity{Type: EntityPre, Language: cb.Lang})
	text := cb.Text
	if text == "" {
		text = " "
	}
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			b.breaks = append(b.breaks, brk{len(b.buf), prioLine})
			b.write("\n")
		}
		b.write(line)
	}
	b.pop()
	b.resume(saved)
}

func (r *entityRenderer) table(t Table) {
	text, ok := tableText(t)
	if !ok {
		r.b.write(tableInApp)
		return
	}
	r.pre(CodeBlock{Text: text})
}

// tableText lays a table out as monospace text, or reports false when it is wider than Limits.TableWidth.
func tableText(t Table) (string, bool) {
	cols := len(t.Align)
	if cols == 0 {
		return "", false
	}
	rows := append([][]Cell{t.Header}, t.Rows...)
	cells := make([][]string, len(rows))
	width := make([]int, cols)
	for i, row := range rows {
		cells[i] = make([]string, cols)
		for j := range cols {
			var s string
			if j < len(row) {
				s = strings.Join(strings.Fields(plain(row[j])), " ")
			}
			cells[i][j] = s
			width[j] = max(width[j], displayWidth(s))
		}
	}
	total := 3*cols + 1
	for _, w := range width {
		total += w
	}
	if total > Limits.TableWidth {
		return "", false
	}
	var sb strings.Builder
	line := func(row []string) {
		sb.WriteString("|")
		for j, s := range row {
			pad := width[j] - displayWidth(s)
			left := 0
			switch t.Align[j] {
			case AlignRight:
				left = pad
			case AlignCenter:
				left = pad / 2
			}
			sb.WriteString(" " + strings.Repeat(" ", left) + s + strings.Repeat(" ", pad-left) + " |")
		}
	}
	for i, row := range cells {
		if i > 0 {
			sb.WriteByte('\n')
		}
		line(row)
		if i == 0 {
			sb.WriteString("\n|")
			for j := range cols {
				sb.WriteString(strings.Repeat("-", width[j]+2) + "|")
			}
		}
	}
	return sb.String(), true
}

// displayWidth approximates the monospace columns of s: East Asian wide characters and emoji take two, combining marks
// and joiners none.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200d || (r >= 0xfe00 && r <= 0xfe0f):
		case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf, r >= 0xac00 && r <= 0xd7a3,
			r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60,
			r >= 0xffe0 && r <= 0xffe6, r >= 0x1f300 && r <= 0x1faff, r >= 0x20000 && r <= 0x3fffd:
			w += 2
		default:
			w++
		}
	}
	return w
}

func (r *entityRenderer) inlines(in []Inline) {
	b := r.b
	for _, n := range in {
		switch n := n.(type) {
		case Text:
			b.write(string(n))
		case Break:
			b.breaks = append(b.breaks, brk{len(b.buf), prioLine})
			b.write("\n")
		case Code:
			b.code(string(n))
		case Styled:
			b.push(Entity{Type: styleEntity(n.Style)})
			r.inlines(n.Children)
			b.pop()
		case Link:
			if b.has(EntityTextLink) {
				r.inlines(n.Children)
				continue
			}
			b.push(Entity{Type: EntityTextLink, URL: n.URL})
			r.inlines(n.Children)
			b.pop()
		case Attachment:
			alt := n.Alt
			if alt == "" {
				alt = "image"
			}
			b.write(imageMark + " " + alt)
		case *FootnoteRef:
			b.write(footnoteMark(n.N))
		}
	}
}

func styleEntity(s Style) string {
	switch s {
	case Bold:
		return EntityBold
	case Strike:
		return EntityStrikethrough
	}
	return EntityItalic
}

// tidy shrinks entities to exclude leading and trailing whitespace (a quote reopened after a code block starts with
// the line break, a style closed around inline code ends with a space) and drops the empty ones. Styles and links
// never contain a code entity, so they lose spaces too; a quote may contain inline code that starts or ends with a
// space, so it loses only line breaks (code never holds one). Code and pre keep their whitespace.
func (b *builder) tidy() {
	out := b.ents[:0]
	for _, e := range b.ents {
		if e.Type != EntityCode && e.Type != EntityPre {
			trim := isSpace
			if isQuoteType(e.Type) {
				trim = func(u uint16) bool { return u == '\n' }
			}
			s, t := e.Offset, e.Offset+e.Length
			for s < t && trim(b.buf[s]) {
				s++
			}
			for t > s && trim(b.buf[t-1]) {
				t--
			}
			e.Offset, e.Length = s, t-s
		}
		if e.Length > 0 {
			out = append(out, e)
		}
	}
	b.ents = out
}

func isQuoteType(t string) bool { return t == EntityBlockquote || t == EntityExpandable }

// sortEntities orders entities by offset, enclosing ones first.
func sortEntities(es []Entity) {
	slices.SortStableFunc(es, func(a, b Entity) int {
		return cmp.Or(cmp.Compare(a.Offset, b.Offset), cmp.Compare(b.Length, a.Length))
	})
}
