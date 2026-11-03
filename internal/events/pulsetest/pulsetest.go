// Package pulsetest runs an in-process NATS server shaped like a Pulse deployment for tests: operator (JWT) mode, one
// JetStream-enabled account, the EVENTS stream on evt.> (docs/contracts/pulse.md) and user credentials tagged
// pulse-client:<id>, so the real go-pulse SDK (v0.1.3) works unmodified, including durable subscriptions. It does not
// model Pulse's per-client grants (pulsed issues those); grant-denial behaviour is documented, not simulated.
package pulsetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

// Server is a running test server.
type Server struct {
	URL  string
	ns   *server.Server
	akp  nkeys.KeyPair
	t    testing.TB
	stop func()
}

// Start runs a server for a test (random port, removed with the test) and creates the EVENTS stream.
func Start(t testing.TB) *Server {
	t.Helper()
	s, err := New(t.TempDir(), -1)
	if err != nil {
		t.Fatal(err)
	}
	s.t = t
	t.Cleanup(s.Stop)
	return s
}

// New runs a server storing JetStream data in dir on port (-1: random) and creates the EVENTS stream with Pulse's
// settings (2-minute dedup window, 1 MiB messages). For tests and the local development stack only.
func New(dir string, port int) (*Server, error) {
	okp, _ := nkeys.CreateOperator()
	opub, _ := okp.PublicKey()
	oc := jwt.NewOperatorClaims(opub)
	oc.Name = "pulsetest"
	skp, _ := nkeys.CreateAccount()
	spub, _ := skp.PublicKey()
	sc := jwt.NewAccountClaims(spub)
	sc.Name = "SYS"
	sjwt, err := sc.Encode(okp)
	if err != nil {
		return nil, err
	}
	akp, _ := nkeys.CreateAccount()
	apub, _ := akp.PublicKey()
	ac := jwt.NewAccountClaims(apub)
	ac.Name = "PULSE"
	ac.Limits.JetStreamLimits = jwt.JetStreamLimits{MemoryStorage: -1, DiskStorage: -1, Streams: -1, Consumer: -1}
	ajwt, err := ac.Encode(okp)
	if err != nil {
		return nil, err
	}
	ojwt, err := oc.Encode(okp)
	if err != nil {
		return nil, err
	}
	ocl, err := jwt.DecodeOperatorClaims(ojwt)
	if err != nil {
		return nil, err
	}
	res := &server.MemAccResolver{}
	_ = res.Store(spub, sjwt)
	_ = res.Store(apub, ajwt)
	opts := &server.Options{Host: "127.0.0.1", Port: port, JetStream: true, StoreDir: dir, NoLog: true, NoSigs: true,
		TrustedOperators: []*jwt.OperatorClaims{ocl}, SystemAccount: spub, AccountResolver: res}
	ns, err := server.NewServer(opts)
	if err != nil {
		return nil, err
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		return nil, errors.New("nats server did not start")
	}
	s := &Server{URL: ns.ClientURL(), ns: ns, akp: akp}
	s.stop = ns.Shutdown
	creds, err := s.CredsFor("admin")
	if err != nil {
		s.Stop()
		return nil, err
	}
	nc, err := connect(s.URL, creds)
	if err != nil {
		s.Stop()
		return nil, err
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "EVENTS", Subjects: []string{"evt.>"}, Storage: jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy, Discard: jetstream.DiscardOld, Duplicates: 2 * time.Minute, MaxMsgSize: 1 << 20, MaxConsumers: 256}); err != nil {
		s.Stop()
		return nil, err
	}
	return s, nil
}

// Stop shuts the server down (idempotent). Restart with a new Start; durable state does not survive.
func (s *Server) Stop() {
	if s.stop != nil {
		s.stop()
		s.ns.WaitForShutdown()
		s.stop = nil
	}
}

// Creds returns .creds content for a Pulse client ID (lowercase, as Pulse issues it).
func (s *Server) Creds(clientID string) []byte {
	s.t.Helper()
	c, err := s.CredsFor(clientID)
	if err != nil {
		s.t.Fatal(err)
	}
	return c
}

// CredsFor returns .creds content for a Pulse client ID: a user of the PULSE account tagged pulse-client:<id>.
func (s *Server) CredsFor(clientID string) ([]byte, error) { return s.credsFor(clientID, nil) }

// SubscribeOnlyCreds returns .creds content for a client that may subscribe (core NATS) only to the given subjects,
// as a Pulse subscribe grant allows; for testing refused subscriptions. Publishing stays unrestricted.
func (s *Server) SubscribeOnlyCreds(clientID string, subjects ...string) []byte {
	s.t.Helper()
	c, err := s.credsFor(clientID, subjects)
	if err != nil {
		s.t.Fatal(err)
	}
	return c
}

func (s *Server) credsFor(clientID string, subscribe []string) ([]byte, error) {
	ukp, _ := nkeys.CreateUser()
	upub, _ := ukp.PublicKey()
	uc := jwt.NewUserClaims(upub)
	uc.Name = clientID
	uc.Tags.Add("pulse-client:" + clientID)
	if subscribe != nil {
		uc.Sub.Allow.Add(subscribe...)
	}
	ujwt, err := uc.Encode(s.akp)
	if err != nil {
		return nil, err
	}
	seed, _ := ukp.Seed()
	return jwt.FormatUserConfig(ujwt, seed)
}

// Conn is a plain NATS connection as a client (for administration and NATS Core tests).
func (s *Server) Conn(clientID string) *nats.Conn {
	s.t.Helper()
	nc, err := connect(s.URL, s.Creds(clientID))
	if err != nil {
		s.t.Fatal(err)
	}
	return nc
}

func connect(url string, creds []byte) (*nats.Conn, error) {
	tok, err := jwt.ParseDecoratedJWT(creds)
	if err != nil {
		return nil, err
	}
	kp, err := nkeys.ParseDecoratedNKey(creds)
	if err != nil {
		return nil, err
	}
	seed, _ := kp.Seed()
	return nats.Connect(url, nats.UserJWTAndSeed(tok, string(seed)))
}
