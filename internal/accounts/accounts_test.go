package accounts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

type fx struct {
	t   *testing.T
	ctx context.Context
	s   *store.Store
	svc *Service
}

func newFx(t *testing.T) *fx {
	t.Helper()
	s := storetest.Open(t, storetest.WithClock(clock.NewFake(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))))
	kr := keyringtest.Load(t, s)
	f := &fx{t: t, ctx: context.Background(), s: s, svc: New(s, kr, audit.NewWriter(kr), changefeed.New(nil))}
	must(t, s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(f.ctx, q); err != nil {
				return err
			}
		}
		return nil
	}))
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func code(t *testing.T, err error, want apperr.Code) {
	t.Helper()
	if apperr.CodeOf(err) != want {
		t.Fatalf("got %v (%s), want %s", err, apperr.CodeOf(err), want)
	}
}

func (f *fx) add(name, kind, secret string) *Account {
	f.t.Helper()
	a, err := f.svc.Create(f.ctx, admin, Input{Name: name, Kind: kind, Secret: secret, Enabled: true})
	must(f.t, err)
	return a
}

func (f *fx) resolve(harness, ref string) (string, error) {
	return f.resolveAvoiding(harness, ref, nil)
}

func (f *fx) resolveAvoiding(harness, ref string, avoid func(string) (bool, error)) (string, error) {
	var id string
	err := f.s.W().TxNamed(f.ctx, "resolve", func(tx *store.Tx) error {
		var err error
		id, err = Resolve(f.ctx, tx, "spc_a", harness, ref, avoid)
		return err
	})
	return id, err
}

// SM-ACCT-001/005: secrets are sealed and never returned; validation; names are unique.
func TestCreateIsWriteOnly(t *testing.T) {
	f := newFx(t)
	a := f.add("team-claude", "claude_oauth_token", "sk-ant-oat01-secret")
	if a.Harness != "claude-code" || a.SecretVersion != 1 || !a.Enabled || a.Metadata == nil {
		t.Fatalf("account %+v", a)
	}
	row, err := f.s.R().GetAccount(f.ctx, a.ID)
	must(t, err)
	if strings.Contains(string(row.SecretEnc), "sk-ant-oat01-secret") {
		t.Fatal("the secret is stored in clear text")
	}
	for name, in := range map[string]Input{
		"bad name":  {Name: "Team Claude", Kind: "claude_oauth_token", Secret: "x"},
		"bad kind":  {Name: "x", Kind: "chatgpt", Secret: "x"},
		"no secret": {Name: "x", Kind: "openai_api_key"},
		"two lines": {Name: "x", Kind: "openai_api_key", Secret: "a\nb"},
		"long desc": {Name: "x", Kind: "openai_api_key", Secret: "s", Description: strings.Repeat("d", 2001)},
		"many meta": {Name: "x", Kind: "openai_api_key", Secret: "s", Metadata: map[string]string{"k": strings.Repeat("v", 501)}},
	} {
		_, err := f.svc.Create(f.ctx, admin, in)
		if apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Fatalf("%s: %v", name, err)
		}
	}
	_, err = f.svc.Create(f.ctx, admin, Input{Name: "team-claude", Kind: "anthropic_api_key", Secret: "k", Enabled: true})
	code(t, err, apperr.CodeAlreadyExists)
	_, err = f.svc.Get(f.ctx, "acc_missing")
	code(t, err, apperr.CodeNotFound)
}

