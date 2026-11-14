package render

import (
	"encoding/json"
	"errors"
	"testing"
	"unicode/utf8"
)

// FuzzRender: any body renders without a panic to entity parts that satisfy Telegram's rules, and to a rich message
// within the limits or a limits error.
func FuzzRender(f *testing.F) {
	for _, s := range []string{
		"", "# h\n\n**b** *i* ~~s~~ `c` [l](https://x.com)", "> q\n> ```\n> c\n> ```", "- [x] a\n  1. b",
		"| a | b |\n|-|-:|\n| 1 | 2 |", "```mermaid\ngraph TD\n```", "x[^1]\n\n[^1]: n", "<details><summary>s</summary>\n\nb\n\n</details>",
		"[js](javascript:alert(1)) [tg](tg://user?id=1) ![a](spaceman://attachment/att_0123456789abcdefghijklmnop)",
		"😀\xff\x00e\u0301", "**a `b` c**", "***", "[a](<b c>)", "&#xD800; &#0;",
	} {
		f.Add(s, uint16(0))
	}
	f.Fuzz(func(t *testing.T, body string, limit uint16) {
		o := Options{Limit: int(limit % 5000), DiagramURL: func(int) string { return "https://tg.example.com/d" }, ContinueURL: "https://tg.example.com/i"}
		doc := Parse(body)
		m := doc.Entities(o)
		if err := validateMessage(m, o); err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		rich, err := doc.Rich(o)
		if err != nil && !errors.Is(err, ErrRichLimits) && !errors.Is(err, ErrEmpty) {
			t.Fatal(err)
		}
		if err == nil {
			if _, err := json.Marshal(rich); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// FuzzMarkdown: any text with any entities converts without a panic to valid UTF-8 Markdown that parses again.
func FuzzMarkdown(f *testing.F) {
	f.Add("hello 😀 world", "bold", 0, 5, "italic", 3, 8)
	f.Add("a\nb", "blockquote", 0, 3, "pre", 2, 1)
	f.Add("x", "text_link", -1, 1<<40, "code", 0, 1)
	f.Add("😀", "bold", 1, 1, "url", 0, 2)
	f.Fuzz(func(t *testing.T, text, t1 string, o1, l1 int, t2 string, o2, l2 int) {
		ents := []Entity{{Type: t1, Offset: o1, Length: l1, URL: "https://example.com"}, {Type: t2, Offset: o2, Length: l2, Language: t1}}
		md := Markdown(text, ents)
		if !utf8.ValidString(md) {
			t.Fatalf("invalid UTF-8 from %q", text)
		}
		m := Parse(md).Entities(Options{})
		if err := validateMessage(m, Options{}); err != nil {
			t.Fatal(err)
		}
	})
}
