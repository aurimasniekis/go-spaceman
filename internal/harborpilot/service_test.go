package harborpilot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hpfake"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

func newSvc(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	s := storetest.Open(t)
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
	return New(s, audit.NewWriter(kr), changefeed.New(nil), kr, Options{Timeout: 5 * time.Second}), s
}

func code(t *testing.T, err error, want apperr.Code) {
	t.Helper()
	_ = appErr(t, err, want)
}

func appErr(t *testing.T, err error, want apperr.Code) *apperr.Error {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != want {
		t.Fatalf("error %v, want %s", err, want)
	}
	return ae
}

func fieldMsg(t *testing.T, err error, field, want string) {
	t.Helper()
	ae := appErr(t, err, apperr.CodeInvalid)
	for _, f := range ae.Fields {
		if f.Field == field && strings.Contains(f.Message, want) {
			return
		}
	}
	t.Fatalf("fields %+v, want %s ~ %q", ae.Fields, field, want)
}

func connect(t *testing.T, svc *Service, f *hpfake.HP) *Connection {
	t.Helper()
	c, err := svc.PutConnection(context.Background(), admin, 0, ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true, ParentToken: f.Parent, ServiceToken: f.Service})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// SM-GW-001, ADR-0060 decision 2: the tokens are sealed and write-only. A canary token is in no answer, audit entry
