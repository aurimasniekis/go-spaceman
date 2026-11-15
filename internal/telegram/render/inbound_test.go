package render

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func ent(typ string, off, n int) Entity { return Entity{Type: typ, Offset: off, Length: n} }

func TestMarkdownFromEntities(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		ents       []Entity
		want       string
	}{
		{"plain escapes", `a*b_c [d] <e> ~f~ |g| h\i &amp; ` + "`j`", nil,
			`a\*b\_c \[d\] \<e\> \~f\~ \|g\| h\\i \&amp; ` + "\\`j\\`"},
		{"line starts", "# h\n- l\n+ p\n= s\n1. n\n12) m\n> q\nok 1. mid", nil,
			"\\# h\\\n\\- l\\\n\\+ p\\\n\\= s\\\n1\\. n\\\n12\\) m\\\n\\> q\\\nok 1. mid"},
		{"leading spaces", "    code?\n\tx", nil, "&#32;&#32;&#32;&#32;code?\\\n&#9;x"},
		{"paragraphs", "a\n\nb\n\n\nc", nil, "a\n\nb\n\nc"},
		{"bold italic strike", "bold it st", []Entity{ent("bold", 0, 4), ent("italic", 5, 2), ent("strikethrough", 8, 2)},
			"**bold** *it* ~~st~~"},
		{"nested", "bold italic", []Entity{ent("bold", 0, 11), ent("italic", 5, 6)}, "**bold *italic***"},
		{"whitespace outside delimiters", "a bold b", []Entity{ent("bold", 1, 6)}, "a **bold** b"},
		{"code", "x a`b y", []Entity{ent("code", 2, 3)}, "x ``a`b`` y"},
		{"code with edge backtick", "`x", []Entity{ent("code", 0, 2)}, "`` `x ``"},
		{"pre", "see\nfunc() {}\nend", []Entity{{Type: "pre", Offset: 4, Length: 9, Language: "go"}},
			"see\n```go\nfunc() {}\n```\nend"},
		{"pre with fence inside", "```x```", []Entity{{Type: "pre", Offset: 0, Length: 7, Language: "bad lang"}},
			"````bad\n```x```\n````"},
		{"text link", "click here", []Entity{{Type: "text_link", Offset: 6, Length: 4, URL: "https://example.com/a)b"}},
			"click [here](<https://example.com/a)b>)"},
		{"refused link", "click here", []Entity{{Type: "text_link", Offset: 6, Length: 4, URL: "javascript:alert(1)"}},
			"click here"},
		{"tg link", "user", []Entity{{Type: "text_link", Offset: 0, Length: 4, URL: "tg://user?id=1"}}, "user"},
		{"url", "go https://example.com/a_b now", []Entity{ent("url", 3, 23)},
			"go [https://example.com/a\\_b](<https://example.com/a_b>) now"},
		{"url without scheme", "example.com", []Entity{ent("url", 0, 11)}, "example.com"},
		{"email", "a@example.com", []Entity{ent("email", 0, 13)}, "[a@example.com](<mailto:a@example.com>)"},
		{"mention", "hi @some_bot", []Entity{ent("mention", 3, 9)}, "hi [@some\\_bot](<https://t.me/some_bot>)"},
		{"spoiler underline plain", "secret under", []Entity{ent("spoiler", 0, 6), ent("underline", 7, 5)}, "secret under"},
		{"custom emoji", "hi 😀", []Entity{{Type: "custom_emoji", Offset: 3, Length: 2, CustomEmojiID: "1"}}, "hi 😀"},
		{"text mention", "Bob", []Entity{ent("text_mention", 0, 3)}, "Bob"},
		{"hashtag and command", "#tag /start", []Entity{ent("hashtag", 0, 4), ent("bot_command", 5, 6)}, "\\#tag /start"},
		{"blockquote", "intro\nquoted line\nsecond\nafter", []Entity{ent("blockquote", 6, 18)},
			"intro\n> quoted line\\\n> second\n\nafter"},
		{"expandable quote", "q", []Entity{ent("expandable_blockquote", 0, 1)}, "> q"},
		{"pre in quote", "a\ncode", []Entity{ent("blockquote", 0, 6), {Type: "pre", Offset: 2, Length: 4}}, "> a\n> ```\n> code\n> ```"},
		{"hard break in bold", "a\nb", []Entity{ent("bold", 0, 3)}, "**a\\\nb**"},
		{"paragraph in bold", "a\n\nb", []Entity{ent("bold", 0, 4)}, "**a\\\nb**"},
		{"control characters", "a\x00b\x1bc", nil, "a b c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Markdown(tc.text, tc.ents); got != tc.want {
				t.Fatalf("Markdown = %q\n           want %q", got, tc.want)
			}
		})
	}
}