// SM-ACCT-003: deterministic resolution with clear errors.
func TestResolution(t *testing.T) {
	f := newFx(t)
	_, err := f.resolve("claude-code", "")
	code(t, err, apperr.CodePrecondition) // nothing bound
	c1 := f.add("claude-one", "claude_oauth_token", "t1")
	c2 := f.add("claude-two", "anthropic_api_key", "k2")
	cx := f.add("codex", "openai_api_key", "o1")
	_, err = f.svc.SetBindings(f.ctx, admin, "spc_a", []Binding{{AccountID: c1.ID}})
	must(t, err)
	if id, err := f.resolve("claude-code", ""); err != nil || id != c1.ID {
		t.Fatalf("only bound account: %s %v", id, err)
	}
	_, err = f.resolve("codex", "")
	code(t, err, apperr.CodePrecondition) // no compatible account
	_, err = f.svc.SetBindings(f.ctx, admin, "spc_a", []Binding{{AccountID: c1.ID}, {AccountID: c2.ID}, {AccountID: cx.ID}})
	must(t, err)
	_, err = f.resolve("claude-code", "")
	if apperr.CodeOf(err) != apperr.CodePrecondition || !strings.Contains(err.Error(), "no default") {
		t.Fatalf("ambiguous: %v", err)
	}
	bs, err := f.svc.SetBindings(f.ctx, admin, "spc_a", []Binding{{AccountID: c1.ID}, {AccountID: c2.ID, Default: true}, {AccountID: cx.ID}})
	must(t, err)
	if len(bs) != 3 || bs[0].Harness != "claude-code" || bs[2].Harness != "codex" {
		t.Fatalf("bindings %+v", bs)
	}
	if id, _ := f.resolve("claude-code", ""); id != c2.ID {
		t.Fatalf("default: %s", id)
	}
	if id, _ := f.resolve("claude-code", "claude-one"); id != c1.ID {
		t.Fatalf("named: %s", id)
	}
	if id, _ := f.resolve("codex", ""); id != cx.ID {
		t.Fatalf("codex: %s", id)
	}
	_, err = f.resolve("claude-code", cx.ID)
	code(t, err, apperr.CodePrecondition) // incompatible
	_, err = f.svc.SetBindings(f.ctx, admin, "spc_a", []Binding{{AccountID: c1.ID, Default: true}, {AccountID: c2.ID, Default: true}})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.SetBindings(f.ctx, admin, "spc_a", []Binding{{AccountID: c1.ID}, {AccountID: c1.ID}})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.SetBindings(f.ctx, admin, "spc_a", []Binding{{AccountID: "acc_nope"}})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.SetBindings(f.ctx, admin, "spc_nope", nil)
	code(t, err, apperr.CodeNotFound)
	// A disabled default is skipped.
	_, err = f.svc.Update(f.ctx, admin, c2.ID, c2.Version, Input{Name: c2.Name, Enabled: false})
	must(t, err)
	if id, err := f.resolve("claude-code", ""); err != nil || id != c1.ID {
		t.Fatalf("disabled default skipped: %s %v", id, err) // c1 is now the only usable Claude account
	}
	if id, err := f.resolve("claude-code", "claude-one"); err != nil || id != c1.ID {
		t.Fatalf("named while default disabled: %s %v", id, err)
	}
}

