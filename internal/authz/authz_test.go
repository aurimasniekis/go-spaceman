package authz

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

type fakeLoader struct {
	rows []db.RoleGrant
	err  error
}

func (f *fakeLoader) ListRoleGrants(context.Context) ([]db.RoleGrant, error) { return f.rows, f.err }

const (
	spaceA = "spc_0000000000000000000000000a"
	spaceB = "spc_0000000000000000000000000b"
)

func grant(sub string, r Role, dom string) db.RoleGrant {
	return db.RoleGrant{Subject: sub, Role: string(r), Domain: dom, Source: "manual"}
}

func engine(t *testing.T, rows ...db.RoleGrant) *Engine {
	t.Helper()
	e, err := New(context.Background(), &fakeLoader{rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSpaceScopedRoles(t *testing.T) {
	e := engine(t,
		grant("user:v", RoleViewer, spaceA),
		grant("user:o", RoleOperator, spaceA),
		grant("user:a", RoleAdmin, spaceA),
		grant("user:g", RoleAdmin, Global),
		grant("user:gv", RoleViewer, Global),
	)
	cases := []struct {
		sub, dom string
		p        Permission
		want     bool
	}{
		{"user:v", spaceA, SpaceRead, true},
		{"user:v", spaceA, QueueWrite, false},
		{"user:v", spaceA, TerminalWatch, false}, // terminals are a separate, operator permission
		{"user:v", spaceB, SpaceRead, false},     // no grant in B: no leak
		{"user:o", spaceA, QueueWrite, true},
		{"user:o", spaceA, ApprovalDecide, true},
		{"user:o", spaceA, TerminalTakeover, false},
		{"user:o", spaceA, SecretWrite, false},
		{"user:o", spaceA, SpaceRead, true}, // inherits viewer
		{"user:a", spaceA, TerminalTakeover, true},
		{"user:a", spaceA, SecretWrite, true},
		{"user:a", spaceB, SpaceRead, false},
		{"user:a", Global, NodeWrite, false}, // space admin is not a global admin
		{"user:a", spaceA, NodeWrite, false}, // global-only permission requested in a space domain
		{"user:g", spaceB, TerminalTakeover, true},
		{"user:g", Global, NodeWrite, true},
		{"user:g", Global, UserWrite, true},
		{"user:gv", spaceB, SpaceRead, true}, // a global viewer sees every space
		{"user:gv", Global, NodeRead, true},
		{"user:gv", spaceB, QueueWrite, false},
		{"user:nobody", spaceA, SpaceRead, false},
		{"", spaceA, SpaceRead, false},
		{"user:g", "", SpaceRead, false},
		{"user:g", spaceA, Permission{}, false},
	}
	for _, c := range cases {
		if got := e.Allowed(c.sub, c.dom, c.p); got != c.want {
			t.Errorf("Allowed(%s, %s, %s) = %v, want %v", c.sub, c.dom, c.p, got, c.want)
		}
	}
}

func TestTokensBoundedByCreator(t *testing.T) {
	e := engine(t,
		grant("user:u", RoleOperator, spaceA),
		grant("token:t", RoleOperator, spaceA),
		grant("token:t", RoleAdmin, Global), // inconsistent data: the creator check still bounds it
	)
	subs := []string{"token:t", "user:u"}
	if !e.AllowedAll(subs, spaceA, QueueWrite) {
		t.Error("token within creator's grants denied")
	}
	if e.AllowedAll(subs, Global, NodeWrite) {
		t.Error("token exceeded its creator")
	}
	if e.AllowedAll(nil, spaceA, SpaceRead) {
		t.Error("no subjects must deny")
	}
}

func TestReloadAppliesRevocation(t *testing.T) {
	l := &fakeLoader{rows: []db.RoleGrant{grant("user:u", RoleAdmin, Global)}}
	e, err := New(context.Background(), l)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Allowed("user:u", Global, UserWrite) {
		t.Fatal("grant not loaded")
	}
	l.rows = nil
	if err := e.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.Allowed("user:u", Global, UserWrite) {
		t.Error("revoked grant still effective after reload")
	}
	l.err = errors.New("db down")
	if err := e.Reload(context.Background()); err == nil {
		t.Error("loader error not reported")
	}
}

func TestInvalidRowsIgnored(t *testing.T) {
	e := engine(t,
		db.RoleGrant{Subject: "user:x", Role: "superuser", Domain: Global},
		db.RoleGrant{Subject: "user:y", Role: "admin", Domain: "spc_*"},
	)
	if e.Allowed("user:x", Global, SpaceRead) || e.Allowed("user:y", spaceA, SpaceRead) {
		t.Error("invalid grant rows widened access")
	}
}

func TestDomainsAndRoles(t *testing.T) {
	e := engine(t, grant("user:u", RoleViewer, spaceB), grant("user:u", RoleOperator, spaceA), grant("user:u", RoleViewer, Global))
	if got := e.Domains("user:u"); !slices.Equal(got, []string{Global, spaceA, spaceB}) {
		t.Errorf("Domains = %v", got)
	}
	if got := e.RolesIn("user:u", spaceA); !slices.Equal(got, []Role{RoleOperator, RoleViewer}) {
		t.Errorf("RolesIn = %v", got)
	}
}

func TestCovers(t *testing.T) {
	holder := []Grant{{Role: RoleOperator, Domain: spaceA}, {Role: RoleViewer, Domain: Global}}
	for _, c := range []struct {
		want Grant
		ok   bool
	}{
		{Grant{Role: RoleViewer, Domain: spaceA}, true},
		{Grant{Role: RoleOperator, Domain: spaceA}, true},
		{Grant{Role: RoleAdmin, Domain: spaceA}, false},
		{Grant{Role: RoleViewer, Domain: spaceB}, true}, // global viewer covers any space
		{Grant{Role: RoleOperator, Domain: spaceB}, false},
		{Grant{Role: RoleViewer, Domain: Global}, true},
		{Grant{Role: "bogus", Domain: spaceA}, false},
	} {
		if got := Covers(holder, c.want); got != c.ok {
			t.Errorf("Covers(%+v) = %v", c.want, got)
		}
	}
}

func TestReadOnlyAdapter(t *testing.T) {
	a := &adapter{}
	if a.SavePolicy(nil) == nil || a.AddPolicy("p", "p", nil) == nil || a.RemovePolicy("p", "p", nil) == nil || a.RemoveFilteredPolicy("p", "p", 0) == nil {
		t.Error("writes must be refused")
	}
}

func TestPermissionsCatalog(t *testing.T) {
	if p := Permissions(RoleOperator); !slices.Contains(p, SpaceRead) || !slices.Contains(p, QueueWrite) || slices.Contains(p, TerminalTakeover) {
		t.Errorf("operator permissions = %v", p)
	}
	if !ValidDomain(spaceA) || ValidDomain("spc_x") || !ValidDomain(Global) {
		t.Error("ValidDomain")
	}
}

// An SSO subject ("user:<id>#sso") holds its own grants and its user's; the user subject never gains the SSO ones
// (ADR-0029, Hallpass SR-26).
func TestSSOSubject(t *testing.T) {
	e := engine(t, grant("user:u", RoleViewer, Global), db.RoleGrant{Subject: "user:u" + SSOSuffix, Role: string(RoleOperator), Domain: spaceA, Source: "sso"},
		db.RoleGrant{Subject: "token:t" + SSOSuffix, Role: string(RoleAdmin), Domain: Global, Source: "sso"})
	sso := "user:u" + SSOSuffix
	if !e.Allowed(sso, spaceA, QueueWrite) || !e.Allowed(sso, spaceB, SpaceRead) || e.Allowed(sso, spaceB, QueueWrite) {
		t.Error("SSO subject grants")
	}
	if e.Allowed("user:u", spaceA, QueueWrite) {
		t.Error("user subject gained an SSO grant")
	}
	if got := e.Domains(sso); !slices.Equal(got, []string{Global, spaceA}) {
		t.Errorf("Domains = %v", got)
	}
	if got := e.RolesIn(sso, spaceA); !slices.Equal(got, []Role{RoleOperator, RoleViewer}) {
		t.Errorf("RolesIn = %v", got)
	}
	// Only user subjects expand: a token subject with the suffix is just an (unused) subject of its own.
	if e.Allowed("token:t", Global, NodeWrite) || len(expand("token:t"+SSOSuffix)) != 1 {
		t.Error("non-user SSO suffix expanded")
	}
}

// SM-CTRL-004: diagnostic launches need run:diagnose on top of run:control. Operators and admins keep both (the GOAL
// lets operators launch audited diagnostics); viewers have neither. Type permissions (SM-HUI-008) are admin-only.
func TestDiagnoseAndTypePermissionRoles(t *testing.T) {
	e := engine(t, grant("user:v", RoleViewer, spaceA), grant("user:o", RoleOperator, spaceA), grant("user:a", RoleAdmin, spaceA))
	for _, c := range []struct {
		sub  string
		p    Permission
		want bool
	}{
		{"user:v", RunDiag, false}, {"user:v", RunControl, false},
		{"user:o", RunDiag, true}, {"user:o", RunControl, true},
		{"user:a", RunDiag, true},
		{"user:o", HumanTypePermissionWrite, false}, {"user:a", HumanTypePermissionWrite, true},
	} {
		if got := e.Allowed(c.sub, spaceA, c.p); got != c.want {
			t.Errorf("Allowed(%s, %s) = %v, want %v", c.sub, c.p, got, c.want)
		}
	}
}

// HoldsRole: every subject needs the role or a stronger one in the domain or globally (a token never exceeds its user).
func TestHoldsRole(t *testing.T) {
	e := engine(t, grant("user:o", RoleOperator, spaceA), grant("user:g", RoleAdmin, Global), grant("token:t", RoleViewer, spaceA),
		grant("user:v", RoleViewer, spaceA))
	for _, c := range []struct {
		subs []string
		dom  string
		r    Role
		want bool
	}{
		{[]string{"user:o"}, spaceA, RoleViewer, true},
		{[]string{"user:o"}, spaceA, RoleOperator, true},
		{[]string{"user:o"}, spaceA, RoleAdmin, false},
		{[]string{"user:o"}, spaceB, RoleViewer, false},
		{[]string{"user:g"}, spaceB, RoleAdmin, true},
		{[]string{"token:t", "user:o"}, spaceA, RoleOperator, false}, // the token is only a viewer
		{[]string{"user:v"}, spaceA, RoleOperator, false},
		{nil, spaceA, RoleViewer, false},
		{[]string{"user:o"}, spaceA, "bogus", false},
	} {
		if got := e.HoldsRole(c.subs, c.dom, c.r); got != c.want {
			t.Errorf("HoldsRole(%v, %s, %s) = %v, want %v", c.subs, c.dom, c.r, got, c.want)
		}
	}
}

// RoleHas matches the policy: admins hold everything, operators inherit viewers', unknown roles hold nothing.
func TestRoleHas(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		r    Role
		p    Permission
		want bool
	}{
		{RoleViewer, SpaceRead, true}, {RoleViewer, QueueWrite, false}, {RoleOperator, SpaceRead, true},
		{RoleOperator, QueueWrite, true}, {RoleOperator, NodeWrite, false}, {RoleAdmin, NodeWrite, true},
		{RoleAdmin, TerminalTakeover, true}, {Role("owner"), SpaceRead, false},
	} {
		if got := RoleHas(tc.r, tc.p); got != tc.want {
			t.Errorf("RoleHas(%s, %s) = %v", tc.r, tc.p, got)
		}
	}
}
