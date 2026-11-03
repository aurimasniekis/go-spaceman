package hc

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// fakeCore speaks the framed protocol as decoded from hc 0.1.16 (S-3), so the client's framing, channel routing,
// credit and close handling are tested without the binary. The real-binary suite is hc_real_test.go.
type fakeCore struct {
	t       *testing.T
	ln      net.Listener
	credits chan uint32
	resizes chan [2]uint16
	inputs  chan []byte
}

func startFake(t *testing.T, handle func(fc *fakeCore, w io.Writer, req map[string]any)) (*fakeCore, string) {
	t.Helper()
	// Short (unix socket paths are limited, S-0) and unique per process, so concurrent test runs never share a socket.
	sock := filepath.Join("/tmp", fmt.Sprintf("smhc-test-%s-%d.sock", strings.ReplaceAll(t.Name(), "/", "_"), os.Getpid()))
	if len(sock) > MaxSocketPath {
		sock = sock[:MaxSocketPath]
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeCore{t: t, ln: ln, credits: make(chan uint32, 64), resizes: make(chan [2]uint16, 8), inputs: make(chan []byte, 8)}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		if _, err := br.ReadBytes('\n'); err != nil {
			return
		}
		hello, _ := json.Marshal(map[string]any{"id": 0, "jsonrpc": "2.0", "result": map[string]any{"framed": true, "generation": 1, "version": "0.1.16",
			"core": map[string]any{"id": "core1", "name": "fake"}}})
		_, _ = conn.Write(append(hello, '\n'))
		for {
			var hdr [9]byte
			if _, err := io.ReadFull(br, hdr[:]); err != nil {
				return
			}
			ch, kind, n := binary.LittleEndian.Uint32(hdr[:4]), hdr[4], binary.LittleEndian.Uint32(hdr[5:9])
			data := make([]byte, n)
			if _, err := io.ReadFull(br, data); err != nil {
				return
			}
			switch {
			case ch == 0:
				var req map[string]any
				_ = json.Unmarshal(data, &req)
				handle(fc, conn, req)
			case kind == KindCredit:
				fc.credits <- binary.LittleEndian.Uint32(data)
			case kind == KindResize:
				fc.resizes <- [2]uint16{binary.LittleEndian.Uint16(data[:2]), binary.LittleEndian.Uint16(data[2:])}
			case kind == KindData:
				fc.inputs <- data
			}
		}
	}()
	return fc, sock
}

func writeFrame(w io.Writer, ch uint32, kind byte, data []byte) {
	var hdr [9]byte
	binary.LittleEndian.PutUint32(hdr[:4], ch)
	hdr[4] = kind
	binary.LittleEndian.PutUint32(hdr[5:], uint32(len(data))) //nolint:gosec // test data
	_, _ = w.Write(append(hdr[:], data...))
}

func reply(w io.Writer, id any, result any) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	writeFrame(w, 0, KindData, b)
}