// or change-feed row, and is not stored in plain text; it opens only for the row it was sealed for.
func TestConnectionTokensAreSealedAndWriteOnly(t *testing.T) {
	svc, s := newSvc(t)
	f := hpfake.New(t)
	ctx := context.Background()
	c := connect(t, svc, f)
	if !c.HasParentToken || !c.HasServiceToken || c.ParentTokenVersion != 1 || c.ServiceTokenVersion != 1 || c.BindingID != "bnd_fake" || c.Version != 1 {
		t.Fatalf("connection %+v", c)
	}
	if got := f.Unrevoked(); len(got) != 0 {
		t.Fatalf("validation credentials left unrevoked: %v", got)
	}
	out, _ := json.Marshal(c)
	got, _ := svc.GetConnection(ctx)
	out2, _ := json.Marshal(got)
	for _, b := range [][]byte{out, out2} {
		if strings.Contains(string(b), f.Parent) || strings.Contains(string(b), f.Service) || strings.Contains(string(b), "hpk_parent") {
			t.Fatalf("a token is in the answer: %s", b)
		}
	}
	for _, q := range []string{
		`SELECT count(*) FROM audit_events WHERE details_json LIKE '%hpk_parent%' OR details_json LIKE '%hpa_service%'`,
		`SELECT count(*) FROM changes WHERE resource_id LIKE '%parent_ppp%' OR resource_id LIKE '%service_sss%' OR action LIKE '%ppp%'`,
		`SELECT count(*) FROM harbor_pilot_connection WHERE CAST(parent_token_enc AS TEXT) LIKE '%hpk_parent%' OR CAST(service_token_enc AS TEXT) LIKE '%hpa_service%'`,
	} {
		var n int
		if err := s.ReadDB().QueryRowContext(ctx, q).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d %v", q, n, err)
		}
	}
	var n int
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE action = 'harbor_pilot.connection.created'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit: %d %v", n, err)
	}
	if p, err := svc.ParentToken(ctx); err != nil || p != f.Parent {
		t.Fatalf("parent %v", err)
	}
	if sp, err := svc.ServiceToken(ctx); err != nil || sp != f.Service {
		t.Fatalf("service %v", err)
	}

	// A sealed value swapped into another slot, or a row moved to another id, does not open.
	exec := func(q string) {
		if err := s.W().TxNamed(ctx, "tamper", func(tx *store.Tx) error { _, err := tx.SQL.ExecContext(ctx, q); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE harbor_pilot_connection SET service_token_enc = parent_token_enc`)
	if _, err := svc.ServiceToken(ctx); err == nil {
		t.Fatal("the parent token opened as the service token")
	}
	exec(`UPDATE harbor_pilot_connection SET id = 'hpc_other'`)
	if _, err := svc.ParentToken(ctx); err == nil {
		t.Fatal("a sealed row moved to another id opened")
	}
}

// Every write is validated against Harbor Pilot: what it refuses becomes a readable field error, and nothing is stored.
func TestConnectionValidation(t *testing.T) {
	svc, s := newSvc(t)
	f := hpfake.New(t)
	ctx := context.Background()
	put := func(in ConnectionInput) error {
		_, err := svc.PutConnection(ctx, admin, 0, in)
		return err
	}
	ok := ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true, ParentToken: f.Parent}

	fieldMsg(t, put(ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true}), "parent_token", "is required")
	fieldMsg(t, put(ConnectionInput{BaseURL: "http://hp.example.com", ParentToken: f.Parent}), "base_url", "plain http")
	// A loopback base URL is fine for central, but runs reach it on their own machine: they need https or the flag.
	fieldMsg(t, put(ConnectionInput{BaseURL: f.URL(), ParentToken: f.Parent}), "run_base_url", "runs reach the gateway on base_url")
	fieldMsg(t, put(ConnectionInput{BaseURL: f.URL(), ParentToken: f.Service, AllowInsecureHTTP: true}), "parent_token", "is a service-principal token")
	fieldMsg(t, put(ConnectionInput{BaseURL: f.URL(), ParentToken: f.Parent, ServiceToken: f.Parent, AllowInsecureHTTP: true}), "service_token", "is a gateway token")
	fieldMsg(t, put(ConnectionInput{BaseURL: f.URL(), ParentToken: "hpk_with space", AllowInsecureHTTP: true}), "parent_token", "printable")

	// Harbor Pilot's refusals, one per class.
	f.Set(func(f *hpfake.HP) { f.ParentScopes = []string{ScopeInvoke, ScopeModels} })
	fieldMsg(t, put(ok), "parent_token", "parent token lacks tokens:mint")
	f.Set(func(f *hpfake.HP) { f.ParentScopes = []string{ScopeInvoke, ScopeMint} })
	fieldMsg(t, put(ok), "parent_token", "subset of the parent's")
	f.Set(func(f *hpfake.HP) { f.ParentScopes = []string{ScopeInvoke, ScopeModels, ScopeMint}; f.Old = true })
	fieldMsg(t, put(ok), "base_url", "older than Harbor Pilot 0.1.3")
	f.Set(func(f *hpfake.HP) { f.Old = false; f.FailMint = 502 })
	code(t, put(ok), apperr.CodeUnavailable)
	f.Set(func(f *hpfake.HP) { f.FailMint = 0 })
	bad := ok
	bad.ParentToken = "hpk_revoked_parent_token"
	fieldMsg(t, put(bad), "parent_token", "invalid, expired or revoked")
	withSP := ok
	withSP.ServiceToken = "hpa_wrong_service_token"
	fieldMsg(t, put(withSP), "service_token", "invalid, expired or revoked")
	withSP.ServiceToken = f.Service
	f.Set(func(f *hpfake.HP) { f.ServicePerms = []string{"config.read"} })
	fieldMsg(t, put(withSP), "service_token", "lacks usage.read")
	f.Set(func(f *hpfake.HP) { f.ServicePerms = []string{"usage.read"} })
	fieldMsg(t, put(withSP), "service_token", "lacks config.read")
	f.Set(func(f *hpfake.HP) { f.ServicePerms = []string{"usage.read", "config.read"} })
	dead := ok
	dead.BaseURL = "http://127.0.0.1:1"
	code(t, put(dead), apperr.CodeUnavailable)

	var n int
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM harbor_pilot_connection`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused connection was stored: %d %v", n, err)
	}
	if _, err := svc.GetConnection(ctx); err == nil {
		t.Fatal("no connection, but GetConnection found one")
	}

	// A probe credential that could not be revoked goes to the durable revocation queue.
	f.Set(func(f *hpfake.HP) { f.FailRevoke = true })
	c, err := svc.PutConnection(ctx, admin, 0, ok)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) == 0 || !strings.Contains(c.Warnings[len(c.Warnings)-1], "queued for revocation") {
		t.Fatalf("warnings %q", c.Warnings)
	}
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM gateway_revocations WHERE state = 'pending'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("queued revocations: %d %v", n, err)
	}
}

