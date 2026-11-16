package notify

import (
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/card"
)

// renderCard renders a resource's card the way the notifier does for a link without buttons.
func renderCard(r *Resource, full bool, appURL string) []cardPart {
	v := card.View{Label: r.Label, Title: r.Title, State: r.State, Full: full, Final: r.Final}
	if full {
		v.Body = r.Detail
	}
	return card.Render(v, card.Caps{AppURL: appURL})
}

const (
	maxParts   = card.MaxParts
	maxMessage = card.MaxMessage
)

func TestRenderCardMarkdown(t *testing.T) {
	r := &Resource{Label: "Message", Title: "Hi", State: "Unread",
		Detail: "**The work:** did `x`\n\n- one\n- two\n\n<script>alert(1)</script>\n\n[bad](javascript:alert(1)) [ok](https://example.com)"}
	p := renderCard(r, true, "")
	if len(p) != 1 {
		t.Fatalf("%d parts, want 1", len(p))
	}
	if strings.Contains(p[0].Text, "**") || strings.Contains(p[0].Text, "`") || !strings.Contains(p[0].Text, "• one") {
		t.Errorf("text = %q, want formatting as entities and text bullets", p[0].Text)
	}
	var bold, code, link int
	for _, e := range p[0].Entities {
		switch e.Type {
		case "bold":
			bold++
		case "code":
			code++
		case "text_link":
			link++
			if e.URL != "https://example.com" {
				t.Errorf("link %q", e.URL)
			}
		}
	}
	if bold != 2 || code != 1 || link != 1 {
		t.Errorf("bold %d code %d link %d, want 2 1 1 (%+v)", bold, code, link, p[0].Entities)
	}
	for _, e := range p[0].Entities {
		if strings.Contains(strings.ToLower(e.URL), "javascript:") {
			t.Errorf("unsafe link kept as an entity: %+v", e)
		}
	}
	if !strings.Contains(p[0].Text, "bad") || !strings.Contains(p[0].Text, "ok") {
		t.Errorf("link texts dropped: %q", p[0].Text)
	}
}

func TestRenderCardSummaryNeverShowsBody(t *testing.T) {
	p := renderCard(&Resource{Label: "Message", Title: "Hi", Detail: "secret body"}, false, "")
	if len(p) != 1 || strings.Contains(p[0].Text, "secret") {
		t.Errorf("parts = %+v", p)
	}
}

// TestRenderCardUnitsAndEntityBounds: after the title offset, every part fits 4096 units and every entity lies inside
// its text.
func TestRenderCardUnitsAndEntityBounds(t *testing.T) {
	body := strings.Repeat("😀 **bold 😀** `code` text\n\n", 900)
	p := renderCard(&Resource{Label: "Message", Title: strings.Repeat("t", 300), State: "Unread", Detail: body}, true, "https://app.example/m")
	if len(p) < 2 || len(p) > maxParts {
		t.Fatalf("%d parts", len(p))
	}
	for i, part := range p {
		n := len(utf16.Encode([]rune(part.Text)))
		if n > maxMessage {
			t.Errorf("part %d: %d units", i, n)
		}
		for _, e := range part.Entities {
			if e.Offset < 0 || e.Offset+e.Length > n {
				t.Errorf("part %d: entity %+v outside %d units", i, e, n)
			}
		}
	}
	if last := p[len(p)-1]; len(last.Buttons) != 1 || last.Buttons[0].WebAppURL != "https://app.example/m" {
		t.Errorf("last part buttons = %+v", last.Buttons)
	}
}

func TestMessageState(t *testing.T) {
	for in, want := range map[string]string{"stored": "Unread", "available": "Unread", "acknowledged": "Read", "resolved": "Resolved", "x": ""} {
		if got := messageState(in); got != want {
			t.Errorf("messageState(%q) = %q, want %q", in, got, want)
		}
	}
}
