package render

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/golden/*.json")

// goldenOptions are the options every golden renders with.
var goldenOptions = Options{
	DiagramURL:  func(n int) string { return "https://tg.example.com/#/diagram/" + strconv.Itoa(n) },
	ContinueURL: "https://tg.example.com/#/item",
}

type goldenEntity struct {
	Entity
	Covers string `json:"covers"`
}

type goldenPart struct {
	Text     string         `json:"text"`
	Entities []goldenEntity `json:"entities"`
	Buttons  []Button       `json:"buttons,omitempty"`
}

type golden struct {
	Parts       []goldenPart    `json:"parts"`
	Attachments []string        `json:"attachments,omitempty"`
	Rich        json.RawMessage `json:"rich"`
	// Markdown is part 1 converted back with Markdown (the inbound direction).
	Markdown string `json:"markdown"`
}

// TestGoldens renders every testdata/golden/*.md to entities, a rich message and back to Markdown, and compares the
// result with the .json next to it. Run with -update to rewrite them, then review the diff.
func TestGoldens(t *testing.T) {
	files, err := filepath.Glob("testdata/golden/*.md")
	if err != nil || len(files) == 0 {
		t.Fatalf("no goldens: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			doc := Parse(string(src))
			msg := doc.Entities(goldenOptions)
			if err := validateMessage(msg, goldenOptions); err != nil {
				t.Fatal(err)
			}
			g := golden{Attachments: doc.Attachments}
			for _, p := range msg.Parts {
				gp := goldenPart{Text: p.Text, Buttons: p.Buttons, Entities: []goldenEntity{}}
				for _, e := range p.Entities {
					gp.Entities = append(gp.Entities, goldenEntity{Entity: e, Covers: entityText(p, e)})
				}
				g.Parts = append(g.Parts, gp)
			}
			if rich, err := doc.Rich(goldenOptions); err != nil {
				g.Rich, _ = json.Marshal(err.Error())
			} else if g.Rich, err = json.Marshal(rich); err != nil {
				t.Fatal(err)
			}
			if len(msg.Parts) > 0 {
				g.Markdown = Markdown(msg.Parts[0].Text, msg.Parts[0].Entities)
			}
			var got bytes.Buffer
			enc := json.NewEncoder(&got)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			if err := enc.Encode(g); err != nil {
				t.Fatal(err)
			}
			want := strings.TrimSuffix(f, ".md") + ".json"
			if *update {
				if err := os.WriteFile(want, got.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			exp, err := os.ReadFile(want)
			if err != nil {
				t.Fatalf("%v (run go test -update)", err)
			}
			if !bytes.Equal(exp, got.Bytes()) {
				t.Errorf("%s differs from the rendering (run go test -update and review the diff):\n%s", want, got.String())
			}
		})
	}
}