// Replacing the connection is version-checked; token versions count writes; a parent expiring sooner than an enabled
// space's token lifetime is warned about; deleting is refused while a space uses Harbor Pilot.
func TestConnectionLifecycleAndSpaces(t *testing.T) {
	svc, _ := newSvc(t)
	f := hpfake.New(t)
	ctx := context.Background()

	// Space settings: defaults first; enabling needs the connection.
	sp, err := svc.GetSpace(ctx, "spc_a")
	if err != nil || sp.Stored || sp.Enabled || sp.TTLSeconds != 86400 || sp.OnBudgetStatusUnavailable != "proceed" || len(sp.Harnesses) != 2 || sp.Connected {
		t.Fatalf("defaults %+v %v", sp, err)
	}
	_, err = svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: "prj_web"})
	code(t, err, apperr.CodePrecondition)
	if _, err := svc.GetSpace(ctx, "spc_nope"); err == nil {
		t.Fatal("unknown space")
	}

	c := connect(t, svc, f)
	if _, err := svc.PutConnection(ctx, admin, 0, ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true, ParentToken: f.Parent}); err == nil {
		t.Fatal("a second connection was created without If-Match")
	}
	c2, err := svc.PutConnection(ctx, admin, c.Version, ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true})
	if err != nil || c2.ParentTokenVersion != 1 || c2.ServiceTokenVersion != 1 || !c2.HasServiceToken || c2.Version != 2 {
		t.Fatalf("update keeping tokens %+v %v", c2, err)
	}
	_, err = svc.PutConnection(ctx, admin, c.Version, ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true})
	code(t, err, apperr.CodeConflict)
	c3, err := svc.PutConnection(ctx, admin, c2.Version, ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true, ParentToken: f.Parent, RemoveServiceToken: true})
	if err != nil || c3.ParentTokenVersion != 2 || c3.ServiceTokenVersion != 2 || c3.HasServiceToken {
		t.Fatalf("rotate parent, remove service token %+v %v", c3, err)
	}
	c4, err := svc.PutConnection(ctx, admin, c3.Version, ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true, ServiceToken: f.Service})
	if err != nil || c4.ServiceTokenVersion != 3 || !c4.HasServiceToken {
		t.Fatalf("service token again %+v %v", c4, err)
	}

	// Space validation.
	fieldMsg(t, func() error { _, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true}); return err }(), "project_id", "required")
	fieldMsg(t, func() error {
		_, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{ProjectID: "prj_web", TTLSeconds: 600})
		return err
	}(), "ttl_seconds", "between 900")
	fieldMsg(t, func() error {
		_, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{ProjectID: "prj_web", TTLSeconds: 604801})
		return err
	}(), "ttl_seconds", "between 900")
	fieldMsg(t, func() error {
		_, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Harnesses: []string{"Claude Code"}})
		return err
	}(), "harnesses", "not a harness kind")
	fieldMsg(t, func() error {
		_, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: "prj_missing"})
		return err
	}(), "project_id", "has no project")
	fieldMsg(t, func() error {
		_, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: "web-app"})
		return err
	}(), "project_id", "use its id prj_web")
	fieldMsg(t, func() error {
		_, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: "prj_other"})
		return err
	}(), "project_id", "allowed projects")
	if got := f.Unrevoked(); len(got) != 0 {
		t.Fatalf("probe credentials left unrevoked: %v", got)
	}

	// Any harness name is accepted (ADR-0060 Q6), sorted and deduplicated.
	sp, err = svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: "prj_web", Harnesses: []string{"codex", "gemini-cli", "codex"},
		TTLSeconds: 7200, OnBudgetStatusUnavailable: "refuse", AllowedModelAliases: []string{"coding/default", "coding/fast", "coding/default"}})
	if err != nil || !sp.Stored || !sp.Enabled || strings.Join(sp.Harnesses, ",") != "codex,gemini-cli" || sp.TTLSeconds != 7200 ||
		sp.OnBudgetStatusUnavailable != "refuse" || strings.Join(sp.AllowedModelAliases, ",") != "coding/default,coding/fast" || sp.Version != 1 || !sp.Connected {
		t.Fatalf("enabled %+v %v", sp, err)
	}
	_, err = svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{})
	code(t, err, apperr.CodeConflict)
	_, err = svc.PutSpace(ctx, admin, "spc_b", 3, SpaceInput{})
	code(t, err, apperr.CodeNotFound)

	// Deleting the connection is refused while a space is enabled.
	cur, _ := svc.GetConnection(ctx)
	if len(cur.EnabledSpaces) != 1 || cur.EnabledSpaces[0] != "spc_a" {
		t.Fatalf("enabled spaces %v", cur.EnabledSpaces)
	}
	code(t, svc.DeleteConnection(ctx, admin, cur.Version), apperr.CodePrecondition)
	// A parent of another binding can't replace this one while spaces use it.
	f.Set(func(f *hpfake.HP) { f.BindingID = "bnd_other" })
	_, err = svc.PutConnection(ctx, admin, cur.Version, ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true})
	code(t, err, apperr.CodePrecondition)
	f.Set(func(f *hpfake.HP) { f.BindingID = "bnd_fake" })

	// A parent expiring before a space's token lifetime is warned about (Harbor Pilot clamps children to it).
	f.Set(func(f *hpfake.HP) { f.ParentExpires = time.Now().Add(time.Hour) })
	cur, err = svc.PutConnection(ctx, admin, cur.Version, ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true})
	if err != nil || cur.ParentExpiresAt == nil || len(cur.Warnings) == 0 || !strings.Contains(strings.Join(cur.Warnings, "\n"), "sooner than space spc_a") {
		t.Fatalf("expiry warning %+v %v", cur, err)
	}
	if sp, _ := svc.GetSpace(ctx, "spc_a"); len(sp.Warnings) != 1 {
		t.Fatalf("space warnings %q", sp.Warnings)
	}

	// Disabling, then deleting the space settings, then the connection.
	sp, err = svc.PutSpace(ctx, admin, "spc_a", sp.Version, SpaceInput{ProjectID: "prj_web"})
	if err != nil || sp.Enabled || sp.Version != 2 {
		t.Fatalf("disabled %+v %v", sp, err)
	}
	code(t, svc.DeleteSpace(ctx, admin, "spc_a", 1), apperr.CodeConflict)
	if err := svc.DeleteSpace(ctx, admin, "spc_a", 2); err != nil {
		t.Fatal(err)
	}
	if sp, _ := svc.GetSpace(ctx, "spc_a"); sp.Stored {
		t.Fatalf("after delete %+v", sp)
	}
	code(t, svc.DeleteConnection(ctx, admin, cur.Version-1), apperr.CodeConflict)
	if err := svc.DeleteConnection(ctx, admin, cur.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetConnection(ctx); err == nil {
		t.Fatal("deleted connection still there")
	}
}

// Live run tokens and pending revocations keep the connection: revoking them needs the parent token.
func TestDeleteConnectionWaitsForRunTokens(t *testing.T) {
	svc, s := newSvc(t)
	f := hpfake.New(t)
	ctx := context.Background()
	c := connect(t, svc, f)
	if err := s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO gateway_revocations (credential_id, reason, expires_at, next_attempt_at, requested_at) VALUES ('crd_x', 'run ended', 1, 1, 1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	code(t, svc.DeleteConnection(ctx, admin, c.Version), apperr.CodePrecondition)
}
