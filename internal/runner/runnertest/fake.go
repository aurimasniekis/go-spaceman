// Package runnertest provides an in-memory runner.Runner for simulated tests of the node agent. It models the
// herdr-core behaviors SpaceMan relies on (verified separately by internal/runner/hc's real suite): unique live
// names, label lookup, parent/child lifetime, observe streams that end with a close, and change events.
// It is a test double — never an integration.
package runnertest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// Fake is an in-memory runner.
type Fake struct {
	mu     sync.Mutex
	next   int
	procs  map[string]*fakeProc
	subs   []chan runner.Event
	seq    uint64
	Starts int // number of successful Start calls (duplicate detection in tests)
	// Specs records every successful Start's spec, so tests can check what reached the runner (and herdr-core state).
	Specs   []runner.Spec
	CoreID  string
	BlockIO bool
	// AfterStart, when set, runs after a successful Start returns its process to the runner (outside the lock). Tests
	// use it to crash the agent between starting a program and recording that it started.
	AfterStart func(runner.Process)
}

type fakeProc struct {
	p      runner.Process
	out    chan []byte
	done   chan struct{}
	inputs []string
	// typed is what takeover streams wrote; attached counts open takeover streams.
	typed    []byte
	attached int
}

// New returns an empty fake runner.
func New() *Fake { return &Fake{procs: map[string]*fakeProc{}, CoreID: "fakecore"} }

func (f *Fake) emit(kind string, p runner.Process) {
	f.seq++
	ev := runner.Event{Seq: f.seq, Kind: kind, TerminalID: p.ID, Process: &p}
	for _, s := range f.subs {
		select {
		case s <- ev:
		default:
		}
	}
}

// Start implements runner.Runner.
func (f *Fake) Start(ctx context.Context, s runner.Spec) (*runner.Process, error) {
	p, err := f.start(ctx, s)
	if err == nil && f.AfterStart != nil {
		f.AfterStart(*p)
	}
	return p, err
}

func (f *Fake) start(_ context.Context, s runner.Spec) (*runner.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fp := range f.procs {
		if fp.p.Name == s.Name && fp.p.State == runner.StateLive {
			return nil, runner.ErrNameTaken
		}
	}
	if s.Parent != "" {
		if par, ok := f.procs[s.Parent]; !ok || par.p.State != runner.StateLive {
			return nil, fmt.Errorf("parent %s is not live", s.Parent)
		}
	}
	f.next++
	f.Starts++
	f.Specs = append(f.Specs, s)
	labels := map[string]string{}
	for k, v := range s.Labels {
		labels[k] = v
	}
	fp := &fakeProc{p: runner.Process{ID: fmt.Sprintf("t%d", f.next), Name: s.Name, CoreID: f.CoreID, PID: 1000 + f.next, State: runner.StateLive,
		Created: time.Now().UTC(), Labels: labels, Cols: 200, Rows: 120, Parent: s.Parent, Argv: s.Argv, Status: runner.Status{State: "working", Reported: true}},
		out: make(chan []byte, 64), done: make(chan struct{})}
	f.procs[fp.p.ID] = fp
	fp.out <- []byte("\x1b[H" + strings.Join(s.Argv, " ") + "\r\n")
	f.emit("created", fp.p)
	p := fp.p
	return &p, nil
}

// Exit ends a process as if its program exited with code (and ends its children, like herdr-core).
func (f *Fake) Exit(id string, code int, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exitLocked(id, code, reason)
}

func (f *Fake) exitLocked(id string, code int, reason string) {
	fp, ok := f.procs[id]
	if !ok || fp.p.State != runner.StateLive {
		return
	}
	now := time.Now().UTC()
	c := code
	fp.p.State, fp.p.ExitCode, fp.p.Reason, fp.p.Finished = runner.StateFinished, &c, reason, &now
	close(fp.done)
	f.emit("finished", fp.p)
	for cid, child := range f.procs {
		if child.p.Parent == id && child.p.State == runner.StateLive {
			f.exitLocked(cid, 129, "killed")
		}
	}
}

// Get implements runner.Runner.
func (f *Fake) Get(_ context.Context, id string) (*runner.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fp, ok := f.procs[id]
	if !ok {
		return nil, runner.ErrNotFound
	}
	p := fp.p
	return &p, nil
}

// List implements runner.Runner.
func (f *Fake) List(_ context.Context, labels map[string]string, all bool) ([]runner.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runner.Process
outer:
	for _, fp := range f.procs {
		if !all && fp.p.State != runner.StateLive {
			continue
		}
		for k, v := range labels {
			if fp.p.Labels[k] != v {
				continue outer
			}
		}
		out = append(out, fp.p)
	}
	return out, nil
}

// Kill implements runner.Runner.
func (f *Fake) Kill(ctx context.Context, id string) (*runner.Process, error) {
	f.Exit(id, 129, "killed")
	return f.Get(ctx, id)
}

// Interrupt implements runner.Runner: the program exits as a status-reporting agent would on Esc.
func (f *Fake) Interrupt(_ context.Context, id string) (string, error) {
	f.Exit(id, 130, "exited")
	return "exited", nil
}

