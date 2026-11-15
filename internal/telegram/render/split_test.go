package render

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func units(s string) int { return len(utf16.Encode([]rune(s))) }

func TestShortBodyIsOnePart(t *testing.T) {
	md := strings.Repeat("a", Limits.Text)
	m := render(t, md, Options{})
	if len(m.Parts) != 1 || units(m.Parts[0].Text) != Limits.Text || m.Truncated {
		t.Fatalf("%d parts, truncated %v", len(m.Parts), m.Truncated)
	}
}

func TestSplitOnBlockBoundaries(t *testing.T) {
	para := strings.Repeat("word ", 150) // 750 units
	var md strings.Builder
	for range 10 {
		md.WriteString(para + "\n\n")
	}
	m := render(t, md.String(), Options{})
	if len(m.Parts) != 2 {
		t.Fatalf("%d parts", len(m.Parts))
	}
	for i, p := range m.Parts {
		// Every part holds whole paragraphs.
		for chunk := range strings.SplitSeq(p.Text, "\n\n") {
			if chunk != strings.TrimSpace(para) {
				t.Fatalf("part %d holds a cut paragraph %q…", i, chunk[:20])
			}
		}
	}
}

func TestSplitPrefersBlocksOverLines(t *testing.T) {
	// A paragraph and then a list too long for the rest of the part: the cut goes between the two (a block boundary in
	// the last three quarters of the window) rather than between list items, and the list then splits on items.
	md := strings.Repeat("x", 1500) + "\n\n" + strings.Repeat("- item line here\n", 300)
	m := render(t, md, Options{})
	if m.Parts[0].Text != strings.Repeat("x", 1500) {
		t.Fatalf("first part ends %q", m.Parts[0].Text[len(m.Parts[0].Text)-20:])
	}
	for _, p := range m.Parts[1:] {
		if !strings.HasPrefix(p.Text, "• item") {
			t.Fatalf("part starts %q", p.Text[:10])
		}
	}
}

func TestSplitCodeBlockContinuesPre(t *testing.T) {
	var code strings.Builder
	for range 600 {
		code.WriteString("fmt.Println(\"line\")\n")
	}
	m := render(t, "```go\n"+code.String()+"```", Options{})
	if len(m.Parts) < 3 {
		t.Fatalf("%d parts", len(m.Parts))
	}
	for i, p := range m.Parts {
		if i == len(m.Parts)-1 && m.Truncated {
			break
		}
		if len(p.Entities) != 1 || p.Entities[0].Type != EntityPre || p.Entities[0].Language != "go" ||
			p.Entities[0].Offset != 0 || p.Entities[0].Length != units(p.Text) {
			t.Fatalf("part %d entities %+v", i, p.Entities)
		}
		// Cut on line ends.
		if !strings.HasSuffix(p.Text, `fmt.Println("line")`) || !strings.HasPrefix(p.Text, "fmt.") {
			t.Fatalf("part %d cut inside a line", i)
		}
	}
}

func TestSplitWithoutBreaks(t *testing.T) {
	// One unbroken word: the cut is hard, at the limit.
	m := render(t, strings.Repeat("a", Limits.Text*2+10), Options{})
	if len(m.Parts) != 3 || units(m.Parts[0].Text) != Limits.Text || units(m.Parts[1].Text) != Limits.Text {
		t.Fatalf("parts %d", len(m.Parts))
	}
}

func TestSplitNeverCutsSurrogatePairs(t *testing.T) {
	// Emoji are two units each; an odd prefix puts the limit inside a pair.
	for _, prefix := range []string{"", "a", "ab"} {
		md := prefix + strings.Repeat("😀", Limits.Text)
		m := render(t, md, Options{})
		total := 0
		for i, p := range m.Parts {
			if strings.ContainsRune(p.Text, '�') {
				t.Fatalf("part %d holds a broken character", i)
			}
			if i < len(m.Parts)-1 || !m.Truncated {
				total += units(p.Text)
			}
		}
		if !m.Truncated && total != units(md) {
			t.Fatalf("lost text: %d of %d units", total, units(md))
		}
	}
}

