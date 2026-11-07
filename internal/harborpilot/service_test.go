package harborpilot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
	_, err = svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: "prj_web"}, true)
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
	fieldMsg(t, func() error {
		_, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true}, true)
		return err
	}(), "project_id", "required")
	fieldMsg(t, func() error {
		_, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{ProjectID: "prj_web", TTLSeconds: 600}, true)
		return err
	}(), "ttl_seconds", "between 900")
	fieldMsg(t, func() error {
		_, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{ProjectID: "prj_web", TTLSeconds: 604801}, true)
		return err
	}(), "ttl_seconds", "between 900")
	fieldMsg(t, func() error {
		_, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Harnesses: []string{"Claude Code"}}, true)
		return err
	}(), "harnesses", "not a harness kind")
	fieldMsg(t, func() error {
		_, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: "prj_missing"}, true)
		return err
	}(), "project_id", "has no project")
	fieldMsg(t, func() error {
		_, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: "web-app"}, true)
		return err
	}(), "project_id", "use its id prj_web")
	fieldMsg(t, func() error {
		_, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: "prj_other"}, true)
		return err
	}(), "project_id", "allowed projects")
	if got := f.Unrevoked(); len(got) != 0 {
		t.Fatalf("probe credentials left unrevoked: %v", got)
	}

	// Any harness name is accepted (ADR-0060 Q6), sorted and deduplicated.
	sp, err = svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: "prj_web", Harnesses: []string{"codex", "gemini-cli", "codex"},
		TTLSeconds: 7200, OnBudgetStatusUnavailable: "refuse", AllowedModelAliases: []string{"coding/default", "coding/fast", "coding/default"}}, true)
	if err != nil || !sp.Stored || !sp.Enabled || strings.Join(sp.Harnesses, ",") != "codex,gemini-cli" || sp.TTLSeconds != 7200 ||
		sp.OnBudgetStatusUnavailable != "refuse" || strings.Join(sp.AllowedModelAliases, ",") != "coding/default,coding/fast" || sp.Version != 1 || !sp.Connected {
		t.Fatalf("enabled %+v %v", sp, err)
	}
	_, err = svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{}, true)
	code(t, err, apperr.CodeConflict)
	_, err = svc.PutSpace(ctx, admin, "spc_b", 3, SpaceInput{}, true)
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
	sp, err = svc.PutSpace(ctx, admin, "spc_a", sp.Version, SpaceInput{ProjectID: "prj_web"}, true)
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
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO gateway_revocations (credential_id, reason, expires_at, next_attempt_at, requested_at) VALUES ('crd_x', 'run ended', ?, 1, 1)`,
			time.Now().Add(time.Hour).UnixMicro())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ae := appErr(t, svc.DeleteConnection(ctx, admin, c.Version), apperr.CodePrecondition)
	if !strings.Contains(ae.Message, "tokens (run or validation)") {
		t.Fatalf("message %q", ae.Message)
	}
	// An expired credential no longer matters (Harbor Pilot refuses it anyway): it does not keep the connection.
	if err := s.W().TxNamed(ctx, "expire", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE gateway_revocations SET expires_at = 1 WHERE credential_id = 'crd_x'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A live run token blocks too (its revocation needs the parent); once it has expired, it no longer does.
	if err := s.W().TxNamed(ctx, "run token", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
			 VALUES ('run_1', 'spc_a', 'nod_1', 1, 'worker', 'implementor', 'claude-code', 'ins_1', 'running', 'running', '{}', 0, 't', 0)`,
			`INSERT INTO run_gateway_credentials (run_id, space_id, credential_id, token_enc, expires_at, created_at) VALUES ('run_1', 'spc_a', 'crd_run', x'00', ?, 0)`,
		} {
			if _, err := tx.SQL.ExecContext(ctx, q, time.Now().Add(time.Hour).UnixMicro()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ae = appErr(t, svc.DeleteConnection(ctx, admin, c.Version), apperr.CodePrecondition)
	if !strings.Contains(ae.Message, "1 unexpired tokens") {
		t.Fatalf("live run token: %q", ae.Message)
	}
	if err := s.W().TxNamed(ctx, "expire run token", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE run_gateway_credentials SET expires_at = 1 WHERE run_id = 'run_1'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteConnection(ctx, admin, c.Version); err != nil {
		t.Fatalf("expired pending revocation or run token still blocks: %v", err)
	}
}

// Security (review of HP-T2, finding 1): the stored tokens are only ever sent to the address they were entered for.
// Moving the gateway (base_url, run_base_url, or newly allowing plain http) without entering the tokens again is
// refused with a validation error before any request, so the new address never sees a stored token.
func TestConnectionAddressChangeNeedsTokens(t *testing.T) {
	t.Parallel()
	svc, _ := newSvc(t)
	f := hpfake.New(t)
	evil := hpfake.New(t)
	ctx := context.Background()
	c := connect(t, svc, f)
	before := f.Count()

	for name, in := range map[string]ConnectionInput{
		"base_url":            {BaseURL: evil.URL(), AllowInsecureHTTP: true},
		"base_url+path":       {BaseURL: f.URL() + "/other", AllowInsecureHTTP: true},
		"run_base_url":        {BaseURL: f.URL(), RunBaseURL: evil.URL(), AllowInsecureHTTP: true},
		"parent_only":         {BaseURL: evil.URL(), AllowInsecureHTTP: true, ParentToken: f.Parent},
		"service_only":        {BaseURL: evil.URL(), AllowInsecureHTTP: true, ServiceToken: f.Service},
		"remove_service_only": {BaseURL: evil.URL(), AllowInsecureHTTP: true, RemoveServiceToken: true},
	} {
		_, err := svc.PutConnection(ctx, admin, c.Version, in)
		ae := appErr(t, err, apperr.CodeInvalid)
		fields := map[string]bool{}
		for _, fe := range ae.Fields {
			if !strings.Contains(fe.Message, "re-enter the tokens when the gateway URL changes") {
				t.Errorf("%s: %s: %q", name, fe.Field, fe.Message)
			}
			fields[fe.Field] = true
		}
		if fields["parent_token"] != (in.ParentToken == "") || fields["service_token"] != (in.ServiceToken == "" && !in.RemoveServiceToken) {
			t.Errorf("%s: fields %+v", name, ae.Fields)
		}
	}
	if n := evil.Count(); n != 0 {
		t.Fatalf("the new address got %d requests", n)
	}
	if n := f.Count(); n != before {
		t.Fatalf("the stored address got %d requests for refused writes", n-before)
	}

	// Newly allowing plain http needs the tokens again too.
	svc2, _ := newSvc(t)
	g := hpfake.New(t)
	c2, err := svc2.PutConnection(ctx, admin, 0, ConnectionInput{BaseURL: g.URL(), RunBaseURL: "https://hp.example.com", ParentToken: g.Parent})
	if err != nil {
		t.Fatal(err)
	}
	before = g.Count()
	_, err = svc2.PutConnection(ctx, admin, c2.Version, ConnectionInput{BaseURL: g.URL(), RunBaseURL: "https://hp.example.com", AllowInsecureHTTP: true})
	fieldMsg(t, err, "parent_token", "re-enter the tokens")
	if g.Count() != before {
		t.Fatal("a refused write called the gateway")
	}

	// Entering the tokens again moves the connection; the same address keeps them without.
	c3, err := svc.PutConnection(ctx, admin, c.Version, ConnectionInput{BaseURL: evil.URL(), AllowInsecureHTTP: true, ParentToken: evil.Parent, ServiceToken: evil.Service})
	if err != nil || c3.BaseURL != evil.URL() {
		t.Fatalf("move with tokens %+v %v", c3, err)
	}
	if _, err := svc.PutConnection(ctx, admin, c3.Version, ConnectionInput{BaseURL: evil.URL(), AllowInsecureHTTP: true}); err != nil {
		t.Fatalf("same address without tokens: %v", err)
	}
}

// ADR-0060 decision 2 (the operator's decision of 2026-11-07): the project decides who pays, so setting or changing it
// needs account:write; the rest of a space's settings need only space_config:write.
func TestSpaceProjectNeedsAccountWrite(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	ctx := context.Background()
	connect(t, svc, f)
	op := audit.Actor{Type: audit.ActorUser, ID: "usr_op"}

	// An operator can't create settings with a project, and no probe reaches Harbor Pilot.
	before := f.Count()
	_, err := svc.PutSpace(ctx, op, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: "prj_web"}, false)
	if ae := appErr(t, err, apperr.CodeForbidden); !strings.Contains(ae.Message, "needs account:write") {
		t.Fatalf("message %q", ae.Message)
	}
	if f.Count() != before {
		t.Fatal("a refused project change probed Harbor Pilot")
	}
	// Settings without a project are fine.
	sp, err := svc.PutSpace(ctx, op, "spc_b", 0, SpaceInput{TTLSeconds: 3600}, false)
	if err != nil || sp.ProjectID != "" {
		t.Fatalf("no project %+v %v", sp, err)
	}

	sp, err = svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: "prj_web"}, true)
	if err != nil {
		t.Fatal(err)
	}
	// The operator changes the rest; an omitted project keeps the stored one, the same one is fine too.
	sp, err = svc.PutSpace(ctx, op, "spc_a", sp.Version, SpaceInput{Enabled: true, TTLSeconds: 3600}, false)
	if err != nil || sp.ProjectID != "prj_web" || sp.TTLSeconds != 3600 {
		t.Fatalf("omitted project %+v %v", sp, err)
	}
	sp, err = svc.PutSpace(ctx, op, "spc_a", sp.Version, SpaceInput{ProjectID: "prj_web"}, false)
	if err != nil || sp.Enabled {
		t.Fatalf("disable %+v %v", sp, err)
	}
	_, err = svc.PutSpace(ctx, op, "spc_a", sp.Version, SpaceInput{Enabled: true, ProjectID: "prj_data"}, false)
	code(t, err, apperr.CodeForbidden)
	sp, err = svc.PutSpace(ctx, admin, "spc_a", sp.Version, SpaceInput{Enabled: true, ProjectID: "prj_data"}, true)
	if err != nil || sp.ProjectID != "prj_data" {
		t.Fatalf("admin change %+v %v", sp, err)
	}
	var got string
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT group_concat(c, ',') FROM (SELECT json_extract(details_json, '$.project_changed') AS c FROM audit_events
		WHERE action LIKE 'harbor_pilot.space.%' ORDER BY seq)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "0,1,0,0,1" {
		t.Fatalf("project_changed in audit: %s", got)
	}

	// The check is repeated in the transaction: a project changed behind the caller's back (between the probe and
	// the write) is not silently kept by an operator's write.
	f.Set(func(h *hpfake.HP) {
		h.Hook = func(r *http.Request) {
			if r.Method == http.MethodGet {
				_ = s.W().TxNamed(ctx, "behind", func(tx *store.Tx) error {
					_, err := tx.SQL.ExecContext(ctx, `UPDATE space_harbor_pilot SET project_id = 'prj_web' WHERE space_id = 'spc_a'`)
					return err
				})
			}
		}
	})
	_, err = svc.PutSpace(ctx, op, "spc_a", sp.Version, SpaceInput{Enabled: true, TTLSeconds: 7200}, false)
	code(t, err, apperr.CodeForbidden)
}

