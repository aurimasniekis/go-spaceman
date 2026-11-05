// Package gitsshtest is an SSH Git host for tests: it serves git-upload-pack for one authorized key, like a hosted
// repository with a read-only deploy key (ADR-0057).
package gitsshtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Server starts a host on 127.0.0.1 that accepts only the authorized key and runs git-upload-pack (fetch only; pushes
// are refused). It returns its address and its known_hosts line; it stops at test cleanup.
func Server(t testing.TB, authorized ssh.PublicKey) (addr, knownHost string) {
	t.Helper()
	return ServerFunc(t, func(k ssh.PublicKey) bool {
		return authorized != nil && string(k.Marshal()) == string(authorized.Marshal())
	})
}

// ServerFunc is Server with the accepted keys decided by allow (for a key generated after the host started).
func ServerFunc(t testing.TB, allow func(ssh.PublicKey) bool) (addr, knownHost string) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostKey, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if allow(k) {
			return nil, nil
		}
		return nil, fmt.Errorf("unknown key")
	}}
	cfg.AddHostKey(hostKey)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c, cfg)
		}
	}()
	return ln.Addr().String(), knownhosts.Line([]string{knownhosts.Normalize(ln.Addr().String())}, hostKey.PublicKey())
}

func serve(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			defer ch.Close()
			for r := range creqs {
				if r.Type != "exec" || len(r.Payload) < 4 {
					_ = r.Reply(false, nil)
					continue
				}
				n := binary.BigEndian.Uint32(r.Payload)
				if int(n) > len(r.Payload)-4 {
					_ = r.Reply(false, nil)
					return
				}
				prog, arg, _ := strings.Cut(string(r.Payload[4:4+n]), " ")
				if prog != "git-upload-pack" {
					_ = r.Reply(false, nil)
					return
				}
				_ = r.Reply(true, nil)
				cmd := exec.CommandContext(context.Background(), "git-upload-pack", strings.Trim(arg, "'")) //nolint:gosec // G204: a test host serving the test's own repository path
				cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
				code := uint32(0)
				if err := cmd.Run(); err != nil {
					code = 1
				}
				st := make([]byte, 4)
				binary.BigEndian.PutUint32(st, code)
				_, _ = ch.SendRequest("exit-status", false, st)
				return
			}
		}()
	}
}

// NewKey returns an Ed25519 private key in OpenSSH PEM form and its public key.
func NewKey(t testing.TB) (pemKey string, pub ssh.PublicKey) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "test")
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.NewSignerFromKey(priv)
	return string(pem.EncodeToMemory(block)), signer.PublicKey()
}