func TestSplitEntitiesAcrossTheCut(t *testing.T) {
	md := "**" + strings.TrimSpace(strings.Repeat("bold ", 1200)) + "**"
	m := render(t, md, Options{})
	if len(m.Parts) != 2 {
		t.Fatalf("%d parts", len(m.Parts))
	}
	for i, p := range m.Parts {
		if len(p.Entities) != 1 || p.Entities[0].Type != EntityBold || p.Entities[0].Offset != 0 || p.Entities[0].Length != units(p.Text) {
			t.Fatalf("part %d entities %+v", i, p.Entities)
		}
	}
}

func TestMaxPartsAndContinuation(t *testing.T) {
	md := strings.Repeat(strings.Repeat("word ", 100)+"\n\n", 100) // ~50k units
	o := Options{ContinueURL: "https://tg.example.com/item"}
	m := render(t, md, o)
	if len(m.Parts) != Limits.Parts || !m.Truncated {
		t.Fatalf("%d parts, truncated %v", len(m.Parts), m.Truncated)
	}
	last := m.Parts[len(m.Parts)-1]
	if !strings.HasSuffix(last.Text, "\n\n"+continuedInApp) || units(last.Text) > Limits.Text {
		t.Fatalf("last part ends %q (%d units)", last.Text[len(last.Text)-40:], units(last.Text))
	}
	if len(last.Buttons) != 1 || last.Buttons[0] != (Button{Text: openInApp, WebAppURL: o.ContinueURL}) {
		t.Fatalf("buttons %+v", last.Buttons)
	}
	if m := render(t, md, Options{}); len(m.Parts[len(m.Parts)-1].Buttons) != 0 {
		t.Fatal("continue button without a URL")
	}
}

func TestMaxPartsExactlyFull(t *testing.T) {
	// The continuation line still fits when the last part is full to the limit.
	md := strings.Repeat("a", Limits.Text*Limits.Parts+1)
	m := render(t, md, Options{})
	if !m.Truncated || len(m.Parts) != Limits.Parts {
		t.Fatalf("%d parts, truncated %v", len(m.Parts), m.Truncated)
	}
}

func TestTruncatedSourceIsMarked(t *testing.T) {
	md := strings.Repeat("x\n\n", Limits.Source)
	m := render(t, md, Options{MaxParts: 100, Limit: Limits.Text})
	if !m.Truncated || !strings.HasSuffix(m.Parts[len(m.Parts)-1].Text, continuedInApp) {
		t.Fatalf("truncated %v", m.Truncated)
	}
}

func TestCaptionLimit(t *testing.T) {
	o := Options{Limit: Limits.Caption, MaxParts: 1}
	m := render(t, strings.Repeat("caption ", 300), o)
	if len(m.Parts) != 1 || units(m.Parts[0].Text) > Limits.Caption || !m.Truncated {
		t.Fatalf("%d parts, %d units", len(m.Parts), units(m.Parts[0].Text))
	}
}

func TestDiagramButtonsFollowTheirPart(t *testing.T) {
	md := "```mermaid\ngraph A\n```\n\n" + strings.Repeat("filler text ", 400) + "\n\n```mermaid\ngraph B\n```"
	o := Options{DiagramURL: func(n int) string { return "https://tg.example.com/d/" + string(rune('0'+n)) }}
	m := render(t, md, o)
	if len(m.Parts) != 2 {
		t.Fatalf("%d parts", len(m.Parts))
	}
	if len(m.Parts[0].Buttons) != 1 || m.Parts[0].Buttons[0].WebAppURL != "https://tg.example.com/d/0" ||
		len(m.Parts[1].Buttons) != 1 || m.Parts[1].Buttons[0].WebAppURL != "https://tg.example.com/d/1" {
		t.Fatalf("buttons %+v / %+v", m.Parts[0].Buttons, m.Parts[1].Buttons)
	}
}
