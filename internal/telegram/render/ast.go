package render

// Document is a parsed body: SpaceMan's own small AST, independent of goldmark's.
type Document struct {
	Blocks []Block
	// Footnotes holds the footnote definitions in reference order; FootnoteRef.N indexes them from 1.
	Footnotes [][]Block
	// Attachments lists the distinct attachment IDs referenced by images, in order (at most Limits.Attachments).
	Attachments []string
	// Truncated reports that the source was longer than Limits.Source and only its start was parsed.
	Truncated bool
}

// Block is a block-level node.
type Block interface{ block() }

// Inline is an inline node.
type Inline interface{ inline() }

type (
	// Paragraph is a run of inline content.
	Paragraph struct{ Inlines []Inline }
	// Heading is a heading of Level 1-6.
	Heading struct {
		Level   int
		Inlines []Inline
	}
	// CodeBlock is fenced or indented code. Lang is the sanitized info-string language, possibly empty.
	CodeBlock struct{ Lang, Text string }
	// Quote is a block quotation.
	Quote struct{ Blocks []Block }
	// List is an ordered or unordered list. Start is the first number of an ordered list.
	List struct {
		Ordered bool
		Start   int
		Items   []ListItem
	}
	// Table is a GFM table; every row has len(Align) cells.
	Table struct {
		Align  []Align
		Header []Cell
		Rows   [][]Cell
	}
	// Rule is a thematic break.
	Rule struct{}
	// Details is a <details> element: a summary that is always shown and content that is folded.
	Details struct {
		Summary []Inline
		Blocks  []Block
	}
)

// ListItem is one item of a List; Task items have a checkbox.
type ListItem struct {
	Task, Checked bool
	Blocks        []Block
}

// Cell is the inline content of one table cell.
type Cell []Inline

// Align is a table column's alignment.
type Align uint8

// Column alignments.
const (
	AlignNone Align = iota
	AlignLeft
	AlignCenter
	AlignRight
)

// Style is an inline text style.
type Style uint8

// Inline styles.
const (
	Bold Style = iota + 1
	Italic
	Strike
)

type (
	// Text is literal text.
	Text string
	// Break is a hard line break.
	Break struct{}
	// Styled is text with a style.
	Styled struct {
		Style    Style
		Children []Inline
	}
	// Code is an inline code span.
	Code string
	// Link is a link whose URL passed the allowlist ([AllowedURL]).
	Link struct {
		URL      string
		Children []Inline
	}
	// Attachment is an image that references a SpaceMan attachment (spaceman://attachment/<id>).
	Attachment struct{ ID, Alt string }
	// FootnoteRef is a reference to Document.Footnotes[N-1].
	FootnoteRef struct{ N int }
)

func (Paragraph) block() {}
func (Heading) block()   {}
func (CodeBlock) block() {}
func (Quote) block()     {}
func (List) block()      {}
func (Table) block()     {}
func (Rule) block()      {}
func (Details) block()   {}

func (Text) inline()        {}
func (Break) inline()       {}
func (Styled) inline()      {}
func (Code) inline()        {}
func (Link) inline()        {}
func (Attachment) inline()  {}
func (FootnoteRef) inline() {}

// IsDiagram reports whether the code block is a Mermaid diagram.
func (c CodeBlock) IsDiagram() bool { return c.Lang == "mermaid" }
