package recording

import (
	"io"
	"slices"
)

// MaxTailBytes is the largest output tail a reader may ask for (Tail): it bounds what one catch-up holds in memory.
const MaxTailBytes = 16 << 20

// Mode is a DEC private mode's state (set with CSI ? n h, reset with CSI ? n l).
type Mode struct {
	Mode int  `json:"mode"`
	On   bool `json:"on"`
}

// trackedModes are the DEC private modes a tail's prelude restores: those that change how later output is drawn or
// how the viewer's input is read (ADR-0081). Cursor keys (1), origin mode (6), autowrap (7), cursor visibility (25),
// the alternate screen (47, 1047, 1049), mouse reporting (1000-1006, 1015) and bracketed paste (2004).
var trackedModes = map[int]bool{1: true, 6: true, 7: true, 25: true, 47: true, 1047: true, 1049: true, 1000: true, 1002: true,
	1003: true, 1004: true, 1005: true, 1006: true, 1015: true, 2004: true}

// ModeTracker follows the DEC private modes of terminal output written to it, across writes that split an escape
// sequence. It understands only CSI ? Pm h / l and RIS (ESC c); everything else is skipped.
type ModeTracker struct {
	modes  []Mode // in the order they last changed
	state  int
	param  int
	params []int
	big    bool
}

const (
	mtGround = iota
	mtEsc
	mtCSI
	mtPrivate
)

// Write feeds output to the tracker.
func (m *ModeTracker) Write(b []byte) {
	for _, c := range b {
		switch m.state {
		case mtGround:
			if c == 0x1b {
				m.state = mtEsc
			}
		case mtEsc:
			switch c {
			case '[':
				m.state = mtCSI
			case 'c':
				m.modes = m.modes[:0]
				m.state = mtGround
			case 0x1b:
			default:
				m.state = mtGround
			}
		case mtCSI:
			if c == '?' {
				m.state, m.param, m.params, m.big = mtPrivate, 0, m.params[:0], false
			} else {
				m.state = mtGround
			}
		case mtPrivate:
			switch {
			case c >= '0' && c <= '9':
				if m.param < 100000 {
					m.param = m.param*10 + int(c-'0')
				} else {
					m.big = true
				}
			case c == ';':
				m.params = append(m.params, m.param)
				m.param = 0
				if len(m.params) > 32 {
					m.state = mtGround
				}
			case c == 'h' || c == 'l':
				if !m.big {
					for _, p := range append(m.params, m.param) {
						m.set(p, c == 'h')
					}
				}
				m.state = mtGround
			case c == 0x1b:
				m.state = mtEsc
			default:
				m.state = mtGround
			}
		}
	}
}

func (m *ModeTracker) set(mode int, on bool) {
	if !trackedModes[mode] {
		return
	}
	m.modes = slices.DeleteFunc(m.modes, func(x Mode) bool { return x.Mode == mode })
	m.modes = append(m.modes, Mode{Mode: mode, On: on})
}

// Modes returns the tracked modes the output set or reset, in the order they last changed.
func (m *ModeTracker) Modes() []Mode { return slices.Clone(m.modes) }

// TailResult is the end of a recording: the records whose output fits a byte budget, and what a terminal needs to know
// about the records before them to draw the tail correctly.
type TailResult struct {
	// Records are the tail, in order.
	Records []Record
	// Skipped is how many bytes of output come before the tail (0: the tail is the whole recording).
	Skipped int64
	// Cols and Rows are the terminal size in effect where the tail starts (0 when no size was recorded before it).
	Cols, Rows int
	// Modes are the DEC private modes the output before the tail left set or reset.
	Modes []Mode
	// SkippedGaps counts the explicit gap records before the tail; SkippedTrunc is set when a trunc record is there.
	SkippedGaps  int
	SkippedTrunc bool
	// LastSeq is the last sequence number of the recording.
	LastSeq uint64
	// Truncated and Gaps are as in ReadResult, over the whole recording.
	Truncated bool
	Gaps      int
}

// TailSegments scans the named segments and returns the records at the end whose output adds up to at most budget
// bytes (always at least the last record). It holds only that window in memory: records leaving it pass through a
// ModeTracker, so the tail can be drawn on a fresh terminal after a prelude that restores the modes (ADR-0081).
// budget <= 0 or above MaxTailBytes is MaxTailBytes.
func TailSegments(names []string, open func(name string) (io.ReadCloser, error), budget int64) (TailResult, error) {
	if budget <= 0 || budget > MaxTailBytes {
		budget = MaxTailBytes
	}
	var (
		out     TailResult
		window  []Record
		head    int
		inBytes int64
		modes   ModeTracker
	)
	drop := func() {
		r := window[head]
		window[head] = Record{}
		head++
		switch r.Kind {
		case KindOut:
			modes.Write(r.Data)
			out.Skipped += int64(len(r.Data))
			inBytes -= int64(len(r.Data))
		case KindOpen, KindSize:
			if r.Cols > 0 && r.Rows > 0 {
				out.Cols, out.Rows = r.Cols, r.Rows
			}
		case KindGap:
			out.SkippedGaps++
		case KindTrunc:
			out.SkippedTrunc = true
		}
	}
	res, err := ScanSegments(names, open, 0, func(r Record) error {
		out.LastSeq = r.Seq
		window = append(window, r)
		if r.Kind == KindOut {
			inBytes += int64(len(r.Data))
		}
		for inBytes > budget && len(window)-head > 1 {
			drop()
		}
		if head > 4096 && head*2 > len(window) {
			window = append(window[:0:0], window[head:]...)
			head = 0
		}
		return nil
	})
	if err != nil {
		return TailResult{}, err
	}
	out.Records = append([]Record{}, window[head:]...)
	out.Modes = modes.Modes()
	out.Truncated, out.Gaps = res.Truncated, res.Gaps
	return out, nil
}
