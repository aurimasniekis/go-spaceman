package gitkeys

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var op = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

func newSvc(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	s := storetest.Open(t, storetest.WithClock(clock.NewFake(time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))))
	kr := keyringtest.Load(t, s)
	ctx := context.Background()
	if err := s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return New(s, audit.NewWriter(kr), changefeed.New(nil), kr), s
}

// ADR-0058: a generated key shows only its public half; its private half opens only for a space on its list; names are
// unique and validated; updating, rotating and deleting keep the rules.
func TestGitKeys(t *testing.T) {
	svc, s := newSvc(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, op, Input{Name: "Bad Name"}); err == nil {
		t.Fatal("an invalid name was accepted")
	}
	if _, err := svc.Create(ctx, op, Input{Name: "bot", Spaces: []string{"spc_nope"}}); err == nil {
		t.Fatal("an unknown space was accepted")
	}
	k, err := svc.Create(ctx, op, Input{Name: "bot", Description: "machine user", Spaces: []string{"spc_a", "spc_a"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.PublicKey, "ssh-ed25519 ") || !strings.HasPrefix(k.Fingerprint, "SHA256:") || len(k.Spaces) != 1 || k.AllSpaces {
		t.Fatalf("key %+v", k)
	}
	if _, err := svc.Create(ctx, op, Input{Name: "bot"}); err == nil {
		t.Fatal("a duplicate name was accepted")
	}
	priv, err := svc.Private(ctx, "spc_a", k.ID)
	if err != nil || !strings.Contains(priv, "OPENSSH PRIVATE KEY") {
		t.Fatalf("private for an allowed space: %v", err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(priv))
	if err != nil || ssh.FingerprintSHA256(signer.PublicKey()) != k.Fingerprint {
		t.Fatalf("the private half does not match the public one: %v", err)
	}
	if p, err := svc.Private(ctx, "spc_b", k.ID); err != nil || p != "" {
		t.Fatalf("private for a space not on the list: %q %v", p, err)
	}
	var n int
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM git_ssh_keys WHERE CAST(private_enc AS TEXT) LIKE '%PRIVATE KEY%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the key is not sealed: %d %v", n, err)
	}
	if l, _ := svc.ForSpace(ctx, "spc_b"); len(l) != 0 {
		t.Fatalf("space b sees %d keys", len(l))
	}

	// Every space, then rotation, then a stale version, then deletion.
	k, err = svc.Update(ctx, op, k.ID, k.Version, Input{Name: "github-bot", AllSpaces: true})
	if err != nil || k.Name != "github-bot" || !k.AllSpaces || len(k.Spaces) != 0 {
		t.Fatalf("update: %+v %v", k, err)
	}
	if l, _ := svc.ForSpace(ctx, "spc_b"); len(l) != 1 {
		t.Fatal("space b does not see an every-space key")
	}
	if _, err := svc.Update(ctx, op, k.ID, k.Version-1, Input{Name: "x"}); err == nil {
		t.Fatal("a stale version was accepted")
	}
	r, err := svc.Rotate(ctx, op, k.ID, k.Version)
	if err != nil || r.Fingerprint == k.Fingerprint || r.RotatedAt == nil {
		t.Fatalf("rotate: %+v %v", r, err)
	}
	if l, _ := svc.List(ctx); len(l) != 1 {
		t.Fatalf("list %d", len(l))
	}
	if err := svc.Delete(ctx, op, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, k.ID); err == nil {
		t.Fatal("a deleted key is still there")
	}
	if Fingerprint("not a key") != "" {
		t.Fatal("a fingerprint for nothing")
	}
}
