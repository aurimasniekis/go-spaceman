package accounts

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// liveRun records a running run of the space on the account (the launch spec carries the account).
func (f *fx) liveRun(id, account string) {
	f.t.Helper()
	must(f.t, f.s.W().TxNamed(f.ctx, "run", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
			VALUES (?, 'spc_a', 'nod_1', 1, 'worker', 'w', 'claude-code', ?, 'running', 'running', ?, 0, 't', 0)`, id, "ins_"+id, fmt.Sprintf(`{"account_ref":%q}`, account))
		return err
	}))
}

func (f *fx) resolvePolicy(role, ref string, pol Policy) (string, error) {
	var id string
	err := f.s.W().TxNamed(f.ctx, "resolve", func(tx *store.Tx) error {
		var err error
		id, err = ResolveWith(f.ctx, tx, "spc_a", "claude-code", role, ref, pol)
		return err
	})
	return id, err
}

// ADR-0090 decision 3: with failover on, the replacement for a held account is the best of the pool by priority, then
// known utilisation, then live runs, then binding order. A role's pool replaces the space pool for that role.
func TestResolvePools(t *testing.T) {
	type tc struct {
		name     string
		bind     []Binding           // by account name, in binding order
		pool     map[string][]string // role -> account names ("name=priority")
		disabled []string            // accounts disabled before resolving
		held     []string            // accounts with an open hold
		live     map[string]int      // live runs per account name
		util     map[string]float64  // known utilisation per account name
		role     string
		ref      string
		off      bool // failover off (no Avoid)
		want     string
		wantErr  apperr.Code
	}
	cases := []tc{
		{name: "nothing held keeps the default", bind: []Binding{{Name: "a"}, {Name: "b", Default: true}, {Name: "c"}}, want: "b"},
		{name: "priority beats binding order", bind: []Binding{{Name: "a", Default: true, Priority: 50}, {Name: "b", Priority: 30}, {Name: "c", Priority: 10}},
			held: []string{"a"}, want: "c"},
		{name: "equal priority: lower utilisation first", bind: []Binding{{Name: "a", Default: true}, {Name: "b"}, {Name: "c"}},
			held: []string{"a"}, util: map[string]float64{"b": 80, "c": 20}, want: "c"},
		{name: "unknown utilisation sorts last among equals", bind: []Binding{{Name: "a", Default: true}, {Name: "b"}, {Name: "c"}},
			held: []string{"a"}, util: map[string]float64{"c": 90}, want: "c"},
		{name: "priority beats utilisation", bind: []Binding{{Name: "a", Default: true}, {Name: "b", Priority: 20}, {Name: "c", Priority: 10}},
			held: []string{"a"}, util: map[string]float64{"b": 1, "c": 99}, want: "c"},
		{name: "tie on utilisation: fewest live runs", bind: []Binding{{Name: "a", Default: true}, {Name: "b"}, {Name: "c"}},
			held: []string{"a"}, util: map[string]float64{"b": 40, "c": 40}, live: map[string]int{"b": 2, "c": 1}, want: "c"},
		{name: "full tie: binding order", bind: []Binding{{Name: "a", Default: true}, {Name: "b"}, {Name: "c"}}, held: []string{"a"}, want: "b"},
		{name: "held accounts are skipped", bind: []Binding{{Name: "a", Default: true}, {Name: "b"}, {Name: "c"}}, held: []string{"a", "b"}, want: "c"},
		{name: "disabled accounts are never chosen", bind: []Binding{{Name: "a", Default: true}, {Name: "b", Priority: 1}, {Name: "c"}},
			held: []string{"a"}, disabled: []string{"b"}, want: "c"},
		{name: "everything held: the usual choice stands", bind: []Binding{{Name: "a", Default: true}, {Name: "b"}}, held: []string{"a", "b"}, want: "a"},
		{name: "named and held fails over", bind: []Binding{{Name: "a", Default: true}, {Name: "b"}, {Name: "c"}}, ref: "b", held: []string{"b"}, want: "a"},
		{name: "named account outside the pool is an error", bind: []Binding{{Name: "a", Default: true}}, ref: "zzz", held: []string{"a"}, wantErr: apperr.CodePrecondition},
		{name: "role pool replaces the space pool", bind: []Binding{{Name: "a", Default: true}, {Name: "b", Priority: 1}, {Name: "c"}, {Name: "d"}},
			pool: map[string][]string{"rev": {"c=20", "d=10"}}, role: "rev", held: []string{"c"}, want: "d"},
		{name: "role pool without a default uses its best account", bind: []Binding{{Name: "a", Default: true}, {Name: "c"}, {Name: "d"}},
			pool: map[string][]string{"rev": {"c=20", "d=10"}}, role: "rev", want: "d"},
		{name: "role pool priorities are the role's own", bind: []Binding{{Name: "a", Default: true}, {Name: "c", Priority: 1}, {Name: "d", Priority: 2}},
			pool: map[string][]string{"rev": {"c=30", "d=10"}}, role: "rev", held: []string{"a"}, ref: "a", want: "d"},
		{name: "another role uses the space pool", bind: []Binding{{Name: "a", Default: true}, {Name: "b", Priority: 1}, {Name: "c"}},
			pool: map[string][]string{"rev": {"c"}}, role: "dev", held: []string{"a"}, want: "b"},
		{name: "no role uses the space pool", bind: []Binding{{Name: "a", Default: true}, {Name: "b", Priority: 1}, {Name: "c"}},
			pool: map[string][]string{"rev": {"c"}}, held: []string{"a"}, want: "b"},
		{name: "role pool of disabled accounts falls back to the space pool", bind: []Binding{{Name: "a", Default: true}, {Name: "b"}, {Name: "c"}},
			pool: map[string][]string{"rev": {"c"}}, role: "rev", disabled: []string{"c"}, held: []string{"a"}, want: "b"},
		{name: "role pool all held: usual choice stands", bind: []Binding{{Name: "a", Default: true}, {Name: "c"}},
			pool: map[string][]string{"rev": {"c"}}, role: "rev", held: []string{"c"}, want: "c"},
		{name: "failover off ignores pools and priorities", bind: []Binding{{Name: "a", Default: true, Priority: 900}, {Name: "b", Priority: 1}},
			pool: map[string][]string{"rev": {"b"}}, role: "rev", off: true, want: "a"},
		{name: "failover off without a default still refuses", bind: []Binding{{Name: "a"}, {Name: "b"}}, off: true, wantErr: apperr.CodePrecondition},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFx(t)
			ids := map[string]string{}
			var bs []Binding
			for _, b := range c.bind {
				a := f.add(b.Name, "claude_oauth_token", "tok-"+b.Name)
				ids[b.Name] = a.ID
				b.AccountID = a.ID
				bs = append(bs, b)
			}
			_, err := f.svc.SetBindings(f.ctx, admin, "spc_a", bs)
			must(t, err)
			for role, names := range c.pool {
				var es []PoolEntry
				for _, n := range names {
					name, prio, _ := strings.Cut(n, "=")
					p := 0
					if prio != "" {
						fmt.Sscan(prio, &p)
					}
					es = append(es, PoolEntry{AccountID: ids[name], Priority: p})
				}
				_, err := f.svc.SetRolePool(f.ctx, admin, "spc_a", role, es, 0)
				must(t, err)
			}
			for _, n := range c.disabled {
				a, err := f.svc.Get(f.ctx, ids[n])
				must(t, err)
				_, err = f.svc.Update(f.ctx, admin, a.ID, a.Version, Input{Name: a.Name, Enabled: false})
				must(t, err)
			}
			held := map[string]bool{}
			for _, n := range c.held {
				held[ids[n]] = true
			}
			i := 0
			for n, k := range c.live {
				for j := 0; j < k; j++ {
					i++
					f.liveRun(fmt.Sprintf("run_%d", i), ids[n])
				}
			}
			pol := Policy{Avoid: func(id string) (bool, error) { return held[id], nil },
				Utilisation: func(id string) (float64, bool) {
					for n, u := range c.util {
						if ids[n] == id {
							return u, true
						}
					}
					return 0, false
				}}
			if c.off {
				pol = Policy{}
			}
			ref := c.ref
			if r, ok := ids[ref]; ok {
				ref = r
			}
			got, err := f.resolvePolicy(c.role, ref, pol)
			if c.wantErr != "" {
				code(t, err, c.wantErr)
				return
			}
			must(t, err)
			if got != ids[c.want] {
				t.Fatalf("got %s, want %s (%s)", name(ids, got), c.want, ids[c.want])
			}
		})
	}
}

func name(ids map[string]string, id string) string {
	for n, v := range ids {
		if v == id {
			return n
		}
	}
	return id
}

// A role pool only holds accounts bound to the space, unbinding drops them, and writes honour If-Match.
func TestRolePoolWrites(t *testing.T) {
	f := newFx(t)
	a, b, c := f.add("a", "claude_oauth_token", "ta"), f.add("b", "claude_oauth_token", "tb"), f.add("c", "claude_oauth_token", "tc")
	_, err := f.svc.SetBindings(f.ctx, admin, "spc_a", []Binding{{AccountID: a.ID, Default: true}, {AccountID: b.ID, Priority: 7}})
	must(t, err)
	_, v, err := f.svc.Pools(f.ctx, "spc_a")
	must(t, err)
	_, err = f.svc.SetRolePool(f.ctx, admin, "spc_a", "rev", []PoolEntry{{AccountID: c.ID}}, v)
	code(t, err, apperr.CodeInvalid) // not bound
	_, err = f.svc.SetRolePool(f.ctx, admin, "spc_a", "Bad Role", nil, v)
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.SetRolePool(f.ctx, admin, "spc_a", "rev", []PoolEntry{{AccountID: b.ID}, {AccountID: b.ID}}, v)
	code(t, err, apperr.CodeInvalid)
	v2, err := f.svc.SetRolePool(f.ctx, admin, "spc_a", "rev", []PoolEntry{{AccountID: b.ID}, {AccountID: a.ID, Priority: 3}}, v)
	must(t, err)
	if v2 != v+1 {
		t.Fatalf("version %d after %d", v2, v)
	}
	_, err = f.svc.SetRolePool(f.ctx, admin, "spc_a", "rev", nil, v) // stale
	code(t, err, apperr.CodePrecondition)
	_, err = f.svc.SetBindingsIf(f.ctx, admin, "spc_a", nil, v) // stale
	code(t, err, apperr.CodePrecondition)
	pools, _, err := f.svc.Pools(f.ctx, "spc_a")
	must(t, err)
	if len(pools) != 1 || pools[0].Role != "rev" || len(pools[0].Items) != 2 || pools[0].Items[0].AccountID != a.ID || pools[0].Items[0].Priority != 3 || pools[0].Items[1].Priority != DefaultPriority {
		t.Fatalf("pools %+v", pools)
	}
	// Rebinding without b drops it from the pool; its priority 7 is kept for a.
	bs, err := f.svc.SetBindingsIf(f.ctx, admin, "spc_a", []Binding{{AccountID: a.ID, Priority: 9}}, v2)
	must(t, err)
	if len(bs) != 1 || bs[0].Priority != 9 {
		t.Fatalf("bindings %+v", bs)
	}
	pools, _, _ = f.svc.Pools(f.ctx, "spc_a")
	if len(pools) != 1 || len(pools[0].Items) != 1 || pools[0].Items[0].AccountID != a.ID {
		t.Fatalf("pool after unbinding: %+v", pools)
	}
	// An empty list removes the pool.
	_, v3, _ := f.svc.Pools(f.ctx, "spc_a")
	_, err = f.svc.SetRolePool(f.ctx, admin, "spc_a", "rev", nil, v3)
	must(t, err)
	if pools, _, _ = f.svc.Pools(f.ctx, "spc_a"); len(pools) != 0 {
		t.Fatalf("pool kept: %+v", pools)
	}
	_, err = f.svc.SetRolePool(f.ctx, admin, "spc_nope", "rev", nil, 0)
	code(t, err, apperr.CodeNotFound)
}

// The audit record of a binding change carries the defaults and priorities.
func TestBindingAuditDetails(t *testing.T) {
	f := newFx(t)
	a, b := f.add("a", "claude_oauth_token", "ta"), f.add("b", "claude_oauth_token", "tb")
	_, err := f.svc.SetBindings(f.ctx, admin, "spc_a", []Binding{{AccountID: a.ID, Default: true}, {AccountID: b.ID, Priority: 10}})
	must(t, err)
	var details string
	must(t, f.s.W().TxNamed(f.ctx, "audit", func(tx *store.Tx) error {
		return tx.SQL.QueryRowContext(f.ctx, `SELECT details_json FROM audit_events WHERE action = 'space.accounts_bound'`).Scan(&details)
	}))
	for _, want := range []string{a.ID, b.ID, `"defaults"`, `"priorities"`, `:10`, `:100`} {
		if !strings.Contains(details, want) {
			t.Fatalf("details %s lacks %s", details, want)
		}
	}
}
