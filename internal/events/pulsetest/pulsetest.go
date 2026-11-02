// Package pulsetest runs an in-process NATS server shaped like a Pulse deployment for tests: operator (JWT) mode, one
// JetStream-enabled account, the EVENTS stream on evt.> (docs/contracts/pulse.md) and user credentials tagged
// pulse-client:<id>, so the real go-pulse SDK (v0.1.3) works unmodified, including durable subscriptions. It does not
// model Pulse's per-client grants (pulsed issues those); grant-denial behaviour is documented, not simulated.
package pulsetest

import (
	"context"
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

// Start runs a server for the test and creates the EVENTS stream (2-minute dedup window, as Pulse configures it).
func Start(t testing.TB) *Server {
	t.Helper()
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
		t.Fatal(err)
	}
	akp, _ := nkeys.CreateAccount()
	apub, _ := akp.PublicKey()
	ac := jwt.NewAccountClaims(apub)
	ac.Name = "PULSE"
	ac.Limits.JetStreamLimits = jwt.JetStreamLimits{MemoryStorage: -1, DiskStorage: -1, Streams: -1, Consumer: -1}
	ajwt, err := ac.Encode(okp)
	if err != nil {
		t.Fatal(err)
	}
	ojwt, err := oc.Encode(okp)
	if err != nil {
		t.Fatal(err)
	}
	ocl, err := jwt.DecodeOperatorClaims(ojwt)
	if err != nil {
		t.Fatal(err)
	}
	res := &server.MemAccResolver{}
	_ = res.Store(spub, sjwt)
	_ = res.Store(apub, ajwt)
	opts := &server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
		TrustedOperators: []*jwt.OperatorClaims{ocl}, SystemAccount: spub, AccountResolver: res}
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server did not start")
	}
	s := &Server{URL: ns.ClientURL(), ns: ns, akp: akp, t: t}
	s.stop = ns.Shutdown
	t.Cleanup(s.Stop)
	nc := s.Conn("admin")
	defer nc.Close()
	js, _ := jetstream.New(nc)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "EVENTS", Subjects: []string{"evt.>"}, Storage: jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy, Discard: jetstream.DiscardOld, Duplicates: 2 * time.Minute, MaxMsgSize: 1 << 20, MaxConsumers: 256}); err != nil {
		t.Fatal(err)
	}
	return s
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
	ukp, _ := nkeys.CreateUser()
	upub, _ := ukp.PublicKey()
	uc := jwt.NewUserClaims(upub)
	uc.Name = clientID
	uc.Tags.Add("pulse-client:" + clientID)
	ujwt, err := uc.Encode(s.akp)
	if err != nil {
		s.t.Fatal(err)
	}
	seed, _ := ukp.Seed()
	creds, err := jwt.FormatUserConfig(ujwt, seed)
	if err != nil {
		s.t.Fatal(err)
	}
	return creds
}

// Conn is a plain NATS connection as a client (for administration and NATS Core tests).
func (s *Server) Conn(clientID string) *nats.Conn {
	s.t.Helper()
	nc, err := nats.Connect(s.URL, nats.UserJWTAndSeed(jwtAndSeed(s.t, s.Creds(clientID))))
	if err != nil {
		s.t.Fatal(err)
	}
	return nc
}

func jwtAndSeed(t testing.TB, creds []byte) (string, string) {
	tok, err := jwt.ParseDecoratedJWT(creds)
	if err != nil {
		t.Fatal(err)
	}
	kp, err := nkeys.ParseDecoratedNKey(creds)
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := kp.Seed()
	return tok, string(seed)
}
