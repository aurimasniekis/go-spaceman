package proto

import "fmt"

// MinorTerminalSize is the first minor version that carries run terminal sizes (terminal.sizes, ADR-0075).
const MinorTerminalSize = 10

// TypeTerminalSizes (central → node, minor ≥ 10) is the complete set of the node's runs that have a terminal size.
const TypeTerminalSizes = "terminal.sizes"

// Terminal size bounds: the same as the node's runner.terminal_size (config.ParseTerminalSize), the space's
// terminal_cols and terminal_rows limits and the API.
const (
	TerminalMinCols = 40
	TerminalMaxCols = 400
	TerminalMinRows = 10
	TerminalMaxRows = 200
)

// CheckTerminalSize reports whether cols×rows is within the terminal size bounds.
func CheckTerminalSize(cols, rows int) error {
	if cols < TerminalMinCols || cols > TerminalMaxCols || rows < TerminalMinRows || rows > TerminalMaxRows {
		return fmt.Errorf("a terminal size must be %d-%d columns and %d-%d rows (got %dx%d)",
			TerminalMinCols, TerminalMaxCols, TerminalMinRows, TerminalMaxRows, cols, rows)
	}
	return nil
}

// TerminalSize is the size one run's terminal is held at.
type TerminalSize struct {
	SpaceID    string `json:"space_id"`
	RunID      string `json:"run_id"`
	Generation int64  `json:"generation"`
	Cols       int    `json:"cols"`
	Rows       int    `json:"rows"`
}

// TerminalSizes is the complete set of the node's runs whose terminal has a size. Central sends it after the node
// connects and whenever a size is set or cleared. The node holds each listed run's terminal at its size through an
// interactive attach of its own that never types (herdr-core resizes a program only while an interactive attach is
// open, docs/contracts/herdr-core.md S-7), and closes the attach of each run no longer listed, so the terminal goes back
// to the node's default size.
type TerminalSizes struct {
	Sizes []TerminalSize `json:"sizes"`
}