func TestClientFramingAttachCreditResizeClose(t *testing.T) {
	_, sock := startFake(t, func(_ *fakeCore, w io.Writer, req map[string]any) {
		switch req["method"] {
		case "terminal.attach":
			// Response then frames on channel 7 immediately (the reader must have registered the channel).
			reply(w, req["id"], map[string]any{"channel": 7, "terminal": map[string]any{"address": "t1", "name": "run-a", "state": "live", "created_ms": 1000, "cols": 200, "rows": 120}})
			writeFrame(w, 7, KindData, []byte("\x1b[Hfirst paint"))
			writeFrame(w, 7, KindData, []byte("tick\r\n"))
			writeFrame(w, 7, KindClose, []byte(`{"exit_code":3,"finished_ms":2000,"reason":"exited","signal":null}`))
		case "terminal.run":
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req["id"], "error": map[string]any{"code": -32000, "message": "name is in use", "data": map[string]any{"kind": "name_taken"}}})
			writeFrame(w, 0, KindData, b)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Hello().Core.ID != "core1" {
		t.Fatalf("hello %+v", c.Hello())
	}
	r := New(c)
	st, p, err := r.Attach(ctx, "t1", true, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "t1" || p.CoreID != "core1" || p.Cols != 200 {
		t.Fatalf("process %+v", p)
	}
	var got []string
	var ci *runner.CloseInfo
	for ci == nil {
		data, closed, err := st.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if closed != nil {
			ci = closed
			break
		}
		got = append(got, string(data))
	}
	if strings.Join(got, "|") != "\x1b[Hfirst paint|tick\r\n" || ci.ExitCode == nil || *ci.ExitCode != 3 || ci.Reason != "exited" {
		t.Fatalf("stream %q close %+v", got, ci)
	}
	if _, err := r.Start(ctx, runner.Spec{Name: "run-a", Argv: []string{"true"}}); !errors.Is(err, runner.ErrNameTaken) {
		t.Fatalf("name_taken not mapped: %v", err)
	}
}

func TestClientCreditsAndInput(t *testing.T) {
	fc, sock := startFake(t, func(_ *fakeCore, w io.Writer, req map[string]any) {
		if req["method"] == "terminal.attach" {
			reply(w, req["id"], map[string]any{"channel": 3, "terminal": map[string]any{"address": "t2", "state": "live"}})
			writeFrame(w, 3, KindData, []byte("12345"))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st, _, err := New(c).Attach(ctx, "t2", false, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-fc.credits; got != 5 {
		t.Fatalf("credit %d, want 5 (bytes consumed)", got)
	}
	if err := st.Resize(111, 33); err != nil {
		t.Fatal(err)
	}
	if got := <-fc.resizes; got != [2]uint16{111, 33} {
		t.Fatalf("resize %v", got)
	}
	if err := st.Write([]byte("hi\r")); err != nil {
		t.Fatal(err)
	}
	if got := <-fc.inputs; string(got) != "hi\r" {
		t.Fatalf("input %q", got)
	}
	if err := st.Resize(0, 1); err == nil {
		t.Fatal("invalid size accepted")
	}
}

// SM-RUN-001: stop, kill and safe input map to herdr-core's terminal.interrupt, terminal.kill and terminal.send
// (docs/contracts/herdr-core.md); idle-gated input sends `if: idle`, and a refused send is ErrBusy.
func TestClientInterruptKillSend(t *testing.T) {
	calls := make(chan map[string]any, 8)
	_, sock := startFake(t, func(_ *fakeCore, w io.Writer, req map[string]any) {
		calls <- req
		switch req["method"] {
		case "terminal.interrupt":
			reply(w, req["id"], map[string]any{"outcome": "stopped"})
		case "terminal.kill":
			reply(w, req["id"], map[string]any{"address": "t3", "state": "finished", "reason": "killed", "created_ms": 1})
		case "terminal.send":
			if p, _ := req["params"].(map[string]any); p["if"] == "idle" {
				b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req["id"], "error": map[string]any{"code": -32000, "message": "busy",
					"data": map[string]any{"kind": "status_mismatch"}}})
				writeFrame(w, 0, KindData, b)
				return
			}
			reply(w, req["id"], map[string]any{"turn": 2, "started": true, "status": "working"})
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := New(c)
	params := func(method string) map[string]any {
		t.Helper()
		req := <-calls
		if req["method"] != method {
			t.Fatalf("method %v, want %s", req["method"], method)
		}
		p, _ := req["params"].(map[string]any)
		if p["terminal"] != "t3" {
			t.Fatalf("%s params %v", method, p)
		}
		return p
	}
	if out, err := r.Interrupt(ctx, "t3"); err != nil || out != "stopped" {
		t.Fatalf("interrupt: %q %v", out, err)
	}
	params("terminal.interrupt")
	p, err := r.Kill(ctx, "t3")
	if err != nil || p.State != runner.StateFinished || p.Reason != "killed" {
		t.Fatalf("kill: %+v %v", p, err)
	}
	params("terminal.kill")
	if err := r.Send(ctx, "t3", "you have 1 message", false); err != nil {
		t.Fatal(err)
	}
	if sp := params("terminal.send"); sp["text"] != "you have 1 message" || sp["if"] != nil {
		t.Fatalf("send params %v", sp)
	}
	if err := r.Send(ctx, "t3", "notice", true); !errors.Is(err, runner.ErrBusy) {
		t.Fatalf("idle-gated send to a busy program: %v", err)
	}
	if sp := params("terminal.send"); sp["if"] != "idle" {
		t.Fatalf("idle-gated send params %v", sp)
	}
}

func TestEventDecoding(t *testing.T) {
	r := &Runner{c: &Client{hello: Hello{Core: Core{ID: "core1"}}}}
	ev, ok := r.decodeEvent([]byte(`{"jsonrpc":"2.0","method":"event","params":{"data":{"exit_code":2,"finished_ms":1,"reason":"exited","signal":null},"seq":37,"terminal":"t11","type":"finished"}}`))
	if !ok || ev.Kind != "finished" || ev.Seq != 37 || ev.TerminalID != "t11" || !strings.Contains(string(ev.Data), `"exit_code":2`) {
		t.Fatalf("finished event %+v", ev)
	}
	ev, ok = r.decodeEvent([]byte(`{"jsonrpc":"2.0","method":"event","params":{"data":{"address":"t11","name":"x","state":"live","created_ms":5,"labels":{"a":"b"}},"seq":35,"terminal":"t11","type":"created"}}`))
	if !ok || ev.Process == nil || ev.Process.Labels["a"] != "b" || ev.Process.CoreID != "core1" {
		t.Fatalf("created event %+v", ev)
	}
	if _, ok := r.decodeEvent([]byte(`{"jsonrpc":"2.0","method":"heartbeat","params":{}}`)); ok {
		t.Fatal("non-event decoded as event")
	}
	if _, ok := r.decodeEvent([]byte(`not json`)); ok {
		t.Fatal("garbage decoded")
	}
}

func TestCoreOptionsValidation(t *testing.T) {
	for _, o := range []CoreOptions{
		{Session: "default", Socket: "/tmp/x.sock", Home: "/tmp/h"},
		{Session: "", Socket: "/tmp/x.sock", Home: "/tmp/h"},
		{Session: "sm", Socket: "rel.sock", Home: "/tmp/h"},
		{Session: "sm", Socket: "/" + strings.Repeat("x", MaxSocketPath), Home: "/tmp/h"},
		{Session: "sm", Socket: "/tmp/x.sock", Home: "rel"},
	} {
		if o.validate() == nil {
			t.Errorf("accepted %+v", o)
		}
	}
	env := CoreOptions{Session: "sm", Socket: "/tmp/x.sock", Home: "/tmp/h"}.env()
	for _, kv := range env {
		if strings.HasPrefix(kv, "HC_TERMINAL") || strings.HasPrefix(kv, "HC_SCOPE") {
			t.Fatalf("inherited %s leaks into the isolated core", kv)
		}
	}
}
