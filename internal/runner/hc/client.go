// Package hc implements runner.Runner on herdr-core (`hc` 0.1.16) using its JSON-RPC protocol over the core's unix
// socket in framed mode (docs/contracts/herdr-core.md, spikes S-1..S-3).
//
// Wire format after `hello{framed:true}` (decoded from real traffic, S-3): every frame is
//
//	channel u32 LE | kind u8 | length u32 LE | payload
//
// Channel 0 carries JSON-RPC (kind 0). terminal.attach and events.subscribe return a channel on which kind 0 is DATA
// (output bytes, or JSON lines for events), kind 1 CREDIT (u32 LE bytes consumed; client to server), kind 2 CLOSE
// (JSON end information) and kind 3 RESIZE (u16 LE cols, u16 LE rows; client to server).
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
	"sync"
	"sync/atomic"
	"time"
)

var timeZero time.Time

// Frame kinds.
const (
	KindData   byte = 0
	KindCredit byte = 1
	KindClose  byte = 2
	KindResize byte = 3
)

// Generation is the protocol generation this client speaks.
const Generation = 1

// maxFrame bounds a single frame (the core never sends more than its documented 8 MB exec output in one frame).
const maxFrame = 16 << 20

// channelBuffer bounds queued frames per channel: the reader never blocks on a slow consumer (it closes the channel
// as overflowed instead, and the consumer re-attaches and records a gap).
const channelBuffer = 1024

// RPCError is a JSON-RPC error from the core; Kind is herdr-core's stable data.kind.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Kind string `json:"kind"`
		Hint string `json:"hint"`
	} `json:"data"`
}

func (e *RPCError) Error() string {
	if e.Data.Kind != "" {
		return fmt.Sprintf("hc %s: %s", e.Data.Kind, e.Message)
	}
	return "hc: " + e.Message
}

// Hello is the core's handshake reply.
type Hello struct {
	Generation int      `json:"generation"`
	Version    string   `json:"version"`
	Core       Core     `json:"core"`
	Framed     bool     `json:"framed"`
	Methods    []string `json:"methods"`
	Features   []string `json:"features"`
}

// Core identifies a herdr-core instance.
type Core struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type frame struct {
	ch   uint32
	kind byte
	data []byte
}

// Client is one framed connection to a herdr-core core. It is safe for concurrent use.
type Client struct {
	conn  net.Conn
	hello Hello

	wmu sync.Mutex
	w   *bufio.Writer

	mu       sync.Mutex
	nextID   int64
	pending  map[int64]chan rpcResponse
	channels map[uint32]*channel
	err      error
	done     chan struct{}
	closed   atomic.Bool
}

type rpcResponse struct {
	result json.RawMessage
	err    error
}

