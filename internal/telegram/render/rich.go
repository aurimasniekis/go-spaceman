package render

import (
	"errors"
	"unicode/utf8"
)

// ErrRichLimits reports that a body exceeds the rich-message limits (Limits.RichChars, RichBlocks, RichDepth); send
// the entity parts instead.
var ErrRichLimits = errors.New("render: body exceeds the rich-message limits")

// ErrEmpty reports that a body renders to nothing.
var ErrEmpty = errors.New("render: empty body")

// RichMessage is an InputRichMessage built from blocks (https://core.telegram.org/bots/api#inputrichmessage).
type RichMessage struct {
	Blocks []RichBlock `json:"blocks"`
	// Truncated reports that the source was longer than Limits.Source; the blocks end with a "continued in the app"
	// paragraph.
	Truncated bool `json:"-"`
}

// RichBlock is one InputRichBlock.
type RichBlock interface{ richBlock() }

// RichText is a Telegram RichText: a string, a []RichText, or a *RichSpan.
type RichText any

// RichSpan is a formatted RichText (RichTextBold, RichTextItalic, RichTextStrikethrough, RichTextCode, RichTextUrl).
type RichSpan struct {
	Type string   `json:"type"`
	Text RichText `json:"text"`
	URL  string   `json:"url,omitempty"`
}

// Rich block types (https://core.telegram.org/bots/api#inputrichblock).
type (
	// RichParagraph is InputRichBlockParagraph.
	RichParagraph struct {
		Type string   `json:"type"`
		Text RichText `json:"text"`
	}
	// RichHeading is InputRichBlockSectionHeading; Size is 1-6.
	RichHeading struct {
		Type string   `json:"type"`
		Text RichText `json:"text"`
		Size int      `json:"size"`
	}
	// RichPre is InputRichBlockPreformatted.
	RichPre struct {
		Type     string   `json:"type"`
		Text     RichText `json:"text"`
		Language string   `json:"language,omitempty"`
	}
	// RichDivider is InputRichBlockDivider.
	RichDivider struct {
		Type string `json:"type"`
	}
	// RichList is InputRichBlockList.
	RichList struct {
		Type  string         `json:"type"`
		Items []RichListItem `json:"items"`
	}
	// RichBlockquote is InputRichBlockBlockQuotation.
	RichBlockquote struct {
		Type   string      `json:"type"`
		Blocks []RichBlock `json:"blocks"`
	}
	// RichTable is InputRichBlockTable.
	RichTable struct {
		Type       string            `json:"type"`
		Cells      [][]RichTableCell `json:"cells"`
		IsBordered bool              `json:"is_bordered,omitempty"`
	}
	// RichDetails is InputRichBlockDetails.
	RichDetails struct {
		Type    string      `json:"type"`
		Summary RichText    `json:"summary"`
		Blocks  []RichBlock `json:"blocks"`
	}
	// RichButtons is InputRichBlockButtons: 1-8 buttons in one row.
	RichButtons struct {
		Type    string       `json:"type"`
		Buttons []RichButton `json:"buttons"`
	}
)

// RichListItem is InputRichBlockListItem. Ordered items carry Value and LabelType "1".
type RichListItem struct {
	Blocks      []RichBlock `json:"blocks"`
	HasCheckbox bool        `json:"has_checkbox,omitempty"`
	IsChecked   bool        `json:"is_checked,omitempty"`
	Value       int         `json:"value,omitempty"`
	LabelType   string      `json:"type,omitempty"`
}

// RichTableCell is RichBlockTableCell; align and valign are required by the API.
type RichTableCell struct {
	Text     RichText `json:"text"`
	IsHeader bool     `json:"is_header,omitempty"`
	Align    string   `json:"align"`
	Valign   string   `json:"valign"`
}

// RichButton is a RichMessageButton that opens the Mini App.
type RichButton struct {
	Text   string      `json:"text"`
	WebApp *WebAppInfo `json:"web_app,omitempty"`
}

