package render

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// hostileBodies are inputs that froze the web renderer (ADR-0082) or are known to be expensive for Markdown parsers.
// Each must render, within the time budget, to valid Telegram messages.
func hostileBodies() map[string]string {
	lines := func(n int, f func(i int) string) string {
		var b strings.Builder
		for i := range n {
			b.WriteString(f(i))
			b.WriteByte('\n')
		}
		return b.String()
	}
	return map[string]string{
		"nested lists by markers":   strings.Repeat("- ", 2000) + "x",
		"nested lists by indent":    lines(2000, func(i int) string { return strings.Repeat(" ", i*2) + "- x" }),
		"nested quotes":             strings.Repeat(">", 5000) + " x",
		"nested quotes per line":    lines(3000, func(i int) string { return strings.Repeat("> ", i%40) + "x" }),
		"unmatched emphasis":        strings.Repeat("*a ", 8000),
		"unmatched emphasis lines":  strings.Repeat("*a\n", 8000),
		"unmatched underscores":     strings.Repeat("_a", 60000),
		"unmatched strike":          strings.Repeat("~~a ", 30000),
		"nested emphasis":           strings.Repeat("**", 5000) + "x" + strings.Repeat("**", 5000),
		"unclosed links":            strings.Repeat("[a](", 30000),
		"unclosed links paragraphs": strings.Repeat(strings.Repeat("[a](", 250)+"\n\n", 120),
		"brackets":                  strings.Repeat("[", 50000) + strings.Repeat("]", 50000),
		"nested images":             strings.Repeat("![", 20000) + "x" + strings.Repeat("](u)", 20000),
		"backtick runs":             lines(400, func(i int) string { return strings.Repeat("`", i%50+1) + " a" }),
		"unclosed details":          strings.Repeat("<details><summary>s</summary>\n\n", 3000),
		"html comments":             strings.Repeat("<!-- a ", 20000),
		"html attributes":           "<a " + strings.Repeat(`b="c" `, 20000),
		"character references":      strings.Repeat("&amp;&#x1F600;&#0;&#xD800;&bogus;", 10000),
		"huge body":                 strings.Repeat("word ", 400000),
		"huge table columns":        "|" + strings.Repeat(" a |", 2000) + "\n|" + strings.Repeat("-|", 2000) + "\n" + strings.Repeat("|"+strings.Repeat(" b |", 2000)+"\n", 20),
		"huge table rows":           "| a | b |\n|-|-|\n" + strings.Repeat("| x | y |\n", 20000),
		"many footnotes":            lines(4000, func(i int) string { return fmt.Sprintf("x[^%d]\n\n[^%d]: n", i, i) }),
		"many fences":               strings.Repeat("```\n", 30000),
		"many diagrams":             strings.Repeat("```mermaid\ngraph TD\n```\n", 5000),
		"many links":                strings.Repeat("[javascript](javascript:alert(1)) [tg](tg://x) [ok](https://example.com) ", 3000),
		"many attachments":          strings.Repeat("![](spaceman://attachment/att_0123456789abcdefghijklmnop)", 5000),
		"surrogates and controls":   strings.Repeat("😀\x00\x1b\u2028\xff\xfe🇱🇹e\u0301", 20000),
		"long line":                 strings.Repeat("x", 500000),
		"setext headings":           strings.Repeat("a\n=\n", 30000),
		"reference definitions":     strings.Repeat("[a]: <", 20000),
	}
}

func TestHostileInputs(t *testing.T) {
	budget := 2 * time.Second
	if raceEnabled {
		budget = 20 * time.Second
	}
	o := Options{DiagramURL: func(int) string { return "https://tg.example.com/d" }, ContinueURL: "https://tg.example.com/i"}
	for name, body := range hostileBodies() {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			doc := Parse(body)
			m := doc.Entities(o)
			rich, err := doc.Rich(o)
			if took := time.Since(start); took > budget {
				t.Fatalf("took %v (budget %v)", took, budget)
			}
			if err := validateMessage(m, o); err != nil {
				t.Fatal(err)
			}
			if err == nil {
				checkRich(t, rich)
			}
			for _, p := range m.Parts {
				Markdown(p.Text, p.Entities)
			}
		})
	}
}

// checkRich checks a rich message against the limits that Rich promises to honour.
func checkRich(t *testing.T, rich RichMessage) {
	t.Helper()
	j, err := json.Marshal(rich)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(j, &v); err != nil {
		t.Fatal(err)
	}
	blocks, depth, buttons := 0, 0, 0
	var walk func(v any, d int)
	walk = func(v any, d int) {
		depth = max(depth, d)
		switch v := v.(type) {
		case map[string]any:
			if _, ok := v["type"].(string); ok {
				blocks++
			}
			if b, ok := v["buttons"].([]any); ok {
				buttons = max(buttons, len(b))
			}
			if c, ok := v["cells"].([]any); ok && len(c) > 0 {
				if row, ok := c[0].([]any); ok && len(row) > Limits.RichColumns {
					t.Fatalf("%d table columns", len(row))
				}
			}
			for _, x := range v {
				walk(x, d+1)
			}
		case []any:
			for _, x := range v {
				walk(x, d)
			}
		}
	}
	walk(v, 0)
	// The JSON walk counts spans as blocks too, so it bounds from above what Telegram counts.
	if buttons > Limits.RichButtons {
		t.Fatalf("%d buttons in a block", buttons)
	}
	if n := len([]rune(string(j))); n > 4*Limits.RichChars {
		t.Fatalf("rich message of %d characters", n)
	}
}

func TestHostileMarkdownInbound(t *testing.T) {
	text := strings.Repeat("a😀\n", 30000)
	var ents []Entity
	for i := range 5000 {
		ents = append(ents, Entity{Type: []string{"bold", "italic", "code", "pre", "blockquote", "text_link", "url"}[i%7], Offset: i * 7 % 90000, Length: i%13 + 1, URL: "https://example.com"})
	}
	start := time.Now()
	md := Markdown(text, ents)
	if took := time.Since(start); took > 2*time.Second && !raceEnabled {
		t.Fatalf("took %v", took)
	}
	if md == "" {
		t.Fatal("empty")
	}
	Parse(md).Entities(Options{})
}
