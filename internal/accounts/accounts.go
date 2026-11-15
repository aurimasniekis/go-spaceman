// Package accounts holds harness accounts (SM-ACCT-*): the credentials Claude Code and Codex runs use. A credential is
// sealed with the keyring, is write-only (never returned, logged or put in events or manifests), and reaches a run only
// at dispatch, over the authenticated node connection, through the launcher shim's file (ADR-0009). Spaces bind the
// accounts their runs may use; resolution is deterministic and fails clearly when no account fits (SM-ACCT-003).
package accounts

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Kind describes one credential mechanism (verified in docs/contracts/harnesses.md).
type Kind struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Harness string `json:"harness" enum:"claude-code,codex"`
	// Env is the variable the launcher shim sets for the harness; for a file credential, the key the secret travels
	// under in the launch payload (never an environment variable).
	Env string `json:"env"`
	// Delivery is env (the launcher shim sets Env) or file (the node keeps the credential in a file the harness reads
	// and rewrites, ADR-0053).
	Delivery string `json:"delivery" enum:"env,file"`
}

// KindCodexChatGPT is a Codex ChatGPT-plan login: the auth.json `codex login` writes (ADR-0053).
const KindCodexChatGPT = "codex_chatgpt_login"

// CodexAuthKey is the launch-payload key of a Codex ChatGPT login (internal/harness reads it; not an env var).
const CodexAuthKey = "CODEX_AUTH_JSON"

// maxAuthJSON bounds a ChatGPT login file.
const maxAuthJSON = 64 << 10

// Kinds are the supported credential mechanisms.
var Kinds = []Kind{
	{Name: "claude_oauth_token", Label: "Claude subscription token (claude setup-token)", Harness: "claude-code", Env: "CLAUDE_CODE_OAUTH_TOKEN", Delivery: "env"},
	{Name: "anthropic_api_key", Label: "Anthropic API key", Harness: "claude-code", Env: "ANTHROPIC_API_KEY", Delivery: "env"},
	{Name: "openai_api_key", Label: "OpenAI API key", Harness: "codex", Env: "OPENAI_API_KEY", Delivery: "env"},
	{Name: KindCodexChatGPT, Label: "ChatGPT login for Codex (auth.json from codex login)", Harness: "codex", Env: CodexAuthKey, Delivery: "file"},
}

// KindOf returns a kind by name.
func KindOf(name string) (Kind, bool) {
	i := slices.IndexFunc(Kinds, func(k Kind) bool { return k.Name == name })
	if i < 0 {
		return Kind{}, false
	}
	return Kinds[i], true
}