// ADR-0079 decision 8: with failover, an avoided (held) account gives way to the next usable account of the harness,
// the default first; a named account fails over too (a resume names the old run's account). When every account is
// avoided the usual choice stands, and failover never hides a configuration error.
func TestResolutionFailover(t *testing.T) {
	f := newFx(t)
	c1 := f.add("claude-one", "claude_oauth_token", "t1")
	c2 := f.add("claude-two", "claude_oauth_token", "t2")
	c3 := f.add("claude-three", "claude_oauth_token", "t3")
	cx := f.add("codex", "openai_api_key", "o1")
	_, err := f.svc.SetBindings(f.ctx, admin, "spc_a", []Binding{{AccountID: c1.ID}, {AccountID: c2.ID}, {AccountID: c3.ID, Default: true}, {AccountID: cx.ID}})
	must(t, err)
	held := map[string]bool{}
	avoid := func(id string) (bool, error) { return held[id], nil }
	if id, err := f.resolveAvoiding("claude-code", "", avoid); err != nil || id != c3.ID {
		t.Fatalf("nothing held: %s %v", id, err)
	}
	held[c3.ID] = true
	if id, err := f.resolveAvoiding("claude-code", "", avoid); err != nil || id != c1.ID {
		t.Fatalf("default held: %s %v", id, err)
	}
	held[c1.ID] = true
	if id, err := f.resolveAvoiding("claude-code", "claude-one", avoid); err != nil || id != c2.ID {
		t.Fatalf("named and held: %s %v", id, err)
	}
	if id, err := f.resolveAvoiding("claude-code", "claude-one", nil); err != nil || id != c1.ID {
		t.Fatalf("failover off: %s %v", id, err)
	}
	held[c2.ID] = true
	if id, err := f.resolveAvoiding("claude-code", "", avoid); err != nil || id != c3.ID {
		t.Fatalf("all held keeps the default: %s %v", id, err)
	}
	held[cx.ID] = true
	if id, err := f.resolveAvoiding("codex", "", avoid); err != nil || id != cx.ID {
		t.Fatalf("never another harness: %s %v", id, err)
	}
	_, err = f.resolveAvoiding("claude-code", "nope", avoid)
	code(t, err, apperr.CodePrecondition)
	boom := errors.New("boom")
	if _, err := f.resolveAvoiding("claude-code", "", func(string) (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Fatalf("avoid error: %v", err)
	}
}

// SM-ACCT-004/006: dispatch gets the current secret only while the account is usable; rotation applies to later
// dispatches.
func TestSecretEnvAndRotation(t *testing.T) {
	f := newFx(t)
	a := f.add("team-claude", "claude_oauth_token", "first")
	_, err := f.svc.SecretEnv(f.ctx, "spc_a", "claude-code", a.ID)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unbound: %v", err)
	}
	_, err = f.svc.SetBindings(f.ctx, admin, "spc_a", []Binding{{AccountID: a.ID, Default: true}})
	must(t, err)
	env, err := f.svc.SecretEnv(f.ctx, "spc_a", "claude-code", a.ID)
	must(t, err)
	if len(env) != 1 || env["CLAUDE_CODE_OAUTH_TOKEN"] != "first" {
		t.Fatalf("env %v", env)
	}
	if _, err := f.svc.SecretEnv(f.ctx, "spc_a", "codex", a.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("incompatible harness: %v", err)
	}
	rot, err := f.svc.Update(f.ctx, admin, a.ID, a.Version, Input{Name: "team-claude", Secret: "second", Enabled: true, Description: "rotated"})
	must(t, err)
	if rot.SecretVersion != 2 || rot.Version != a.Version+2 || rot.Spaces != 1 {
		t.Fatalf("rotated %+v", rot)
	}
	if env, _ := f.svc.SecretEnv(f.ctx, "spc_a", "claude-code", a.ID); env["CLAUDE_CODE_OAUTH_TOKEN"] != "second" {
		t.Fatalf("after rotation %v", env)
	}
	_, err = f.svc.Update(f.ctx, admin, a.ID, a.Version, Input{Name: "team-claude", Enabled: true})
	code(t, err, apperr.CodePrecondition)
	other := f.add("other", "anthropic_api_key", "k")
	_, err = f.svc.Update(f.ctx, admin, other.ID, other.Version, Input{Name: "team-claude", Enabled: true})
	code(t, err, apperr.CodeAlreadyExists)
	_, err = f.svc.Update(f.ctx, admin, "acc_nope", 1, Input{Name: "x", Enabled: true})
	code(t, err, apperr.CodeNotFound)
	off, err := f.svc.Update(f.ctx, admin, a.ID, rot.Version, Input{Name: "team-claude", Enabled: false})
	must(t, err)
	if off.SecretVersion != 2 || off.Enabled {
		t.Fatalf("disabled %+v", off) // an edit without a secret keeps it
	}
	if _, err := f.svc.SecretEnv(f.ctx, "spc_a", "claude-code", a.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("disabled: %v", err)
	}
	l, err := f.svc.List(f.ctx)
	must(t, err)
	if len(l) != 2 || l[0].Name != "other" {
		t.Fatalf("list %+v", l)
	}
	must(t, f.svc.Delete(f.ctx, admin, a.ID))
	code(t, f.svc.Delete(f.ctx, admin, a.ID), apperr.CodeNotFound)
	if _, err := f.svc.SecretEnv(f.ctx, "spc_a", "claude-code", a.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("deleted: %v", err)
	}
	if bs, _ := f.svc.Bindings(f.ctx, "spc_a"); len(bs) != 0 {
		t.Fatalf("bindings survive deletion: %+v", bs)
	}
}

