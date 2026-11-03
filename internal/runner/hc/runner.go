package hc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// terminal is herdr-core's Terminal object (subset SpaceMan uses; docs/contracts/herdr-core.md).
type terminal struct {
	Address    string            `json:"address"`
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels"`
	Argv       []string          `json:"argv"`
	PID        int               `json:"pid"`
	State      string            `json:"state"`
	CreatedMs  int64             `json:"created_ms"`
	FinishedMs *int64            `json:"finished_ms"`
	ExitCode   *int              `json:"exit_code"`
	Signal     *int              `json:"signal"` // a signal number (verified: SIGHUP = 1)
	Reason     *string           `json:"reason"`
	Status     *struct {
		State string `json:"state"`
		App   string `json:"app"`
		Kind  string `json:"kind"`
		Msg   string `json:"msg"`
	} `json:"status"`
	Reported bool    `json:"reported"`
	Turn     uint64  `json:"turn"`
	Cols     int     `json:"cols"`
	Rows     int     `json:"rows"`
	Parent   *string `json:"parent"`
}

func (r *Runner) proc(t terminal) runner.Process {
	p := runner.Process{ID: t.Address, Name: t.Name, CoreID: r.c.hello.Core.ID, PID: t.PID, State: runner.State(t.State),
		ExitCode: t.ExitCode, Created: time.UnixMilli(t.CreatedMs).UTC(), Labels: t.Labels, Turn: t.Turn, Cols: t.Cols, Rows: t.Rows,
		Argv: t.Argv}
	if p.Labels == nil {
		p.Labels = map[string]string{}
	}
	if t.Reason != nil {
		p.Reason = *t.Reason
	}
	if t.Signal != nil {
		p.Signal = *t.Signal
	}
	if t.Parent != nil {
		p.Parent = *t.Parent
	}
	if t.FinishedMs != nil {
		f := time.UnixMilli(*t.FinishedMs).UTC()
		p.Finished = &f
	}
	if t.Status != nil {
		p.Status = runner.Status{State: t.Status.State, App: t.Status.App, Kind: t.Status.Kind, Message: t.Status.Msg, Reported: t.Reported}
	}
	return p
}

// Runner implements runner.Runner over one Client.
type Runner struct{ c *Client }

// New wraps a connected client.
func New(c *Client) *Runner { return &Runner{c: c} }

// Client returns the underlying connection.
func (r *Runner) Client() *Client { return r.c }

// Close closes the connection (running programs keep running; herdr-core owns them).
func (r *Runner) Close() error { return r.c.Close() }

func kind(err error) string {
	var re *RPCError
	if errors.As(err, &re) {
		return re.Data.Kind
	}
	return ""
}

func mapErr(err error) error {
	switch kind(err) {
	case "name_taken":
		return errors.Join(runner.ErrNameTaken, err)
	case "not_found":
		return errors.Join(runner.ErrNotFound, err)
	case "finished":
		return errors.Join(runner.ErrFinished, err)
	case "blocked", "status_mismatch":
		return errors.Join(runner.ErrBusy, err)
	}
	return err
}

// Start runs a program. The parent is always explicit (`no_parent` otherwise) so a terminal never inherits the
// caller's own terminal as parent (S-2).
func (r *Runner) Start(ctx context.Context, s runner.Spec) (*runner.Process, error) {
	params := map[string]any{"argv": s.Argv, "name": s.Name, "no_notify": true}
	if len(s.Labels) > 0 {
		params["labels"] = s.Labels // hc rejects null ("invalid type: null, expected a map")
	}
	if s.Cwd != "" {
		params["cwd"] = s.Cwd
	}
	if len(s.Env) > 0 {
		params["env"] = s.Env
	}
	if s.Parent != "" {
		params["parent"] = s.Parent
	} else {
		params["no_parent"] = true
	}
	var t terminal
	if err := r.c.Call(ctx, "terminal.run", params, &t); err != nil {
		return nil, mapErr(err)
	}
	p := r.proc(t)
	return &p, nil
}

// Get returns one terminal.
func (r *Runner) Get(ctx context.Context, id string) (*runner.Process, error) {
	var t terminal
	if err := r.c.Call(ctx, "terminal.get", map[string]any{"terminal": id}, &t); err != nil {
		return nil, mapErr(err)
	}
	p := r.proc(t)
	return &p, nil
}

