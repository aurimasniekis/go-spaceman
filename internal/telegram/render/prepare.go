package render

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits bound the work a hostile body can cause and the size of what is sent. The parser caps are the web
// renderer's (ADR-0082, web/src/lib/markdown.ts MARKDOWN_LIMITS); the Telegram ones come from docs/contracts/telegram.md.
var Limits = struct {
	// Source is the number of bytes of a body that are parsed; the rest is dropped and the result is marked truncated.
	Source int
	// Indent is the number of columns of leading indentation kept outside fenced code.
	Indent int
	// Markers is the number of stacked block markers (>, -, 1.) kept at the start of a line.
	Markers int
	// Delimiters is the number of emphasis delimiters (*, _, ~) in one paragraph beyond which they are escaped.
	Delimiters int
	// Brackets is the number of link brackets ([) in one paragraph beyond which brackets are escaped. goldmark is
	// quadratic in unclosed links: 30,000 "[a](" took 10 s.
	Brackets int
	// Depth is the nesting depth of block containers (lists, quotes, details) kept in the AST; deeper content is
	// lifted into the deepest kept container.
	Depth int
	// InlineDepth is the nesting depth of inline styles kept in the AST; deeper styles are dropped (their text stays).
	InlineDepth int
	// Diagrams is the number of Mermaid diagrams per body that get a "View diagram" button.
	Diagrams int
	// Attachments is the number of distinct attachment references collected per body.
	Attachments int
	// TableWidth is the widest table, in characters, shown as monospace text in entity messages.
	TableWidth int
	// Text is Telegram's message text limit, after entity parsing.
	Text int
	// Caption is Telegram's media caption limit.
	Caption int
	// Parts is the number of messages one body is split into at most.
	Parts int
	// RichChars, RichBlocks, RichDepth and RichColumns are the rich-message limits.
	RichChars, RichBlocks, RichDepth, RichColumns int
	// RichButtons is the number of buttons in one rich buttons block.
	RichButtons int
}{
	Source:      128 << 10,
	Indent:      64,
	Markers:     16,
	Delimiters:  256,
	Brackets:    256,
	Depth:       8,
	InlineDepth: 6,
	Diagrams:    20,
	Attachments: 50,
	TableWidth:  60,
	Text:        4096,
	Caption:     1024,
	Parts:       4,
	RichChars:   32768,
	RichBlocks:  500,
	RichDepth:   16,
	RichColumns: 20,
	RichButtons: 8,
}

var (
	fenceRE = regexp.MustCompile("^[ \t]*(`{3,}|~{3,})")
	// marker is one leading block marker: a quote marker, or a list marker followed by whitespace.
	marker        = `[ \t]*(?:>[ \t]?|(?:[-*+]|[0-9]{1,9}[.)])[ \t]+)`
	excessMarkers = regexp.MustCompile(`^((?:` + marker + `){` + strconv.Itoa(Limits.Markers) + `})(?:` + marker + `)+`)
	// delimiterRE matches what escapeDelimiters leaves alone (an escape, a code span) or a delimiter to escape.
	delimiterRE = regexp.MustCompile("\\\\[\\s\\S]|(`+)|[*_~]")
	bracketRE   = regexp.MustCompile("\\\\[\\s\\S]|(`+)|[\\[\\]]")
)

// sanitize makes the body valid UTF-8 without control characters other than tab and newline, with \n line ends, and
// cut at Limits.Source bytes (on a line end where possible). It reports whether it cut anything.
func sanitize(md string) (string, bool) {
	cut := false
	if len(md) > Limits.Source {
		end := Limits.Source
		for end > 0 && !utf8.RuneStart(md[end]) {
			end--
		}
		md = md[:end]
		if i := strings.LastIndexByte(md, '\n'); i > len(md)/2 {
			md = md[:i]
		}
		cut = true
	}
	md = strings.ToValidUTF8(md, "�")
	md = strings.ReplaceAll(md, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r':
			return '\n'
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		case r == '\u2028' || r == '\u2029':
			return '\n'
		}
		return r
	}, md), cut
}

// limitNesting bounds the parser's work outside fenced code, as the web renderer does: leading indentation is capped at
// Limits.Indent columns, stacked block markers at Limits.Markers per line, and a paragraph with more than
// Limits.Delimiters emphasis delimiters has them escaped so they show literally.
func limitNesting(md string) string {
	var out, block []string
	flush := func() {
		if len(block) == 0 {
			return
		}
		text := strings.Join(block, "\n")
		if strings.Count(text, "*")+strings.Count(text, "_")+strings.Count(text, "~") > Limits.Delimiters {
			text = escapeChars(text, delimiterRE)
		}
		if strings.Count(text, "[") > Limits.Brackets {
			text = escapeChars(text, bracketRE)
		}
		out = append(out, text)
		block = block[:0]
	}
	fence := ""
	for line := range strings.SplitSeq(md, "\n") {
		f := fenceRE.FindStringSubmatch(line)
		if fence != "" {
			if f != nil && f[1][0] == fence[0] && len(f[1]) >= len(fence) && strings.TrimSpace(line) == f[1] {
				fence = ""
			}
			out = append(out, line)
			continue
		}
		if f != nil {
			flush()
			fence = f[1]
			out = append(out, line)
			continue
		}
		if strings.TrimSpace(line) == "" {
			flush()
			out = append(out, line)
			continue
		}
		ws := len(line) - len(strings.TrimLeft(line, " \t"))
		cols := 0
		for _, c := range line[:ws] {
			if c == '\t' {
				cols += 4 - cols%4
			} else {
				cols++
			}
		}
		if cols > Limits.Indent {
			line = strings.Repeat(" ", Limits.Indent) + line[ws:]
		}
		block = append(block, excessMarkers.ReplaceAllString(line, "$1"))
	}
	flush()
	return strings.Join(out, "\n")
}

// escapeChars escapes the single characters re matches in a paragraph (re's other alternatives match an escape or a
// backtick run), leaving code spans and existing escapes alone.
func escapeChars(block string, re *regexp.Regexp) string {
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringSubmatchIndex(block, -1) {
		if m[0] < last {
			continue
		}
		b.WriteString(block[last:m[0]])
		tok := block[m[0]:m[1]]
		switch {
		case m[2] >= 0:
			// A backtick run opens a code span only when a run of the same length closes it.
			if end := closingRun(block[m[1]:], len(tok)); end >= 0 {
				b.WriteString(block[m[0] : m[1]+end])
				last = m[1] + end
				continue
			}
			b.WriteString(tok)
		case len(tok) == 1:
			b.WriteByte('\\')
			b.WriteString(tok)
		default:
			b.WriteString(tok)
		}
		last = m[1]
	}
	b.WriteString(block[last:])
	return b.String()
}

// closingRun returns the end offset in s of the first backtick run exactly n long, or -1.
func closingRun(s string, n int) int {
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		if j-i == n {
			return j
		}
		i = j
	}
	return -1
}
