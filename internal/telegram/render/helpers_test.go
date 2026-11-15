package render

import (
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// validatePart checks one part against Telegram's rules (docs/contracts/telegram.md): the length limit in UTF-16 code
// units, entity ranges inside the text and on character boundaries, ordering, proper nesting, and the containment
// rules (pre and code contain nothing and sit in no style; links contain no code, pre or link; quotes don't nest).
func validatePart(p Part, limit int) error {
	if !utf8.ValidString(p.Text) {
		return fmt.Errorf("invalid UTF-8")
	}
	u := utf16.Encode([]rune(p.Text))
	if len(u) == 0 {
		return fmt.Errorf("empty part")
	}
	if len(u) > limit {
		return fmt.Errorf("part is %d UTF-16 units, limit %d", len(u), limit)
	}
	for i, e := range p.Entities {
		end := e.Offset + e.Length
		if e.Offset < 0 || e.Length <= 0 || end > len(u) {
			return fmt.Errorf("entity %d %+v out of range (len %d)", i, e, len(u))
		}
		if isLow(u, e.Offset) || isLow(u, end) {
			return fmt.Errorf("entity %d %+v splits a surrogate pair", i, e)
		}
		if i > 0 {
			prev := p.Entities[i-1]
			if prev.Offset > e.Offset || (prev.Offset == e.Offset && prev.Length < e.Length) {
				return fmt.Errorf("entities not sorted at %d", i)
			}
		}
		switch e.Type {
		case EntityTextLink:
			if _, ok := AllowedURL(e.URL); !ok || e.URL == "" {
				return fmt.Errorf("entity %d links to a refused URL %q", i, e.URL)
			}
		case EntityPre:
			if e.Language != "" && !languageRE.MatchString(e.Language) {
				return fmt.Errorf("entity %d has language %q", i, e.Language)
			}
		}
		for j, f := range p.Entities {
			if i == j {
				continue
			}
			fend := f.Offset + f.Length
			if fend <= e.Offset || f.Offset >= end {
				continue
			}
			contains := f.Offset >= e.Offset && fend <= end
			inside := e.Offset >= f.Offset && end <= fend
			if !contains && !inside {
				return fmt.Errorf("entities %+v and %+v overlap without nesting", e, f)
			}
			if !contains {
				continue
			}
			// e contains f (or they cover the same range).
			if err := mayContain(e.Type, f.Type); err != nil && (!inside || mayContain(f.Type, e.Type) != nil) {
				return fmt.Errorf("%+v contains %+v: %w", e, f, err)
			}
		}
	}
	return nil
}

func mayContain(outer, inner string) error {
	switch {
	case outer == EntityPre || outer == EntityCode:
		return fmt.Errorf("pre and code can't contain entities")
	case inner == EntityPre:
		return fmt.Errorf("pre can't be inside another entity")
	case inner == EntityCode && (isStyle(outer) || outer == EntityTextLink):
		return fmt.Errorf("code inside a style or link")
	case outer == EntityTextLink && inner == EntityTextLink:
		return fmt.Errorf("nested links")
	case isQuote(outer) && isQuote(inner):
		return fmt.Errorf("nested quotes")
	case isQuote(inner) && !isQuote(outer):
		return fmt.Errorf("quote inside %s", outer)
	}
	return nil
}

func isQuote(t string) bool { return t == EntityBlockquote || t == EntityExpandable }

func isLow(u []uint16, i int) bool { return i > 0 && i < len(u) && u[i] >= 0xdc00 && u[i] < 0xe000 }

// validateMessage checks every part and the part count.
func validateMessage(m Message, o Options) error {
	if len(m.Parts) > o.maxParts() {
		return fmt.Errorf("%d parts, at most %d", len(m.Parts), o.maxParts())
	}
	for i, p := range m.Parts {
		if err := validatePart(p, o.limit()); err != nil {
			return fmt.Errorf("part %d: %w", i, err)
		}
	}
	return nil
}

// entityText returns the text an entity covers.
func entityText(p Part, e Entity) string {
	u := utf16.Encode([]rune(p.Text))
	return string(utf16.Decode(u[e.Offset : e.Offset+e.Length]))
}