// SM-ACCT-002, ADR-0053: a Codex ChatGPT login is the auth.json `codex login` writes. Only a ChatGPT-mode file with
// its tokens is accepted (an API-key file belongs to the openai_api_key kind), it is stored compacted, a rotation is
// checked the same way, and it reaches a launch under CODEX_AUTH_JSON, never as an environment variable.
func TestCodexChatGPTLogin(t *testing.T) {
	f := newFx(t)
	login := "{\n  \"auth_mode\": \"chatgpt\",\n  \"tokens\": {\"access_token\": \"a1\", \"refresh_token\": \"r1\"},\n  \"last_refresh\": \"2026-11-03T10:00:00Z\"\n}\n"
	for _, bad := range []string{
		"",
		"not json",
		`{"auth_mode": "apikey", "OPENAI_API_KEY": "sk-x"}`,
		`{"auth_mode": "chatgpt"}`,
		`{"auth_mode": "chatgpt", "tokens": {}}`,
		`["auth_mode", "chatgpt"]`,
		`{"auth_mode": "chatgpt", "tokens": {"a": "` + strings.Repeat("x", 70_000) + `"}}`,
	} {
		_, err := f.svc.Create(f.ctx, admin, Input{Name: "codex-bad", Kind: KindCodexChatGPT, Secret: bad, Enabled: true})
		code(t, err, apperr.CodeInvalid)
	}
	if CodexAuthKey != harness.CodexAuthKey {
		t.Fatalf("payload key %q, the Codex harness reads %q", CodexAuthKey, harness.CodexAuthKey)
	}
	a := f.add("codex-plan", KindCodexChatGPT, login)
	if a.Harness != "codex" {
		t.Fatalf("harness %q", a.Harness)
	}
	_, err := f.svc.SetBindings(f.ctx, admin, "spc_a", []Binding{{AccountID: a.ID, Default: true}})
	must(t, err)
	env, err := f.svc.SecretEnv(f.ctx, "spc_a", "codex", a.ID)
	must(t, err)
	if len(env) != 1 || env[CodexAuthKey] != `{"auth_mode":"chatgpt","tokens":{"access_token":"a1","refresh_token":"r1"},"last_refresh":"2026-11-03T10:00:00Z"}` {
		t.Fatalf("secret env %v", env)
	}
	// A rotation is checked like a new login; a one-line token is not a login.
	_, err = f.svc.Update(f.ctx, admin, a.ID, a.Version, Input{Name: "codex-plan", Secret: "sk-plain-token", Enabled: true})
	code(t, err, apperr.CodeInvalid)
	rot, err := f.svc.Update(f.ctx, admin, a.ID, a.Version, Input{Name: "codex-plan", Secret: strings.ReplaceAll(login, "r1", "r2"), Enabled: true})
	must(t, err)
	if env, _ := f.svc.SecretEnv(f.ctx, "spc_a", "codex", a.ID); !strings.Contains(env[CodexAuthKey], `"refresh_token":"r2"`) || rot.SecretVersion != 2 {
		t.Fatalf("after rotation %v %+v", env, rot)
	}
	// The other kinds keep their one-line rule.
	_, err = f.svc.Create(f.ctx, admin, Input{Name: "key-json", Kind: "openai_api_key", Secret: login, Enabled: true})
	code(t, err, apperr.CodeInvalid)
}