// Send implements runner.Runner.
func (f *Fake) Send(_ context.Context, id, text string, ifIdle bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fp, ok := f.procs[id]
	if !ok {
		return runner.ErrNotFound
	}
	if ifIdle && fp.p.Status.State == "working" {
		return runner.ErrBusy
	}
	fp.inputs = append(fp.inputs, text)
	return nil
}

// SetStatus changes a process's reported status.
func (f *Fake) SetStatus(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fp, ok := f.procs[id]; ok {
		fp.p.Status.State = state
		f.emit("status", fp.p)
	}
}

// Inputs returns the input sent to a process.
func (f *Fake) Inputs(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fp, ok := f.procs[id]; ok {
		return append([]string(nil), fp.inputs...)
	}
	return nil
}

// Remove implements runner.Runner.
func (f *Fake) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.procs, id)
	return nil
}

// Attach implements runner.Runner.
func (f *Fake) Attach(_ context.Context, id string, _ bool, _, _ int) (runner.Stream, *runner.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fp, ok := f.procs[id]
	if !ok {
		return nil, nil, runner.ErrNotFound
	}
	if fp.p.State != runner.StateLive {
		return nil, nil, runner.ErrFinished
	}
	p := fp.p
	return &stream{f: f, fp: fp}, &p, nil
}

type stream struct {
	f  *Fake
	fp *fakeProc
}

func (s *stream) Next(ctx context.Context) ([]byte, *runner.CloseInfo, error) {
	select {
	case d := <-s.fp.out:
		return d, nil, nil
	case <-s.fp.done:
		s.f.mu.Lock()
		p := s.fp.p
		s.f.mu.Unlock()
		return nil, &runner.CloseInfo{ExitCode: p.ExitCode, Reason: p.Reason}, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (s *stream) Resize(int, int) error { return nil }
func (s *stream) Write([]byte) error    { return nil }
func (s *stream) Close() error          { return nil }

// Takeover implements runner.Runner: an interactive stream whose writes are recorded as typed bytes. It carries no
// output (the recorder's observe stream gets that) and ends when the program ends.
func (f *Fake) Takeover(_ context.Context, id string) (runner.Stream, *runner.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fp, ok := f.procs[id]
	if !ok {
		return nil, nil, runner.ErrNotFound
	}
	if fp.p.State != runner.StateLive {
		return nil, nil, runner.ErrFinished
	}
	fp.attached++
	p := fp.p
	return &takeoverStream{f: f, fp: fp, closed: make(chan struct{})}, &p, nil
}

type takeoverStream struct {
	f      *Fake
	fp     *fakeProc
	once   sync.Once
	closed chan struct{}
}

func (s *takeoverStream) Next(ctx context.Context) ([]byte, *runner.CloseInfo, error) {
	select {
	case <-s.fp.done:
		s.f.mu.Lock()
		p := s.fp.p
		s.f.mu.Unlock()
		return nil, &runner.CloseInfo{ExitCode: p.ExitCode, Reason: p.Reason}, nil
	case <-s.closed:
		return nil, nil, context.Canceled
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (s *takeoverStream) Resize(int, int) error { return nil }

func (s *takeoverStream) Write(p []byte) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	select {
	case <-s.closed:
		return fmt.Errorf("takeover stream closed")
	default:
	}
	if s.fp.p.State != runner.StateLive {
		return runner.ErrFinished
	}
	s.fp.typed = append(s.fp.typed, p...)
	return nil
}

func (s *takeoverStream) Close() error {
	s.once.Do(func() {
		s.f.mu.Lock()
		s.fp.attached--
		s.f.mu.Unlock()
		close(s.closed)
	})
	return nil
}

// Typed returns the bytes takeover streams typed into a process.
func (f *Fake) Typed(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fp, ok := f.procs[id]; ok {
		return string(fp.typed)
	}
	return ""
}

// TakeoverAttached returns how many takeover streams are open on a process.
func (f *Fake) TakeoverAttached(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fp, ok := f.procs[id]; ok {
		return fp.attached
	}
	return 0
}

// Output writes program output to a live process.
func (f *Fake) Output(id string, data string) {
	f.mu.Lock()
	fp, ok := f.procs[id]
	f.mu.Unlock()
	if ok {
		select {
		case fp.out <- []byte(data):
		default:
		}
	}
}

// Subscribe implements runner.Runner.
func (f *Fake) Subscribe(ctx context.Context, _ map[string]string) (<-chan runner.Event, error) {
	ch := make(chan runner.Event, 256)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	ch <- runner.Event{Kind: "snapshot"}
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		for i, s := range f.subs {
			if s == ch {
				f.subs = append(f.subs[:i], f.subs[i+1:]...)
				break
			}
		}
		f.mu.Unlock()
		close(ch)
	}()
	return ch, nil
}

// Close implements runner.Runner.
func (f *Fake) Close() error { return nil }

// StartCount returns Starts under the lock.
func (f *Fake) StartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Starts
}

// LiveByLabel returns live processes with a label value.
func (f *Fake) LiveByLabel(k, v string) []runner.Process {
	ps, _ := f.List(context.Background(), map[string]string{k: v}, false)
	return ps
}

var _ runner.Runner = (*Fake)(nil)