// WebAppInfo is https://core.telegram.org/bots/api#webappinfo.
type WebAppInfo struct {
	URL string `json:"url"`
}

func (RichParagraph) richBlock()  {}
func (RichHeading) richBlock()    {}
func (RichPre) richBlock()        {}
func (RichDivider) richBlock()    {}
func (RichList) richBlock()       {}
func (RichBlockquote) richBlock() {}
func (RichTable) richBlock()      {}
func (RichDetails) richBlock()    {}
func (RichButtons) richBlock()    {}

// Rich renders the document as a rich message. It returns ErrRichLimits when the result would exceed Telegram's
// limits (characters, blocks, nesting) and ErrEmpty when there is nothing to send; the caller then falls back to
// [Document.Entities].
func (d *Document) Rich(o Options) (RichMessage, error) {
	r := &richRenderer{o: o}
	blocks := r.blocks(d.Blocks, 1)
	if len(d.Footnotes) > 0 {
		blocks = append(blocks, r.count(RichDivider{Type: "divider"}, 1))
		l := RichList{Type: "list"}
		for i, fn := range d.Footnotes {
			r.nblocks++
			l.Items = append(l.Items, RichListItem{Blocks: r.blocks(fn, 3), Value: i + 1, LabelType: "1"})
		}
		blocks = append(blocks, r.count(l, 1))
	}
	if d.Truncated {
		blocks = append(blocks, r.count(RichParagraph{Type: "paragraph", Text: r.str(continuedInApp)}, 1))
		if o.ContinueURL != "" {
			blocks = append(blocks, r.buttons([]RichButton{{Text: openInApp, WebApp: &WebAppInfo{URL: o.ContinueURL}}}, 1))
		}
	}
	if r.over() {
		return RichMessage{}, ErrRichLimits
	}
	if len(blocks) == 0 {
		return RichMessage{}, ErrEmpty
	}
	return RichMessage{Blocks: blocks, Truncated: d.Truncated}, nil
}

type richRenderer struct {
	o                       Options
	chars, nblocks, maxDeep int
	ndiag                   int
}

func (r *richRenderer) over() bool {
	return r.chars > Limits.RichChars || r.nblocks > Limits.RichBlocks || r.maxDeep > Limits.RichDepth
}

// count records one block at nesting level depth.
func (r *richRenderer) count(b RichBlock, depth int) RichBlock {
	r.nblocks++
	r.maxDeep = max(r.maxDeep, depth)
	return b
}

func (r *richRenderer) str(s string) RichText {
	r.chars += utf8.RuneCountInString(s)
	return s
}

func (r *richRenderer) buttons(bs []RichButton, depth int) RichBlock {
	for _, b := range bs {
		r.chars += utf8.RuneCountInString(b.Text)
	}
	return r.count(RichButtons{Type: "buttons", Buttons: bs}, depth)
}

func (r *richRenderer) blocks(bs []Block, depth int) []RichBlock {
	var out []RichBlock
	for _, b := range bs {
		out = append(out, r.block(b, depth)...)
		if r.over() {
			// Stop early: the result is refused anyway, and a hostile body shouldn't cost more work.
			return out
		}
	}
	return out
}

