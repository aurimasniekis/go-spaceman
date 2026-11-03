package apptest

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

// Proxy is a TCP proxy between the agent and central that simulates network faults: Cut resets every connection and
// refuses new ones until Heal; DropUpstream black-holes what the agent sends while central's messages still arrive
// (an acknowledgement lost in transit).
type Proxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	refuse bool
	dropUp atomic.Bool
	// UpBytesDropped counts bytes discarded by DropUpstream.
	UpBytesDropped atomic.Int64
	wg             sync.WaitGroup
}

// NewProxy listens on a loopback port and forwards to target (host:port).
func NewProxy(t testing.TB, target string) *Proxy {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.accept()
	return p
}

// URL is the proxy's http URL.
func (p *Proxy) URL() string { return "http://" + p.ln.Addr().String() }

func (p *Proxy) accept() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		refuse := p.refuse
		p.mu.Unlock()
		if refuse {
			_ = c.Close()
			continue
		}
		up, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		p.conns[c], p.conns[up] = struct{}{}, struct{}{}
		p.mu.Unlock()
		go p.pipe(up, c, true)
		go p.pipe(c, up, false)
	}
}

// pipe copies src to dst; upstream copies honour DropUpstream.
func (p *Proxy) pipe(dst, src net.Conn, upstream bool) {
	defer func() {
		_ = dst.Close()
		_ = src.Close()
		p.mu.Lock()
		delete(p.conns, dst)
		delete(p.conns, src)
		p.mu.Unlock()
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if upstream && p.dropUp.Load() {
				p.UpBytesDropped.Add(int64(n))
			} else if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// DropUpstream starts (true) or stops discarding the agent's bytes. A connection whose bytes were dropped is
// corrupt; Cut it before healing.
func (p *Proxy) DropUpstream(on bool) { p.dropUp.Store(on) }

// Cut resets every proxied connection and refuses new ones until Heal.
func (p *Proxy) Cut() {
	p.mu.Lock()
	p.refuse = true
	for c := range p.conns {
		_ = c.Close()
	}
	p.mu.Unlock()
}

// Heal accepts connections again (and stops dropping upstream bytes).
func (p *Proxy) Heal() {
	p.dropUp.Store(false)
	p.mu.Lock()
	p.refuse = false
	p.mu.Unlock()
}

// Close stops the proxy.
func (p *Proxy) Close() {
	_ = p.ln.Close()
	p.Cut()
	p.wg.Wait()
}