// List returns terminals matching every label pair.
func (r *Runner) List(ctx context.Context, labels map[string]string, all bool) ([]runner.Process, error) {
	var out struct {
		Terminals []terminal `json:"terminals"`
	}
	params := map[string]any{"all": all}
	if len(labels) > 0 {
		params["labels"] = labels
	}
	if err := r.c.Call(ctx, "terminal.list", params, &out); err != nil {
		return nil, mapErr(err)
	}
	ps := make([]runner.Process, 0, len(out.Terminals))
	for _, t := range out.Terminals {
		ps = append(ps, r.proc(t))
	}
	return ps, nil
}

// Kill ends the program (hangup, terminate, kill).
func (r *Runner) Kill(ctx context.Context, id string) (*runner.Process, error) {
	var t terminal
	if err := r.c.Call(ctx, "terminal.kill", map[string]any{"terminal": id}, &t); err != nil {
		return nil, mapErr(err)
	}
	p := r.proc(t)
	return &p, nil
}

// Interrupt presses the program's stop key.
func (r *Runner) Interrupt(ctx context.Context, id string) (string, error) {
	var out struct {
		Outcome string `json:"outcome"`
	}
	if err := r.c.Call(ctx, "terminal.interrupt", map[string]any{"terminal": id}, &out); err != nil {
		return "", mapErr(err)
	}
	return out.Outcome, nil
}

// Send pastes text and presses Enter. With ifIdle it types only while the program reports idle or done.
//
// herdr-core's gate takes one exact status (`if: idle|working`) and refuses a program that reports done
// (status_mismatch "… is done, not idle", hc 0.1.16). Claude Code reports done after every turn and stays there until
// it gets input, so the idle gate alone would refuse every notice after a run's first turn. A done program has
// finished its turn and waits for input, like an idle one: when the gate refuses and the program still reports done,
// the text is sent ungated. The core still waits ~3 s without human typing before pressing Enter.
func (r *Runner) Send(ctx context.Context, id, text string, ifIdle bool) error {
	params := map[string]any{"terminal": id, "text": text}
	if !ifIdle {
		return mapErr(r.c.Call(ctx, "terminal.send", params, nil))
	}
	params["if"] = "idle"
	err := r.c.Call(ctx, "terminal.send", params, nil)
	if kind(err) != "status_mismatch" {
		return mapErr(err)
	}
	if p, gerr := r.Get(ctx, id); gerr != nil || p.Status.State != "done" {
		return mapErr(err)
	}
	delete(params, "if")
	return mapErr(r.c.Call(ctx, "terminal.send", params, nil))
}

// Remove deletes a finished terminal record.
func (r *Runner) Remove(ctx context.Context, id string) error {
	return mapErr(r.c.Call(ctx, "terminal.rm", map[string]any{"terminal": id}, nil))
}

// Label sets labels ("" values remove keys).
func (r *Runner) Label(ctx context.Context, id string, labels map[string]string) error {
	return mapErr(r.c.Call(ctx, "terminal.label", map[string]any{"terminal": id, "labels": labels}, nil))
}

// Attach opens an output stream. Observers never resize the terminal and their input is discarded (S-3).
func (r *Runner) Attach(ctx context.Context, id string, observe bool, cols, rows int) (runner.Stream, *runner.Process, error) {
	if cols <= 0 || rows <= 0 {
		cols, rows = 200, 120
	}
	var out struct {
		Channel  uint32   `json:"channel"`
		Terminal terminal `json:"terminal"`
	}
	if err := r.c.Call(ctx, "terminal.attach", map[string]any{"terminal": id, "cols": cols, "rows": rows, "observe": observe}, &out); err != nil {
		return nil, nil, mapErr(err)
	}
	p := r.proc(out.Terminal)
	return &stream{ch: r.c.takeChannel(out.Channel)}, &p, nil
}

// Takeover attaches interactively (observe false) at the terminal's current size on a dedicated connection to the same
// core: an interactive attach resizes the terminal to the viewer's size (S-3), so the current size is kept, and a
// client-side detach frame is not part of the verified protocol, so the stream's Close ends the whole connection
// (S-5, docs/contracts/herdr-core.md).
func (r *Runner) Takeover(ctx context.Context, id string) (runner.Stream, *runner.Process, error) {
	if r.c.socket == "" {
		return nil, nil, errors.New("hc: takeover needs a dialed client")
	}
	cur, err := r.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if cur.State != runner.StateLive {
		return nil, nil, runner.ErrFinished
	}
	c, err := Dial(ctx, r.c.socket)
	if err != nil {
		return nil, nil, err
	}
	sub := &Runner{c: c}
	st, p, err := sub.Attach(ctx, id, false, cur.Cols, cur.Rows)
	if err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	p.CoreID = r.c.hello.Core.ID
	return &ownedStream{Stream: st, c: c}, p, nil
}