func (r *richRenderer) block(b Block, depth int) []RichBlock {
	switch b := b.(type) {
	case Paragraph:
		return []RichBlock{r.count(RichParagraph{Type: "paragraph", Text: r.inlines(b.Inlines, depth)}, depth)}
	case Heading:
		return []RichBlock{r.count(RichHeading{Type: "heading", Text: r.inlines(b.Inlines, depth), Size: b.Level}, depth)}
	case CodeBlock:
		out := []RichBlock{r.count(RichPre{Type: "pre", Text: r.str(b.Text), Language: b.Lang}, depth)}
		if b.IsDiagram() {
			n := r.ndiag
			r.ndiag++
			if r.o.DiagramURL != nil && n < Limits.Diagrams {
				out = append(out, r.buttons([]RichButton{{Text: viewDiagram, WebApp: &WebAppInfo{URL: r.o.DiagramURL(n)}}}, depth))
			}
		}
		return out
	case Rule:
		return []RichBlock{r.count(RichDivider{Type: "divider"}, depth)}
	case Quote:
		return []RichBlock{r.count(RichBlockquote{Type: "blockquote", Blocks: r.blocks(b.Blocks, depth+1)}, depth)}
	case Details:
		summary := r.inlines(b.Summary, depth)
		if summary == "" {
			summary = r.str("Details")
		}
		return []RichBlock{r.count(RichDetails{Type: "details", Summary: summary, Blocks: r.blocks(b.Blocks, depth+1)}, depth)}
	case List:
		l := RichList{Type: "list"}
		for i, it := range b.Items {
			// A list item is a block of its own, one level below the list.
			r.nblocks++
			r.maxDeep = max(r.maxDeep, depth+1)
			item := RichListItem{Blocks: r.blocks(it.Blocks, depth+2), HasCheckbox: it.Task, IsChecked: it.Checked}
			if b.Ordered {
				item.Value, item.LabelType = b.Start+i, "1"
			}
			l.Items = append(l.Items, item)
		}
		return []RichBlock{r.count(l, depth)}
	case Table:
		if len(b.Align) > Limits.RichColumns {
			if text, ok := tableText(b); ok {
				return []RichBlock{r.count(RichPre{Type: "pre", Text: r.str(text)}, depth)}
			}
			return []RichBlock{r.count(RichParagraph{Type: "paragraph", Text: r.str(tableInApp)}, depth)}
		}
		t := RichTable{Type: "table", IsBordered: true}
		rows := append([][]Cell{b.Header}, b.Rows...)
		for i, row := range rows {
			r.nblocks++
			var cells []RichTableCell
			for j, cell := range row {
				cells = append(cells, RichTableCell{
					Text: r.inlines(cell, depth+1), IsHeader: i == 0, Align: alignName(b.Align[j]), Valign: "top",
				})
			}
			t.Cells = append(t.Cells, cells)
		}
		return []RichBlock{r.count(t, depth)}
	}
	return nil
}

func alignName(a Align) string {
	switch a {
	case AlignCenter:
		return "center"
	case AlignRight:
		return "right"
	}
	return "left"
}

// inlines converts inline content at block nesting level depth; formatting adds levels.
func (r *richRenderer) inlines(in []Inline, depth int) RichText {
	var out []RichText
	for _, n := range in {
		switch n := n.(type) {
		case Text:
			out = append(out, r.str(string(n)))
		case Break:
			out = append(out, r.str("\n"))
		case Code:
			r.maxDeep = max(r.maxDeep, depth+1)
			out = append(out, &RichSpan{Type: "code", Text: r.str(string(n))})
		case Styled:
			r.maxDeep = max(r.maxDeep, depth+1)
			out = append(out, &RichSpan{Type: richStyle(n.Style), Text: r.inlines(n.Children, depth+1)})
		case Link:
			r.maxDeep = max(r.maxDeep, depth+1)
			out = append(out, &RichSpan{Type: "url", Text: r.inlines(n.Children, depth+1), URL: n.URL})
		case Attachment:
			alt := n.Alt
			if alt == "" {
				alt = "image"
			}
			out = append(out, r.str(imageMark+" "+alt))
		case *FootnoteRef:
			out = append(out, r.str(footnoteMark(n.N)))
		}
	}
	switch len(out) {
	case 0:
		return ""
	case 1:
		return out[0]
	}
	return out
}

func richStyle(s Style) string {
	switch s {
	case Bold:
		return "bold"
	case Strike:
		return "strikethrough"
	}
	return "italic"
}