// Review of HP-T2, finding 6: the project is checked against one connection; a connection replaced meanwhile (another
// binding, say) makes the write a conflict instead of storing a project the new binding may not allow.
func TestSpaceProjectCheckedAgainstTheStoredConnection(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	ctx := context.Background()
	connect(t, svc, f)
	f.Set(func(h *hpfake.HP) {
		h.Hook = func(r *http.Request) {
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/projects/") {
				_ = s.W().TxNamed(ctx, "replace", func(tx *store.Tx) error {
					_, err := tx.SQL.ExecContext(ctx, `UPDATE harbor_pilot_connection SET binding_id = 'bnd_other', version = version + 1`)
					return err
				})
			}
		}
	})
	_, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: "prj_web"}, true)
	if ae := appErr(t, err, apperr.CodeConflict); !strings.Contains(ae.Message, "connection changed") {
		t.Fatalf("message %q", ae.Message)
	}
	if sp, _ := svc.GetSpace(ctx, "spc_a"); sp.Stored {
		t.Fatalf("stored %+v", sp)
	}
}

// Review of HP-T2, finding 5: a validation credential that can be neither revoked nor queued is said to stay valid,
// not to be queued.
func TestUnqueuedValidationCredentialIsReported(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t)
	f := hpfake.New(t)
	ctx := context.Background()
	if err := s.W().TxNamed(ctx, "block", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `CREATE TRIGGER no_revocations BEFORE INSERT ON gateway_revocations BEGIN SELECT RAISE(ABORT, 'refused'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.Set(func(h *hpfake.HP) { h.FailRevoke = true })
	c := connect(t, svc, f)
	w := strings.Join(c.Warnings, "\n")
	if !strings.Contains(w, "nor queued for revocation") || strings.Contains(w, "it is queued") {
		t.Fatalf("warnings %q", c.Warnings)
	}
}