// Dial connects to the core socket and completes the framed handshake.
func Dial(ctx context.Context, socket string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("hc dial %s: %w", socket, err)
	}
	c := &Client{conn: conn, w: bufio.NewWriter(conn), pending: map[int64]chan rpcResponse{}, channels: map[uint32]*channel{},
		done: make(chan struct{}), nextID: 1}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	hello, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "hello", "params": map[string]any{"framed": true, "generation": Generation}})
	if _, err := conn.Write(append(hello, '\n')); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReaderSize(conn, 64<<10)
	line, err := br.ReadBytes('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("hc hello: %w", err)
	}
	var resp struct {
		Result Hello     `json:"result"`
		Error  *RPCError `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("hc hello: %w", err)
	}
	if resp.Error != nil {
		_ = conn.Close()
		return nil, resp.Error
	}
	if !resp.Result.Framed || resp.Result.Generation != Generation {
		_ = conn.Close()
		return nil, fmt.Errorf("hc hello: unsupported core (framed=%v generation=%d)", resp.Result.Framed, resp.Result.Generation)
	}
	_ = conn.SetDeadline(timeZero)
	c.hello = resp.Result
	go c.readLoop(br)
	return c, nil
}

// Hello returns the handshake information (core identity, version, methods).
func (c *Client) Hello() Hello { return c.hello }

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns why the connection ended.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection.
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	return c.conn.Close()
}

func (c *Client) writeFrame(f frame) error {
	if len(f.data) > maxFrame {
		return errors.New("hc: frame too large")
	}
	var hdr [9]byte
	binary.LittleEndian.PutUint32(hdr[0:4], f.ch)
	hdr[4] = f.kind
	binary.LittleEndian.PutUint32(hdr[5:9], uint32(len(f.data))) //nolint:gosec // bounded by maxFrame
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := c.w.Write(f.data); err != nil {
		return err
	}
	return c.w.Flush()
}

func (c *Client) readLoop(r *bufio.Reader) {
	var err error
	for {
		var hdr [9]byte
		if _, err = io.ReadFull(r, hdr[:]); err != nil {
			break
		}
		ch := binary.LittleEndian.Uint32(hdr[0:4])
		kind := hdr[4]
		n := binary.LittleEndian.Uint32(hdr[5:9])
		if n > maxFrame {
			err = fmt.Errorf("hc: frame of %d bytes exceeds limit", n)
			break
		}
		data := make([]byte, n)
		if _, err = io.ReadFull(r, data); err != nil {
			break
		}
		if ch == 0 {
			c.dispatchRPC(data)
			continue
		}
		c.mu.Lock()
		chn := c.channels[ch]
		c.mu.Unlock()
		if chn != nil {
			chn.deliver(frame{ch: ch, kind: kind, data: data})
		}
	}
	c.mu.Lock()
	if c.err == nil {
		if c.closed.Load() {
			c.err = net.ErrClosed
		} else {
			c.err = fmt.Errorf("hc connection lost: %w", err)
		}
	}
	for id, p := range c.pending {
		p <- rpcResponse{err: c.err}
		delete(c.pending, id)
	}
	for id, chn := range c.channels {
		chn.fail(c.err)
		delete(c.channels, id)
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *Client) dispatchRPC(data []byte) {
	var msg struct {
		ID     *int64          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.ID == nil {
		return // notifications on channel 0 are not used in framed mode
	}
	// A result naming a channel (attach, events.subscribe) registers it here, in the reader, before the next frame is
	// read: the core may send the channel's first frames immediately after the response.
	var named struct {
		Channel uint32 `json:"channel"`
	}
	if msg.Error == nil && json.Unmarshal(msg.Result, &named) == nil && named.Channel != 0 {
		c.mu.Lock()
		if c.channels[named.Channel] == nil {
			c.channels[named.Channel] = &channel{c: c, id: named.Channel, frames: make(chan frame, channelBuffer)}
		}
		c.mu.Unlock()
	}
	c.mu.Lock()
	p := c.pending[*msg.ID]
	delete(c.pending, *msg.ID)
	c.mu.Unlock()
	if p == nil {
		return
	}
	if msg.Error != nil {
		p <- rpcResponse{err: msg.Error}
		return
	}
	p <- rpcResponse{result: msg.Result}
}

// Call performs one JSON-RPC request and decodes the result into out (when non-nil).
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	raw, err := c.call(ctx, method, params)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	resp := make(chan rpcResponse, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	id := c.nextID
	c.nextID++
	c.pending[id] = resp
	c.mu.Unlock()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if err := c.writeFrame(frame{ch: 0, kind: KindData, data: body}); err != nil {
		return nil, err
	}
	select {
	case r := <-resp:
		return r.result, r.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// channel is a stream on the connection (attach output or event lines).
type channel struct {
	c      *Client
	id     uint32
	frames chan frame
	mu     sync.Mutex
	err    error
	over   bool
	closed bool
}

// takeChannel returns the channel the reader registered for a response (see dispatchRPC).
func (c *Client) takeChannel(id uint32) *channel {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.channels[id]
	if ch == nil {
		ch = &channel{c: c, id: id, frames: make(chan frame, channelBuffer)}
		c.channels[id] = ch
	}
	return ch
}

func (ch *channel) deliver(f frame) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.closed || ch.over {
		return
	}
	select {
	case ch.frames <- f:
	default:
		// The consumer fell too far behind: stop delivering (never block the shared reader). The consumer sees
		// ErrOverflow, detaches and re-attaches, recording a gap.
		ch.over = true
		close(ch.frames)
	}
	if f.kind == KindClose && !ch.over {
		ch.closed = true
		close(ch.frames)
	}
}

func (ch *channel) fail(err error) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if !ch.closed && !ch.over {
		ch.err = err
		ch.closed = true
		close(ch.frames)
	}
}

// ErrOverflow means a consumer fell behind and the stream was cut; re-attach (the new stream starts with a repaint).
var ErrOverflow = errors.New("hc: stream consumer fell behind; re-attach")

func (ch *channel) next(ctx context.Context) (frame, error) {
	select {
	case f, ok := <-ch.frames:
		if !ok {
			ch.mu.Lock()
			defer ch.mu.Unlock()
			switch {
			case ch.over:
				return frame{}, ErrOverflow
			case ch.err != nil:
				return frame{}, ch.err
			}
			return frame{}, io.EOF
		}
		if f.kind == KindData && len(f.data) > 0 {
			var credit [4]byte
			binary.LittleEndian.PutUint32(credit[:], uint32(len(f.data))) //nolint:gosec // bounded by maxFrame
			_ = ch.c.writeFrame(frame{ch: ch.id, kind: KindCredit, data: credit[:]})
		}
		return f, nil
	case <-ctx.Done():
		return frame{}, ctx.Err()
	}
}

func (ch *channel) close() {
	ch.c.mu.Lock()
	delete(ch.c.channels, ch.id)
	ch.c.mu.Unlock()
	ch.fail(io.EOF)
}
