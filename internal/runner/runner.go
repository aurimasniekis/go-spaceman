// Package runner defines SpaceMan's process/terminal supervision interface (SM-RUN-001, SM-ARCH-003). The node agent
// talks only to this interface; internal/runner/hc implements it on herdr-core.
package runner

import (
	"context"
	"errors"
	"time"
)

// Spec describes a terminal program to start.
type Spec struct {
	// Name is unique among live terminals and is the idempotency key of a launch: starting a Spec whose name is
	// already live returns ErrNameTaken (callers then adopt the existing process).
	Name   string
	Argv   []string
	Cwd    string
	Env    map[string]string // never secrets: runners may persist env (herdr-core does, ADR-0009)
	Labels map[string]string
	// Parent is the terminal whose program's end also ends this one (ADR-0017 lease guard). Empty means no parent.
	Parent string
}

// State is a process state.
type State string

// States.
const (
	StateLive     State = "live"
	StateFinished State = "finished"
)

// Status is the program-reported status (herdr-core OSC 7501; never inferred from screen text, SM-RUN-002).
type Status struct {
	State    string `json:"state,omitempty"` // idle, working, done, blocked, error
	App      string `json:"app,omitempty"`
	Kind     string `json:"kind,omitempty"` // permission, question, auth
	Message  string `json:"msg,omitempty"`
	Reported bool   `json:"reported"`
}

// Process is one supervised terminal.
type Process struct {
	// ID is the runner's permanent address of the terminal on its core (herdr-core "t7").
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	CoreID    string            `json:"core_id"`
	PID       int               `json:"pid"`
	State     State             `json:"state"`
	Reason    string            `json:"reason,omitempty"`
	ExitCode  *int              `json:"exit_code,omitempty"`
	Signal    int               `json:"signal,omitempty"` // signal number that ended it, 0 if none
	Created   time.Time         `json:"created"`
	Finished  *time.Time        `json:"finished,omitempty"`
	Labels    map[string]string `json:"labels"`
	Status    Status            `json:"status"`
	Turn      uint64            `json:"turn"`
	Cols      int               `json:"cols"`
	Rows      int               `json:"rows"`
	Parent    string            `json:"parent,omitempty"`
	Argv      []string          `json:"argv,omitempty"`
	ScreenRaw string            `json:"-"`
}

// Identity is the tuple that identifies one process instance across restarts: herdr-core has no process start time,
// so (core, address, created, pid) is used (docs/contracts/herdr-core.md).
func (p Process) Identity() string {
	return p.CoreID + "/" + p.ID + "/" + p.Created.UTC().Format(time.RFC3339Nano)
}

// Errors.
var (
	ErrNameTaken = errors.New("runner: a live terminal already has this name")
	ErrNotFound  = errors.New("runner: terminal not found")
	ErrFinished  = errors.New("runner: terminal finished")
	ErrBusy      = errors.New("runner: terminal is not idle")
)

// Stream is an attached terminal output stream. Frames arrive in order; there are no byte offsets, so a re-attach
// after any interruption starts with a fresh screen repaint (callers record a gap).
type Stream interface {
	// Next returns the next output chunk, or the close information when the terminal finished (io.EOF-like: ok false).
	Next(ctx context.Context) (data []byte, closed *CloseInfo, err error)
	// Resize changes the terminal size (only for interactive attaches).
	Resize(cols, rows int) error
	// Write sends input (only for interactive attaches; observers' input is discarded by the runner).
	Write(p []byte) error
	Close() error
}

// CloseInfo is the terminal end reported on a stream.
type CloseInfo struct {
	ExitCode *int   `json:"exit_code"`
	Reason   string `json:"reason"`
	Signal   *int   `json:"signal"`
	Finished int64  `json:"finished_ms"`
}

// Event is a runner change notification.
type Event struct {
	Seq  uint64
	Kind string // snapshot, created, lost, restarted, finished, removed, status, turn, title, labels, size, foreground, …
	// TerminalID is the terminal the event is about.
	TerminalID string
	// Process is set for "created" (the event carries the full terminal); other kinds carry partial Data.
	Process *Process
	// Data is the event's raw payload (e.g. finished: {exit_code, finished_ms, reason, signal}).
	Data []byte
	// Missed is set when the runner dropped events for this subscriber; Snapshot then holds the full state.
	Missed   bool
	Snapshot []Process
}

// Runner supervises terminal programs.
type Runner interface {
	Start(ctx context.Context, s Spec) (*Process, error)
	Get(ctx context.Context, id string) (*Process, error)
	// List returns live terminals (and finished ones with all) whose labels include every given pair.
	List(ctx context.Context, labels map[string]string, all bool) ([]Process, error)
	Kill(ctx context.Context, id string) (*Process, error)
	// Interrupt asks the program to stop its current work (Esc for status-reporting programs, else Ctrl+C).
	Interrupt(ctx context.Context, id string) (outcome string, err error)
	// Send pastes text and presses Enter; with ifIdle it fails with ErrBusy unless the program reports idle/done.
	Send(ctx context.Context, id, text string, ifIdle bool) error
	Remove(ctx context.Context, id string) error
	Attach(ctx context.Context, id string, observe bool, cols, rows int) (Stream, *Process, error)
	// Takeover opens an interactive attach for an operator's takeover (SM-UI-004): input written to the stream reaches
	// the program as keystrokes. It attaches at the terminal's current size, so the program's screen keeps its size,
	// and it uses a connection of its own, so Close detaches for certain (the core drops an attach with its connection).
	Takeover(ctx context.Context, id string) (Stream, *Process, error)
	// Subscribe streams change events; the first event is a snapshot.
	Subscribe(ctx context.Context, labels map[string]string) (<-chan Event, error)
	Close() error
}