func TestMarkdownMalformedEntities(t *testing.T) {
	text := "hello 😀 world"
	for _, tc := range []struct {
		name string
		ents []Entity
		want string
	}{
		{"negative offset", []Entity{ent("bold", -1, 3)}, "hello 😀 world"},
		{"zero length", []Entity{ent("bold", 0, 0)}, "hello 😀 world"},
		{"negative length", []Entity{ent("bold", 2, -5)}, "hello 😀 world"},
		{"past the end", []Entity{ent("bold", 100, 3)}, "hello 😀 world"},
		{"length past the end", []Entity{ent("bold", 9, 100)}, "hello 😀 **world**"},
		{"huge length", []Entity{ent("bold", 0, 1<<62)}, "**hello 😀 world**"},
		{"start in a surrogate pair", []Entity{ent("bold", 7, 1)}, "hello **😀** world"},
		{"end in a surrogate pair", []Entity{ent("bold", 6, 1)}, "hello **😀** world"},
		{"overlap", []Entity{ent("bold", 0, 5), ent("italic", 3, 8)}, "**hello** 😀 world"},
		{"same range", []Entity{ent("bold", 0, 5), ent("italic", 0, 5)}, "***hello*** 😀 world"},
		{"code contains nothing", []Entity{ent("code", 0, 5), ent("bold", 1, 2)}, "`hello` 😀 world"},
		{"nested links", []Entity{{Type: "text_link", Offset: 0, Length: 11, URL: "https://a.example"}, {Type: "text_link", Offset: 0, Length: 5, URL: "https://b.example"}},
			"[hello 😀 wo](<https://a.example>)rld"},
		{"quote in bold", []Entity{ent("bold", 0, 14), ent("blockquote", 0, 5)}, "**hello 😀 world**"},
		{"nested quotes", []Entity{ent("blockquote", 0, 14), ent("blockquote", 6, 2)}, "> hello 😀 world"},
		{"unknown type", []Entity{ent("future_thing", 0, 5)}, "hello 😀 world"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Markdown(text, tc.ents); got != tc.want {
				t.Fatalf("Markdown = %q, want %q", got, tc.want)
			}
		})
	}
	// Too many entities: only the first maxInboundEntities are read.
	many := make([]Entity, maxInboundEntities+100)
	long := strings.Repeat("ab", len(many))
	for i := range many {
		many[i] = ent("bold", 2*i, 1)
	}
	if got := Markdown(long, many); strings.Count(got, "**") != 2*maxInboundEntities {
		t.Fatalf("%d bold markers", strings.Count(got, "**")/2)
	}
}

// TestMarkdownRoundTrip renders Markdown to entities, converts them back, and renders again: the text and entities
// must be the same. It runs over the goldens that convert cleanly. Lists and tables come back as their text (the
// entity form has no structure to give back); a code block inside a quote, and details (an expandable quote after a
// bold summary), come back with a blank line more and as a plain quote.
func TestMarkdownRoundTrip(t *testing.T) {
	sources := []string{
		"plain *it* **bold** ~~st~~ `code` [link](https://example.com/x_y)",
		"Escapes \\* \\_ \\[x\\] <tag> & &amp; | ~ # not heading",
		"# Heading\n\npara one\nstill one\n\npara two",
		"> quoted **bold** text\n\nafter",
		"```go\nfunc() {\n\treturn\n}\n```\n\nafter code",
		"Emoji 😀 **bold 👍🏽** *ZWJ 👩\u200d💻* `𝒸ℴ𝒹ℯ` [🔗](https://example.com)",
		"1. not a list\n- not a list either",
		"x `a``b` y ``` `` ``` z",
		"a **b `c` d** e",
	}
	for _, name := range []string{"basic.md", "unicode.md", "html.md", "attachments.md", "footnotes.md", "links.md", "code.md"} {
		b, err := os.ReadFile(filepath.Join("testdata", "golden", name))
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, string(b))
	}
	for i, src := range sources {
		first := Parse(src).Entities(Options{})
		if len(first.Parts) != 1 {
			t.Fatalf("source %d: %d parts", i, len(first.Parts))
		}
		p := first.Parts[0]
		md := Markdown(p.Text, p.Entities)
		again := Parse(md).Entities(Options{})
		if len(again.Parts) != 1 || again.Parts[0].Text != p.Text || !reflect.DeepEqual(again.Parts[0].Entities, p.Entities) {
			t.Errorf("source %d does not round-trip:\nfirst  %q %+v\nmd     %q\nsecond %+v", i, p.Text, p.Entities, md, again.Parts)
		}
	}
}
