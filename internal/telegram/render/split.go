package render

import (
	"strconv"
	"unicode/utf16"
)

// split cuts the rendered text into parts of at most o.limit() UTF-16 units, preferring block boundaries, then list
// items and lines, then spaces. Entities that cross a cut are clipped into both parts, so a code block split inside
// continues as a pre entity with the same language. After o.maxParts() parts the rest is dropped and the last part ends
// with a "continued in the app" line.
func split(b *builder, o Options, truncated bool) Message {
	sortEntities(b.ents)
	text := b.buf
	limit, maxParts := o.limit(), o.maxParts()
	end := trimRight(text, 0, len(text))
	var cuts [][2]int
	pos := skipNewlines(text, 0, end)
	for pos < end && len(cuts) < maxParts {
		cut := end
		if end-pos > limit {
			cut = findCut(b, pos, pos+limit)
		}
		// A window of only whitespace (a run of spaces past a cut) makes no part.
		if t := trimRight(text, pos, cut); t > pos {
			cuts = append(cuts, [2]int{pos, t})
		}
		pos = skipNewlines(text, cut, end)
	}
	if len(cuts) == 0 {
		return Message{Truncated: truncated}
	}
	suffix := "\n\n" + continuedInApp
	if pos < end {
		truncated = true
	}
	if truncated {
		last := &cuts[len(cuts)-1]
		if budget := limit - len(utf16.Encode([]rune(suffix))); last[1]-last[0] > budget {
			last[1] = trimRight(text, last[0], findCut(b, last[0], last[0]+budget))
		}
	}
	msg := Message{Truncated: truncated}
	diag := 0
	for i, c := range cuts {
		p := Part{Text: utf16Decode(text[c[0]:c[1]]), Entities: clip(b.ents, c[0], c[1])}
		for diag < len(b.diagrams) && b.diagrams[diag] < c[1] {
			if b.diagrams[diag] >= c[0] && o.DiagramURL != nil && diag < Limits.Diagrams {
				label := viewDiagram
				if len(b.diagrams) > 1 {
					label += " " + strconv.Itoa(diag+1)
				}
				p.Buttons = append(p.Buttons, Button{Text: label, WebAppURL: o.DiagramURL(diag)})
			}
			diag++
		}
		if truncated && i == len(cuts)-1 {
			if p.Text == "" {
				p.Text = continuedInApp
			} else {
				p.Text += suffix
			}
			if o.ContinueURL != "" {
				p.Buttons = append(p.Buttons, Button{Text: openInApp, WebAppURL: o.ContinueURL})
			}
		}
		msg.Parts = append(msg.Parts, p)
	}
	return msg
}

// findCut returns where to end a part that starts at lo and may not pass hi. It takes the best break point in the
// last three quarters of the window (a block boundary over a list item over a line), then the last space, and else
// cuts at hi (never inside a surrogate pair).
func findCut(b *builder, lo, hi int) int {
	floor := lo + (hi-lo)/4
	best, prio := -1, 0
	for _, br := range b.breaks {
		if br.at <= floor || br.at > hi {
			continue
		}
		if br.prio >= prio {
			best, prio = br.at, br.prio
		}
	}
	if best > 0 {
		return best
	}
	for i := hi; i > floor; i-- {
		if i < len(b.buf) && b.buf[i] == ' ' {
			return i
		}
	}
	if hi < len(b.buf) && utf16.IsSurrogate(rune(b.buf[hi])) && b.buf[hi] >= 0xdc00 {
		hi--
	}
	return hi
}

// clip returns the entities that overlap [lo, hi), shifted to start at lo and cut to the range.
func clip(es []Entity, lo, hi int) []Entity {
	var out []Entity
	for _, e := range es {
		s, t := max(e.Offset, lo), min(e.Offset+e.Length, hi)
		if t <= s {
			continue
		}
		e.Offset, e.Length = s-lo, t-s
		out = append(out, e)
	}
	return out
}

func isSpace(u uint16) bool { return u == ' ' || u == '\n' || u == '\t' }

// trimRight returns hi moved back over trailing whitespace, but not before lo.
func trimRight(text []uint16, lo, hi int) int {
	for hi > lo && isSpace(text[hi-1]) {
		hi--
	}
	return hi
}

// skipNewlines returns pos moved forward over line breaks, but not past end. Leading spaces stay: they may be a nested
// list item's indentation or code.
func skipNewlines(text []uint16, pos, end int) int {
	for pos < end && text[pos] == '\n' {
		pos++
	}
	return pos
}

func utf16Decode(u []uint16) string { return string(utf16.Decode(u)) }
