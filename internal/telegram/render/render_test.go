package render

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf16"
)

func render(t *testing.T, md string, o Options) Message {
	t.Helper()
	m := Parse(md).Entities(o)
	if err := validateMessage(m, o); err != nil {
		t.Fatalf("%q: %v", md, err)
	}
	return m
}

func one(t *testing.T, md string) Part {
	t.Helper()
	m := render(t, md, Options{})
	if len(m.Parts) != 1 {
		t.Fatalf("%q: %d parts", md, len(m.Parts))
	}
	return m.Parts[0]
}

func TestAllowedURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://example.com/a":        "https://example.com/a",
		"HTTP://Example.com":           "http://Example.com",
		"mailto:a@example.com":         "mailto:a@example.com",
		"www.example.com/x":            "http://www.example.com/x",
		" https://example.com ":        "https://example.com",
		"javascript:alert(1)":          "",
		"JavaScript:alert(1)":          "",
		"java\tscript:alert(1)":        "",
		"data:text/html,<b>x</b>":      "",
		"vbscript:msgbox":              "",
		"tg://user?id=1":               "",
		"tg://resolve?domain=x":        "",
		"file:///etc/passwd":           "",
		"/relative/path":               "",
		"//example.com/proto-relative": "",
		"example.com":                  "",
		"https://":                     "",
		"https://user:pw@example.com/": "",
		"mailto:nobody":                "",
		"https://exa mple.com":         "",
		"https://example.com/\x00":     "",
		"https://example.com/" + strings.Repeat("a", maxURL): "",
	} {
		got, ok := AllowedURL(raw)
		if ok != (want != "") || got != want {
			t.Errorf("AllowedURL(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}

func TestLinksOutsideTheAllowlistBecomeText(t *testing.T) {
	p := one(t, "[a](javascript:alert(1)) [b](tg://user?id=1) [c](data:x) ![d](javascript:x) <javascript:x>")
	if len(p.Entities) != 0 {
		t.Fatalf("entities for refused links: %+v", p.Entities)
	}
	if p.Text != "a b c d <javascript:x>" && p.Text != "a b c d javascript:x" {
		t.Fatalf("text %q", p.Text)
	}
	rich, err := Parse("[a](javascript:alert(1)) [ok](https://example.com)").Rich(Options{})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := json.Marshal(rich)
	if strings.Contains(string(j), "javascript") || !strings.Contains(string(j), `"url":"https://example.com"`) {
		t.Fatalf("rich %s", j)
	}
}

func TestStylesNeverContainCode(t *testing.T) {
	p := one(t, "**a `b` c** [x `y` z](https://example.com)")
	for _, e := range p.Entities {
		if e.Type == EntityCode && entityText(p, e) != "b" {
			t.Fatalf("code entity %q", entityText(p, e))
		}
	}
	if !hasEntity(p, EntityCode, "b") || !hasEntity(p, EntityBold, "a") || !hasEntity(p, EntityBold, "c") ||
		!hasEntity(p, EntityTextLink, "x y z") {
		t.Fatalf("entities %+v", p.Entities)
	}
}

func hasEntity(p Part, typ, covers string) bool {
	for _, e := range p.Entities {
		if e.Type == typ && entityText(p, e) == covers {
			return true
		}
	}
	return false
}

func TestHeadingsListsTasksAndRules(t *testing.T) {
	p := one(t, "# Head\n\n- a\n- [x] b\n- [ ] c\n\n5. five\n6. six\n\n***")
	want := "Head\n\n• a\n☑ b\n☐ c\n\n5. five\n6. six\n\n" + ruleText
	if p.Text != want {
		t.Fatalf("text %q, want %q", p.Text, want)
	}
	if !hasEntity(p, EntityBold, "Head") {
		t.Fatalf("heading not bold: %+v", p.Entities)
	}
}

func TestQuotes(t *testing.T) {
	p := one(t, "> a\n>\n> > nested\n\nafter")
	if len(p.Entities) != 1 || p.Entities[0].Type != EntityBlockquote || entityText(p, p.Entities[0]) != "a\nnested" {
		t.Fatalf("entities %+v in %q", p.Entities, p.Text)
	}
	long := one(t, "> "+strings.Repeat("word ", 100))
	if long.Entities[0].Type != EntityExpandable {
		t.Fatalf("long quote is %s", long.Entities[0].Type)
	}
	// A code block in a quote leaves the quote: pre can't be inside another entity.
	p = one(t, "> before\n> ```\n> code\n> ```\n> after")
	if !hasEntity(p, EntityBlockquote, "before") || !hasEntity(p, EntityPre, "code") || !hasEntity(p, EntityBlockquote, "after") {
		t.Fatalf("entities %+v in %q", p.Entities, p.Text)
	}
}

func TestTables(t *testing.T) {
	p := one(t, "| a | b |\n|---|--:|\n| x | 10 |")
	if p.Text != "| a |  b |\n|---|----|\n| x | 10 |" || len(p.Entities) != 1 || p.Entities[0].Type != EntityPre {
		t.Fatalf("narrow table %q %+v", p.Text, p.Entities)
	}
	wide := "|" + strings.Repeat(" column |", 8) + "\n|" + strings.Repeat("---|", 8) + "\n|" + strings.Repeat(" value |", 8)
	if p := one(t, wide); p.Text != tableInApp {
		t.Fatalf("wide table %q", p.Text)
	}
	rich, err := Parse(wide).Rich(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if tb, ok := rich.Blocks[0].(RichTable); !ok || len(tb.Cells) != 2 || len(tb.Cells[0]) != 8 || !tb.Cells[0][0].IsHeader {
		t.Fatalf("rich table %+v", rich.Blocks[0])
	}
	cols := "|" + strings.Repeat("c|", 21) + "\n|" + strings.Repeat("-|", 21) + "\n"
	rich, err = Parse(cols).Rich(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rich.Blocks[0].(RichTable); ok {
		t.Fatalf("a %d-column table must not be a rich table", 21)
	}
}

func TestDiagramsGetButtons(t *testing.T) {
	md := "```mermaid\ngraph TD\nA-->B\n```\n\n```go\nx\n```"
	o := Options{DiagramURL: func(n int) string { return "https://tg.example.com/d" }}
	p := render(t, md, o).Parts[0]
	if len(p.Buttons) != 1 || p.Buttons[0] != (Button{Text: viewDiagram, WebAppURL: "https://tg.example.com/d"}) {
		t.Fatalf("buttons %+v", p.Buttons)
	}
	if !hasEntity(p, EntityPre, "graph TD\nA-->B") || p.Entities[0].Language != "mermaid" {
		t.Fatalf("entities %+v", p.Entities)
	}
	if p := render(t, md, Options{}).Parts[0]; len(p.Buttons) != 0 {
		t.Fatalf("buttons without a DiagramURL: %+v", p.Buttons)
	}
	// At most Limits.Diagrams buttons per body.
	many := strings.Repeat("```mermaid\ngraph TD\n```\n\n", Limits.Diagrams+5)
	n := 0
	for _, p := range render(t, many, o).Parts {
		n += len(p.Buttons)
	}
	if n != Limits.Diagrams {
		t.Fatalf("%d diagram buttons", n)
	}
	rich, err := Parse(md).Rich(o)
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := rich.Blocks[1].(RichButtons); !ok || b.Buttons[0].WebApp == nil || b.Buttons[0].Text != viewDiagram {
		t.Fatalf("rich blocks %+v", rich.Blocks)
	}
}

func TestCodeLanguage(t *testing.T) {
	for info, want := range map[string]string{
		"go": "go", "Go": "go", "c++": "c++", "c#": "c#", "{python}": "python", "sh title=x": "sh",
		"": "", "<script>": "", "a\"b": "", strings.Repeat("x", 40): "",
	} {
		if got := codeLanguage(info); got != want {
			t.Errorf("codeLanguage(%q) = %q, want %q", info, got, want)
		}
	}
}

func TestAttachmentsAreCollected(t *testing.T) {
	id := "att_" + strings.Repeat("a", 26)
	doc := Parse("![x](spaceman://attachment/" + id + ") ![y](spaceman://attachment/" + id + ")")
	if len(doc.Attachments) != 1 || doc.Attachments[0] != id {
		t.Fatalf("attachments %v", doc.Attachments)
	}
	var b strings.Builder
	for i := range Limits.Attachments + 10 {
		b.WriteString("![](spaceman://attachment/att_" + strings.Repeat("b", 24) + string(rune('a'+i/26%26)) + string(rune('a'+i%26)) + ") ")
	}
	if n := len(Parse(b.String()).Attachments); n != Limits.Attachments {
		t.Fatalf("%d attachments collected", n)
	}
}

func TestEmptyBody(t *testing.T) {
	for _, md := range []string{"", "   ", "\n\n", "<!-- only a comment -->", "[ref]: https://example.com"} {
		if m := Parse(md).Entities(Options{}); len(m.Parts) != 0 {
			t.Errorf("%q rendered %+v", md, m.Parts)
		}
		if _, err := Parse(md).Rich(Options{}); !errors.Is(err, ErrEmpty) {
			t.Errorf("%q: rich error %v", md, err)
		}
	}
}

func TestRichLimits(t *testing.T) {
	if _, err := Parse(strings.Repeat("a", Limits.RichChars+1)).Rich(Options{}); !errors.Is(err, ErrRichLimits) {
		t.Fatalf("chars: %v", err)
	}
	if _, err := Parse(strings.Repeat("p\n\n", Limits.RichBlocks+1)).Rich(Options{}); !errors.Is(err, ErrRichLimits) {
		t.Fatalf("blocks: %v", err)
	}
	if _, err := Parse(strings.Repeat("p\n\n", Limits.RichBlocks)).Rich(Options{}); err != nil {
		t.Fatalf("exactly %d blocks: %v", Limits.RichBlocks, err)
	}
	// Nesting: list levels count twice (list, item), inline styles once each.
	deep := ""
	for i := range 8 {
		deep += strings.Repeat("  ", i) + "- x\n"
	}
	if _, err := Parse(deep).Rich(Options{}); !errors.Is(err, ErrRichLimits) {
		t.Fatalf("depth: %v", err)
	}
	if _, err := Parse("- a\n  - b\n    - c").Rich(Options{}); err != nil {
		t.Fatalf("shallow list: %v", err)
	}
}

// TestUTF16Offsets is the property test of plan §4.3: for random text mixing ASCII, emoji (surrogate pairs), CJK and
// combining characters, every entity's offset and length, read as UTF-16 code units of the part text, cover exactly
// the text that was styled.
func TestUTF16Offsets(t *testing.T) {
	alphabet := []string{"a", "b", " ", "é", "e\u0301", "漢", "字", "😀", "👍🏽", "🇱🇹", "👩\u200d💻", "𝔘", "\u200b", "ñ"}
	rng := rand.New(rand.NewPCG(1, 2))
	word := func() string {
		var b strings.Builder
		for range 1 + rng.IntN(6) {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		return strings.TrimSpace(b.String()) + "x"
	}
	for range 500 {
		var md strings.Builder
		var styled []string
		for range 1 + rng.IntN(12) {
			w := word()
			switch rng.IntN(4) {
			case 0:
				md.WriteString("**" + w + "**")
				styled = append(styled, w)
			case 1:
				md.WriteString("`" + w + "`")
				styled = append(styled, w)
			case 2:
				md.WriteString("[" + w + "](https://example.com)")
				styled = append(styled, w)
			default:
				md.WriteString(w)
			}
			md.WriteString(" ")
		}
		p := one(t, md.String())
		if len(p.Entities) != len(styled) {
			t.Fatalf("%q: %d entities, %d styled", md.String(), len(p.Entities), len(styled))
		}
		u := utf16.Encode([]rune(p.Text))
		for i, e := range p.Entities {
			prefix := string(utf16.Decode(u[:e.Offset]))
			if got := len(utf16.Encode([]rune(prefix))); got != e.Offset {
				t.Fatalf("offset %d != %d", e.Offset, got)
			}
			if got := entityText(p, e); got != styled[i] {
				t.Fatalf("%q: entity %d covers %q, want %q", md.String(), i, got, styled[i])
			}
			if !strings.HasPrefix(p.Text[len(prefix):], styled[i]) {
				t.Fatalf("entity %d doesn't start at its text", i)
			}
		}
	}
}

func TestSanitize(t *testing.T) {
	got, cut := sanitize("a\r\nb\rc\x00d\x1be\u2028f\x7fg\xffh\u0085i")
	if cut || got != "a\nb\ncde\nfg�hi" {
		t.Fatalf("sanitize = %q, %v", got, cut)
	}
	// A cut never splits a character and prefers a line end.
	big := strings.Repeat("😀", Limits.Source/4) + "tail"
	got, cut = sanitize(big)
	if !cut || len(got) > Limits.Source || !strings.HasSuffix(got, "😀") {
		t.Fatalf("cut %v, %d bytes, suffix %q", cut, len(got), got[len(got)-8:])
	}
	if d := Parse(big); !d.Truncated {
		t.Fatal("document not marked truncated")
	}
}

func TestLimitNesting(t *testing.T) {
	deep := strings.Repeat("> ", 40) + "x"
	got := limitNesting(deep)
	if strings.Count(got, ">") != Limits.Markers {
		t.Fatalf("markers kept: %d", strings.Count(got, ">"))
	}
	indented := strings.Repeat(" ", 200) + "- x"
	if got := limitNesting(indented); len(got)-len(strings.TrimLeft(got, " ")) != Limits.Indent {
		t.Fatalf("indent kept: %q", got[:80])
	}
	// Fenced code is left alone, however it looks.
	fenced := "```\n" + deep + "\n" + strings.Repeat("*", 1000) + "\n```"
	if got := limitNesting(fenced); got != fenced {
		t.Fatal("fenced code changed")
	}
	stars := strings.Repeat("*a ", Limits.Delimiters) + "`*code*` \\*"
	got = limitNesting(stars)
	if !strings.Contains(got, `\*a`) || !strings.Contains(got, "`*code*`") || !strings.Contains(got, `\*`) ||
		strings.Contains(got, `\\*`) {
		t.Fatalf("delimiters: %q", got[len(got)-40:])
	}
	if got := limitNesting(strings.Repeat("*a ", 10)); got != strings.Repeat("*a ", 10) {
		t.Fatal("few delimiters escaped")
	}
	brackets := strings.Repeat("[a](", Limits.Brackets+1)
	if got := limitNesting(brackets); !strings.HasPrefix(got, `\[a\](`) {
		t.Fatalf("brackets: %q", got[:20])
	}
}

func TestDepthCaps(t *testing.T) {
	doc := Parse(strings.Repeat("> ", Limits.Markers) + "deep")
	depth := 0
	for bs := doc.Blocks; len(bs) > 0; {
		q, ok := bs[0].(Quote)
		if !ok {
			break
		}
		depth++
		bs = q.Blocks
	}
	if depth != Limits.Depth {
		t.Fatalf("quote depth %d, want %d", depth, Limits.Depth)
	}
	p := one(t, strings.Repeat("> ", Limits.Markers)+"deep")
	if p.Text != "deep" {
		t.Fatalf("text %q", p.Text)
	}
	in := Parse(strings.Repeat("**", 20) + "x" + strings.Repeat("**", 20)).Blocks[0].(Paragraph).Inlines
	d := 0
	for len(in) == 1 {
		s, ok := in[0].(Styled)
		if !ok {
			break
		}
		d++
		in = s.Children
	}
	if d != Limits.InlineDepth {
		t.Fatalf("inline depth %d, want %d", d, Limits.InlineDepth)
	}
}