// ownedStream is a stream on its own connection; closing it closes the connection.
type ownedStream struct {
	runner.Stream
	c *Client
}

func (s *ownedStream) Close() error {
	_ = s.Stream.Close()
	return s.c.Close()
}

type stream struct{ ch *channel }

func (s *stream) Next(ctx context.Context) ([]byte, *runner.CloseInfo, error) {
	for {
		f, err := s.ch.next(ctx)
		if err != nil {
			return nil, nil, err
		}
		switch f.kind {
		case KindData:
			return f.data, nil, nil
		case KindClose:
			var ci runner.CloseInfo
			_ = json.Unmarshal(f.data, &ci)
			return nil, &ci, nil
		}
	}
}

func (s *stream) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 || cols > 65535 || rows > 65535 {
		return errors.New("hc: invalid size")
	}
	var b [4]byte
	binary.LittleEndian.PutUint16(b[0:2], uint16(cols)) //nolint:gosec // range checked
	binary.LittleEndian.PutUint16(b[2:4], uint16(rows)) //nolint:gosec // range checked
	return s.ch.c.writeFrame(frame{ch: s.ch.id, kind: KindResize, data: b[:]})
}

func (s *stream) Write(p []byte) error {
	return s.ch.c.writeFrame(frame{ch: s.ch.id, kind: KindData, data: p})
}

func (s *stream) Close() error {
	s.ch.close()
	return nil
}

// eventNote is one event line on a framed subscription channel (verified against hc 0.1.16):
//
//	{"jsonrpc":"2.0","method":"event","params":{"type":"finished","seq":37,"terminal":"t11","data":{…}}}
//
// For "created" data is the full Terminal; for "finished" {exit_code, finished_ms, reason, signal}.
type eventNote struct {
	Method string `json:"method"`
	Params struct {
		Type     string          `json:"type"`
		Seq      uint64          `json:"seq"`
		Terminal string          `json:"terminal"`
		Data     json.RawMessage `json:"data"`
	} `json:"params"`
}

// Subscribe streams change events. The first event carries the snapshot. In framed mode the core sends each event
// as one newline-terminated JSON-RPC notification on the subscription channel, flow-controlled with credit.
func (r *Runner) Subscribe(ctx context.Context, labels map[string]string) (<-chan runner.Event, error) {
	params := map[string]any{}
	if len(labels) > 0 {
		params["labels"] = labels
	}
	var out struct {
		Seq       uint64     `json:"seq"`
		Terminals []terminal `json:"terminals"`
		Channel   uint32     `json:"channel"`
	}
	if err := r.c.Call(ctx, "events.subscribe", params, &out); err != nil {
		return nil, mapErr(err)
	}
	ch := r.c.takeChannel(out.Channel)
	events := make(chan runner.Event, 64)
	snap := make([]runner.Process, 0, len(out.Terminals))
	for _, t := range out.Terminals {
		snap = append(snap, r.proc(t))
	}
	go func() {
		defer close(events)
		defer ch.close()
		send := func(ev runner.Event) bool {
			select {
			case events <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !send(runner.Event{Seq: out.Seq, Kind: "snapshot", Snapshot: snap}) {
			return
		}
		var buf []byte
		for {
			f, err := ch.next(ctx)
			if err != nil || f.kind == KindClose {
				return
			}
			if f.kind != KindData {
				continue
			}
			buf = append(buf, f.data...)
			for {
				nl := bytes.IndexByte(buf, '\n')
				if nl < 0 {
					break
				}
				line := buf[:nl]
				buf = buf[nl+1:]
				if ev, ok := r.decodeEvent(line); ok && !send(ev) {
					return
				}
			}
			if len(buf) > maxFrame {
				return // a runaway line: drop the subscription; the consumer re-subscribes and reconciles from a snapshot
			}
		}
	}()
	return events, nil
}

func (r *Runner) decodeEvent(line []byte) (runner.Event, bool) {
	var n eventNote
	if json.Unmarshal(line, &n) != nil || n.Method != "event" || n.Params.Type == "" {
		return runner.Event{}, false
	}
	ev := runner.Event{Seq: n.Params.Seq, Kind: n.Params.Type, TerminalID: n.Params.Terminal, Data: n.Params.Data}
	switch n.Params.Type {
	case "created":
		var t terminal
		if json.Unmarshal(n.Params.Data, &t) == nil {
			p := r.proc(t)
			ev.Process = &p
		}
	case "missed":
		ev.Missed = true
	}
	return ev, true
}

var _ runner.Runner = (*Runner)(nil)
var _ io.Closer = (*Runner)(nil)