// Account is an account without its secret.
type Account struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Kind            string            `json:"kind" enum:"claude_oauth_token,anthropic_api_key,openai_api_key,codex_chatgpt_login"`
	Harness         string            `json:"harness" enum:"claude-code,codex"`
	Description     string            `json:"description,omitempty"`
	Metadata        map[string]string `json:"metadata"`
	SecretVersion   int64             `json:"secret_version" doc:"Increases with every rotation; the secret itself is never returned"`
	SecretRotatedAt time.Time         `json:"secret_rotated_at"`
	Enabled         bool              `json:"enabled"`
	Spaces          int64             `json:"spaces" doc:"Number of spaces bound to the account"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	Version         int64             `json:"version"`
}

// Binding allows a space's runs to use an account.
type Binding struct {
	AccountID string `json:"account_id"`
	Harness   string `json:"harness,omitempty" enum:"claude-code,codex" doc:"Set by the server from the account kind"`
	Default   bool   `json:"default" doc:"The account the space's runs of this harness use unless a launch names another"`
	Name      string `json:"name,omitempty"`
	Enabled   bool   `json:"enabled"`
	Priority  int    `json:"priority,omitempty" minimum:"0" maximum:"1000" doc:"Pool order for usage-limit failover, lower first; 0 or omitted is 100"`
}

// DefaultPriority is the pool priority of a binding that sets none. Lower is tried first.
const DefaultPriority = 100

// MaxPriority bounds a priority.
const MaxPriority = 1000

func priorityOf(p int) int {
	if p == 0 {
		return DefaultPriority
	}
	return p
}

// PoolEntry is one account of a role pool.
type PoolEntry struct {
	AccountID string `json:"account_id" maxLength:"64"`
	Name      string `json:"name,omitempty"`
	Priority  int    `json:"priority,omitempty" minimum:"0" maximum:"1000" doc:"Lower first; 0 or omitted is 100"`
}

// RolePool is the accounts one role's runs use instead of the space pool.
type RolePool struct {
	Role  string      `json:"role"`
	Items []PoolEntry `json:"items"`
}

// Input creates or edits an account. Secret is write-only: empty on an edit keeps the stored one.
type Input struct {
	Name        string
	Kind        string
	Description string
	Metadata    map[string]string
	Secret      string
	Enabled     bool
}

// Service manages accounts.
type Service struct {
	s       *store.Store
	kr      *keyring.Keyring
	audit   *audit.Writer
	changes *changefeed.Recorder
}

// New returns the service.
func New(s *store.Store, kr *keyring.Keyring, aw *audit.Writer, ch *changefeed.Recorder) *Service {
	return &Service{s: s, kr: kr, audit: aw, changes: ch}
}

var (
	reName      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	errNotFound = apperr.New(apperr.CodeNotFound, "account not found")
)

func aad(id string) []byte { return []byte("account:" + id) }

func validSecret(s string) bool {
	if s == "" || len(s) > 8192 {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

// normalizeAuthJSON checks a ChatGPT login file and compacts it: a JSON object in ChatGPT mode (`auth_mode: chatgpt`
// with a `tokens` object, docs/contracts/harnesses.md). The rest of the file is Codex's and kept as it is.
func normalizeAuthJSON(s string) (string, bool) {
	if s == "" || len(s) > maxAuthJSON {
		return "", false
	}
	var f map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return "", false
	}
	var mode string
	if err := json.Unmarshal(f["auth_mode"], &mode); err != nil || mode != "chatgpt" {
		return "", false
	}
	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(f["tokens"], &tokens); err != nil || len(tokens) == 0 {
		return "", false
	}
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		return "", false
	}
	return b.String(), true
}

// validate checks an input; kind is the account's kind (the input's on create, the stored one on an edit).
func (in *Input) validate(create bool, kind string) error {
	v := &apperr.Validation{}
	in.Name = strings.TrimSpace(in.Name)
	if !reName.MatchString(in.Name) {
		v.Add("name", "must be 1-63 lowercase letters, digits, dots, dashes or underscores")
	}
	if create {
		if _, ok := KindOf(in.Kind); !ok {
			v.Add("kind", "must be claude_oauth_token, anthropic_api_key, openai_api_key or codex_chatgpt_login")
		}
	}
	switch {
	case !create && in.Secret == "":
	case kind == KindCodexChatGPT:
		if norm, ok := normalizeAuthJSON(in.Secret); ok {
			in.Secret = norm
		} else {
			v.Add("secret", "must be the auth.json of a ChatGPT login (codex login): a JSON object with auth_mode chatgpt and its tokens, at most 64 KiB")
		}
	case !validSecret(in.Secret):
		v.Add("secret", "is required: one line, no spaces, at most 8192 bytes")
	}
	if len(in.Description) > 2000 {
		v.Add("description", "at most 2000 characters")
	}
	if len(in.Metadata) > 20 {
		v.Add("metadata", "at most 20 entries")
	}
	for k, val := range in.Metadata {
		if len(k) > 64 || len(val) > 500 {
			v.Add("metadata", "keys up to 64 and values up to 500 characters")
			break
		}
	}
	return v.Err()
}

func fromRow(r db.Account, bindings int64) *Account {
	k, _ := KindOf(r.Kind)
	md := map[string]string{}
	_ = json.Unmarshal([]byte(r.MetadataJson), &md)
	return &Account{ID: r.ID, Name: r.Name, Kind: r.Kind, Harness: k.Harness, Description: r.Description, Metadata: md, SecretVersion: r.SecretVersion,
		SecretRotatedAt: time.UnixMicro(r.SecretRotatedAt).UTC(), Enabled: r.Enabled == 1, Spaces: bindings, CreatedAt: time.UnixMicro(r.CreatedAt).UTC(),
		UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), Version: r.Version}
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Create stores an account with its sealed secret.
func (svc *Service) Create(ctx context.Context, actor audit.Actor, in Input) (*Account, error) {
	if err := in.validate(true, in.Kind); err != nil {
		return nil, err
	}
	id := ids.New(ids.Account)
	sealed, err := svc.kr.Seal(keyring.PurposeCredential, aad(id), []byte(in.Secret))
	if err != nil {
		return nil, err
	}
	md, _ := json.Marshal(nonNil(in.Metadata))
	err = svc.s.W().TxNamed(ctx, "accounts.create", func(tx *store.Tx) error {
		if _, err := tx.Q.GetAccountByName(ctx, in.Name); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "an account named %q exists", in.Name)
		}
		if err := tx.Q.InsertAccount(ctx, db.InsertAccountParams{ID: id, Name: in.Name, Kind: in.Kind, Description: in.Description, MetadataJson: string(md),
			SecretEnc: sealed, SecretRotatedAt: tx.NowMicros(), Enabled: b2i(in.Enabled), CreatedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, "", "account", id, "created", 1); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "account.created", Target: audit.Target{Type: "account", ID: id},
			Details: map[string]any{"name": in.Name, "kind": in.Kind, "enabled": in.Enabled}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, id)
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// Get returns an account.
func (svc *Service) Get(ctx context.Context, id string) (*Account, error) {
	r, err := svc.s.R().GetAccount(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	} else if err != nil {
		return nil, err
	}
	n, err := svc.s.R().CountAccountBindings(ctx, id)
	if err != nil {
		return nil, err
	}
	return fromRow(r, n), nil
}

// List returns every account.
func (svc *Service) List(ctx context.Context) ([]*Account, error) {
	rows, err := svc.s.R().ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Account, 0, len(rows))
	for _, r := range rows {
		n, err := svc.s.R().CountAccountBindings(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, fromRow(r, n))
	}
	return out, nil
}

// Update edits an account. A non-empty secret rotates it: later launches use the new one, running harnesses keep the
// credential they started with (SM-ACCT-006). The kind never changes.
func (svc *Service) Update(ctx context.Context, actor audit.Actor, id string, version int64, in Input) (*Account, error) {
	cur, err := svc.s.R().GetAccount(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	} else if err != nil {
		return nil, err
	}
	if err := in.validate(false, cur.Kind); err != nil {
		return nil, err
	}
	var sealed []byte
	if in.Secret != "" {
		var err error
		if sealed, err = svc.kr.Seal(keyring.PurposeCredential, aad(id), []byte(in.Secret)); err != nil {
			return nil, err
		}
	}
	md, _ := json.Marshal(nonNil(in.Metadata))
	err = svc.s.W().TxNamed(ctx, "accounts.update", func(tx *store.Tx) error {
		cur, err := tx.Q.GetAccount(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		} else if err != nil {
			return err
		}
		if cur.Version != version {
			return apperr.New(apperr.CodePrecondition, "the account changed since version %d (now %d)", version, cur.Version)
		}
		if other, err := tx.Q.GetAccountByName(ctx, in.Name); err == nil && other.ID != id {
			return apperr.New(apperr.CodeAlreadyExists, "an account named %q exists", in.Name)
		}
		if _, err := tx.Q.UpdateAccount(ctx, db.UpdateAccountParams{Name: in.Name, Description: in.Description, MetadataJson: string(md), Enabled: b2i(in.Enabled),
			Now: tx.NowMicros(), ID: id, Version: version}); err != nil {
			return err
		}
		next := version + 1
		if sealed != nil {
			if _, err := tx.Q.RotateAccountSecret(ctx, db.RotateAccountSecretParams{SecretEnc: sealed, Now: tx.NowMicros(), ID: id, Version: next}); err != nil {
				return err
			}
			next++
		}
		if err := svc.changes.Record(ctx, tx, "", "account", id, "updated", next); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "account.updated", Target: audit.Target{Type: "account", ID: id},
			Details: map[string]any{"name": in.Name, "enabled": in.Enabled, "secret_rotated": sealed != nil}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, id)
}

// Delete removes an account and its bindings. Running harnesses keep the credential they started with.
func (svc *Service) Delete(ctx context.Context, actor audit.Actor, id string) error {
	return svc.s.W().TxNamed(ctx, "accounts.delete", func(tx *store.Tx) error {
		cur, err := tx.Q.GetAccount(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		} else if err != nil {
			return err
		}
		// The delete cascades away the account's bindings and pool rows: the spaces that had them change.
		if _, err := tx.SQL.ExecContext(ctx, `UPDATE spaces SET accounts_version = accounts_version + 1
			WHERE id IN (SELECT space_id FROM space_accounts WHERE account_id = ? UNION SELECT space_id FROM space_role_accounts WHERE account_id = ?)`, id, id); err != nil {
			return err
		}
		if _, err := tx.Q.DeleteAccount(ctx, id); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, "", "account", id, "deleted", cur.Version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "account.deleted", Target: audit.Target{Type: "account", ID: id},
			Details: map[string]any{"name": cur.Name}})
		return err
	})
}

// Bindings returns the accounts a space may use.
func (svc *Service) Bindings(ctx context.Context, spaceID string) ([]Binding, error) {
	return bindings(ctx, svc.s.R(), spaceID)
}

func bindings(ctx context.Context, q *db.Queries, spaceID string) ([]Binding, error) {
	rows, err := q.ListSpaceAccounts(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]Binding, 0, len(rows))
	for _, r := range rows {
		a, err := q.GetAccount(ctx, r.AccountID)
		if err != nil {
			return nil, err
		}
		out = append(out, Binding{AccountID: r.AccountID, Harness: r.Harness, Default: r.IsDefault == 1, Name: a.Name, Enabled: a.Enabled == 1, Priority: int(r.Priority)})
	}
	return out, nil
}

// Pools returns a space's role pools and the version of its account configuration (the If-Match value of every pool
// write).
func (svc *Service) Pools(ctx context.Context, spaceID string) ([]RolePool, int64, error) {
	q := svc.s.R()
	v, err := q.GetSpaceAccountsVersion(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, apperr.New(apperr.CodeNotFound, "space not found")
	} else if err != nil {
		return nil, 0, err
	}
	rows, err := q.ListSpaceRoleAccounts(ctx, spaceID)
	if err != nil {
		return nil, 0, err
	}
	out := []RolePool{}
	for _, r := range rows {
		a, err := q.GetAccount(ctx, r.AccountID)
		if err != nil {
			return nil, 0, err
		}
		if n := len(out); n == 0 || out[n-1].Role != r.Role {
			out = append(out, RolePool{Role: r.Role, Items: []PoolEntry{}})
		}
		out[len(out)-1].Items = append(out[len(out)-1].Items, PoolEntry{AccountID: r.AccountID, Name: a.Name, Priority: int(r.Priority)})
	}
	return out, v, nil
}

// AccountsVersion is the version of a space's account configuration.
func (svc *Service) AccountsVersion(ctx context.Context, spaceID string) (int64, error) {
	v, err := svc.s.R().GetSpaceAccountsVersion(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, apperr.New(apperr.CodeNotFound, "space not found")
	}
	return v, err
}

var reRole = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

// SetRolePool replaces the pool of one role (an empty list removes it, so the role uses the space pool again). Every
// account must be bound to the space for its harness. ifVersion, when above zero, must be the space's current account
// configuration version (a mismatch is a precondition failure).
func (svc *Service) SetRolePool(ctx context.Context, actor audit.Actor, spaceID, role string, in []PoolEntry, ifVersion int64) (int64, error) {
	if !reRole.MatchString(role) {
		return 0, apperr.New(apperr.CodeInvalid, "role must be lower-case letters, digits and dashes")
	}
	if len(in) > 50 {
		return 0, apperr.New(apperr.CodeInvalid, "at most 50 accounts in a pool")
	}
	var version int64
	err := svc.s.W().TxNamed(ctx, "accounts.role_pool", func(tx *store.Tx) error {
		if err := checkAccountsVersion(ctx, tx, spaceID, ifVersion); err != nil {
			return err
		}
		bound, err := tx.Q.ListSpaceAccounts(ctx, spaceID)
		if err != nil {
			return err
		}
		if err := tx.Q.DeleteSpaceRoleAccounts(ctx, db.DeleteSpaceRoleAccountsParams{SpaceID: spaceID, Role: role}); err != nil {
			return err
		}
		seen := map[string]bool{}
		ids := make([]string, 0, len(in))
		prios := map[string]int{}
		for i, e := range in {
			if seen[e.AccountID] {
				return apperr.New(apperr.CodeInvalid, "account %q is listed twice", e.AccountID)
			}
			seen[e.AccountID] = true
			if e.Priority < 0 || e.Priority > MaxPriority {
				return apperr.New(apperr.CodeInvalid, "priority is 0 to %d", MaxPriority)
			}
			if !slices.ContainsFunc(bound, func(b db.SpaceAccount) bool { return b.AccountID == e.AccountID }) {
				return apperr.New(apperr.CodeInvalid, "account %q is not bound to this space; bind it first", e.AccountID)
			}
			if err := tx.Q.InsertSpaceRoleAccount(ctx, db.InsertSpaceRoleAccountParams{SpaceID: spaceID, Role: role, AccountID: e.AccountID,
				Priority: int64(priorityOf(e.Priority)), CreatedAt: tx.NowMicros() + int64(i), CreatedBy: actor.ID}); err != nil {
				return err
			}
			ids = append(ids, e.AccountID)
			prios[e.AccountID] = priorityOf(e.Priority)
		}
		if version, err = tx.Q.BumpSpaceAccountsVersion(ctx, spaceID); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space", spaceID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space.role_accounts_bound", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"role": role, "accounts": ids, "priorities": prios}})
		return err
	})
	return version, err
}

// checkAccountsVersion refuses a write based on an older account configuration of the space. It also reports an
// unknown space.
func checkAccountsVersion(ctx context.Context, tx *store.Tx, spaceID string, ifVersion int64) error {
	cur, err := tx.Q.GetSpaceAccountsVersion(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return apperr.New(apperr.CodeNotFound, "space not found")
	} else if err != nil {
		return err
	}
	if ifVersion > 0 && ifVersion != cur {
		return apperr.New(apperr.CodePrecondition, "the space's accounts changed since version %d (now %d)", ifVersion, cur)
	}
	return nil
}

// SetBindings replaces a space's bindings. At most one default per harness.
func (svc *Service) SetBindings(ctx context.Context, actor audit.Actor, spaceID string, in []Binding) ([]Binding, error) {
	return svc.SetBindingsIf(ctx, actor, spaceID, in, 0)
}

// SetBindingsIf is SetBindings that, when ifVersion is above zero, refuses a space whose account configuration moved
// on since that version. The order of in is the binding order, and each Binding's Priority orders the pool (0 is 100).
// Role-pool entries of accounts that are no longer bound are dropped with them.
func (svc *Service) SetBindingsIf(ctx context.Context, actor audit.Actor, spaceID string, in []Binding, ifVersion int64) ([]Binding, error) {
	if len(in) > 50 {
		return nil, apperr.New(apperr.CodeInvalid, "at most 50 bindings")
	}
	err := svc.s.W().TxNamed(ctx, "accounts.bind", func(tx *store.Tx) error {
		if err := checkAccountsVersion(ctx, tx, spaceID, ifVersion); err != nil {
			return err
		}
		if err := tx.Q.DeleteSpaceAccounts(ctx, spaceID); err != nil {
			return err
		}
		defaults := map[string]bool{}
		seen := map[string]bool{}
		defaultIDs := []string{}
		prios := map[string]int{}
		for i, b := range in {
			if b.Priority < 0 || b.Priority > MaxPriority {
				return apperr.New(apperr.CodeInvalid, "priority is 0 to %d", MaxPriority)
			}
			a, err := tx.Q.GetAccount(ctx, b.AccountID)
			if err != nil {
				return apperr.New(apperr.CodeInvalid, "account %q not found", b.AccountID)
			}
			if seen[a.ID] {
				return apperr.New(apperr.CodeInvalid, "account %q is listed twice", a.Name)
			}
			seen[a.ID] = true
			k, _ := KindOf(a.Kind)
			if b.Default {
				if defaults[k.Harness] {
					return apperr.New(apperr.CodeInvalid, "only one default account per harness (%s)", k.Harness)
				}
				defaults[k.Harness] = true
				defaultIDs = append(defaultIDs, a.ID)
			}
			prios[a.ID] = priorityOf(b.Priority)
			// created_at carries the binding order: one microsecond apart in the order given.
			if err := tx.Q.InsertSpaceAccount(ctx, db.InsertSpaceAccountParams{SpaceID: spaceID, AccountID: a.ID, Harness: k.Harness, IsDefault: b2i(b.Default),
				Priority: int64(priorityOf(b.Priority)), CreatedAt: tx.NowMicros() + int64(i), CreatedBy: actor.ID}); err != nil {
				return err
			}
		}
		if err := tx.Q.DeleteOrphanSpaceRoleAccounts(ctx, spaceID); err != nil {
			return err
		}
		if _, err := tx.Q.BumpSpaceAccountsVersion(ctx, spaceID); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space", spaceID, "updated", 0); err != nil {
			return err
		}
		names := make([]string, 0, len(in))
		for _, b := range in {
			names = append(names, b.AccountID)
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space.accounts_bound", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"accounts": names, "defaults": defaultIDs, "priorities": prios}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Bindings(ctx, spaceID)
}

// Policy is what usage-limit failover needs to order a pool (ADR-0090 decision 3). A nil Avoid is failover off.
type Policy struct {
	// Avoid reports an account that must not be chosen (it holds an open usage limit). Set only when the space's
	// usage_limit_failover is on.
	Avoid func(accountID string) (bool, error)
	// Utilisation is the known utilisation, in percent, of the window that limited the run on an account; false when
	// unknown (unknown sorts last among equal priorities). Optional.
	Utilisation func(accountID string) (float64, bool)
}

// Resolve picks the account a launch uses, inside the launch transaction (SM-ACCT-003): the account the launch names
// (by ID or name) when the space binds it, otherwise the space's default for the harness, otherwise its only bound
// account for the harness. Disabled accounts are never chosen. It returns a clear error when nothing fits.
//
// avoid, when set, is usage-limit failover (ADR-0079 decision 8, a space setting that is off by default): when it
// avoids the account chosen above, the best other account of the pool that it doesn't avoid is used instead, ordered by
// pool priority, then fewest live runs, then binding order (ResolveWith adds utilisation). The pool is the role's pool
// when role has one, otherwise the space's bindings for the harness. When it avoids them all, the usual choice stands
// and the launch meets its hold. Failover never turns a configuration error into a choice: an unknown name or no
// default is still refused. With avoid nil nothing about pools applies: role pools and priorities are failover's.
func Resolve(ctx context.Context, tx *store.Tx, spaceID, harness, role, ref string, avoid func(accountID string) (bool, error)) (string, error) {
	return ResolveWith(ctx, tx, spaceID, harness, role, ref, Policy{Avoid: avoid})
}

// ResolveWith is Resolve with the whole failover policy.
func ResolveWith(ctx context.Context, tx *store.Tx, spaceID, harness, role, ref string, pol Policy) (string, error) {
	bs, err := bindings(ctx, tx.Q, spaceID)
	if err != nil {
		return "", err
	}
	var usable []Binding
	for _, b := range bs {
		if b.Harness == harness && b.Enabled {
			usable = append(usable, b)
		}
	}
	if pol.Avoid == nil {
		return pick(usable, harness, ref)
	}
	cands, scoped, err := candidates(ctx, tx.Q, spaceID, harness, role, bs, usable)
	if err != nil {
		return "", err
	}
	live, err := liveRuns(ctx, tx.Q)
	if err != nil {
		return "", err
	}
	slices.SortStableFunc(cands, func(a, b candidate) int {
		if a.priority != b.priority {
			return a.priority - b.priority
		}
		if pol.Utilisation != nil {
			ua, oka := pol.Utilisation(a.AccountID)
			ub, okb := pol.Utilisation(b.AccountID)
			switch {
			case oka && okb && ua != ub:
				if ua < ub {
					return -1
				}
				return 1
			case oka != okb:
				if oka {
					return -1
				}
				return 1
			}
		}
		if live[a.AccountID] != live[b.AccountID] {
			return live[a.AccountID] - live[b.AccountID]
		}
		return a.order - b.order
	})
	if ref == "" && scoped && len(cands) == 0 {
		// The role's pool has accounts for this harness but none is usable: never widen to the space's accounts.
		return "", apperr.New(apperr.CodePrecondition, "no enabled %s account of role %q's pool is usable; enable one or change the role's pool", harness, role)
	}
	chosen, err := pick(usable, harness, ref)
	if ref == "" && len(cands) > 0 && cands[0].fromRole {
		// A role with a pool uses the pool's best account, not the space's default.
		chosen, err = cands[0].AccountID, nil
	}
	if err != nil {
		return chosen, err
	}
	if held, err := pol.Avoid(chosen); err != nil || !held {
		return chosen, err
	}
	for _, c := range cands {
		if c.AccountID == chosen {
			continue
		}
		held, err := pol.Avoid(c.AccountID)
		if err != nil {
			return "", err
		}
		if !held {
			return c.AccountID, nil
		}
	}
	return chosen, nil
}

// candidate is an account of the pool in play: its priority there and its place in the space's binding order.
type candidate struct {
	Binding
	priority int
	order    int
	fromRole bool
}

// candidates is the pool of a run's role (usable accounts only, with the role's priorities) and whether the role's pool
// governs. A role pool governs when any of its accounts is bound to the space for the harness (usable or not): it is
// then those accounts only, even when none is usable. Only a role without such an entry uses the space's usable
// bindings (ADR-0090 §3.2). all is every binding of the space.
func candidates(ctx context.Context, q *db.Queries, spaceID, harness, role string, all, usable []Binding) ([]candidate, bool, error) {
	var out []candidate
	if role != "" {
		rows, err := q.ListSpaceRoleAccounts(ctx, spaceID)
		if err != nil {
			return nil, false, err
		}
		scoped := false
		for _, r := range rows {
			if r.Role != role {
				continue
			}
			if slices.ContainsFunc(all, func(b Binding) bool { return b.AccountID == r.AccountID && b.Harness == harness }) {
				scoped = true
			}
			if i := slices.IndexFunc(usable, func(b Binding) bool { return b.AccountID == r.AccountID }); i >= 0 {
				out = append(out, candidate{Binding: usable[i], priority: int(r.Priority), order: i, fromRole: true})
			}
		}
		if scoped {
			return out, true, nil
		}
	}
	for i, b := range usable {
		out = append(out, candidate{Binding: b, priority: priorityOf(b.Priority), order: i})
	}
	return out, false, nil
}

// liveRuns counts the live runs of each account across spaces: the tie-break after priority and utilisation.
func liveRuns(ctx context.Context, q *db.Queries) (map[string]int, error) {
	rows, err := q.CountLiveRunsByAccount(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]int, len(rows))
	for _, r := range rows {
		m[r.AccountID] = int(r.Live)
	}
	return m, nil
}

func pick(usable []Binding, harness, ref string) (string, error) {
	if ref != "" {
		for _, b := range usable {
			if b.AccountID == ref || b.Name == ref {
				return b.AccountID, nil
			}
		}
		return "", apperr.New(apperr.CodePrecondition, "account %q is not bound to this space for %s, or it is disabled", ref, harness)
	}
	for _, b := range usable {
		if b.Default {
			return b.AccountID, nil
		}
	}
	switch len(usable) {
	case 1:
		return usable[0].AccountID, nil
	case 0:
		return "", apperr.New(apperr.CodePrecondition, "no enabled %s account is bound to this space; add one under Accounts and bind it to the space", harness)
	default:
		return "", apperr.New(apperr.CodePrecondition, "this space has %d %s accounts and no default; mark one as the default or name one in the launch", len(usable), harness)
	}
}

// ErrUnavailable marks an account that can no longer be used at dispatch (deleted, disabled or rebound away).
var ErrUnavailable = errors.New("account unavailable")

// SecretEnv returns the environment a run's harness gets from its account at dispatch: the current (possibly rotated)
// secret, if the account is still enabled, compatible and bound to the space.
func (svc *Service) SecretEnv(ctx context.Context, spaceID, harness, accountID string) (map[string]string, error) {
	r, err := svc.s.R().GetAccount(ctx, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnavailable
	} else if err != nil {
		return nil, err
	}
	k, _ := KindOf(r.Kind)
	if r.Enabled != 1 || k.Harness != harness {
		return nil, ErrUnavailable
	}
	bs, err := svc.Bindings(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(bs, func(b Binding) bool { return b.AccountID == accountID }) {
		return nil, ErrUnavailable
	}
	secret, err := svc.kr.Open(keyring.PurposeCredential, aad(accountID), r.SecretEnc)
	if err != nil {
		return nil, err
	}
	return map[string]string{k.Env: string(secret)}, nil
}
