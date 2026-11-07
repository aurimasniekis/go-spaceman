package app_test

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hpfake"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// lockedBuffer collects central's logs.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// SM-GW-001/002 over HTTP (ADR-0060 decision 2): who may manage the Harbor Pilot connection and a space's settings.
// The connection needs account:write (global administrators and the root token, never a space's own admin), reading
// it account:read; a space's settings are space configuration: its viewers read them, its operators and admins change
// them, and another space's operator does neither. Writes are If-Match-checked.
func TestHarborPilotPermissionsOverHTTP(t *testing.T) {
	h := newHarness(t)
	f := hpfake.New(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, _ := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	a, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "bravo", Name: "Bravo", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	user := func(email string, grants ...auth.GrantInput) *client {
		c := h.client()
		c.bearer = userToken(t, h.c.Auth, email, grants)
		return c
	}
	global := user("gina@example.com", auth.GrantInput{Role: authz.RoleAdmin, Domain: "*"})
	spaceAdmin := user("sam@example.com", auth.GrantInput{Role: authz.RoleAdmin, Domain: a.ID})
	op := user("oscar@example.com", auth.GrantInput{Role: authz.RoleOperator, Domain: a.ID})
	viewer := user("vic@example.com", auth.GrantInput{Role: authz.RoleViewer, Domain: a.ID})
	other := user("olga@example.com", auth.GrantInput{Role: authz.RoleOperator, Domain: b.ID})

	conn := map[string]any{"base_url": f.URL(), "allow_insecure_http": true, "parent_token": f.Parent}
	for _, c := range []*client{spaceAdmin, op, viewer, other} {
		expect(t, c.do("GET", "/harbor-pilot/connection", nil), 403)
		expect(t, c.do("PUT", "/harbor-pilot/connection", conn), 403)
	}
	expect(t, root.do("GET", "/harbor-pilot/connection", nil), 404)
	r := global.do("PUT", "/harbor-pilot/connection", conn)
	expect(t, r, 200)
	if r.header.Get("ETag") != `"1"` {
		t.Fatalf("etag %q", r.header.Get("ETag"))
	}
	expect(t, root.do("PUT", "/harbor-pilot/connection", conn), 409)                    // exists: replace with If-Match
	expect(t, root.do("PUT", "/harbor-pilot/connection", conn, "If-Match", `"7"`), 409) // stale
	expect(t, root.do("PUT", "/harbor-pilot/connection", conn, "If-Match", `"1"`), 200)
	expect(t, global.do("GET", "/harbor-pilot/connection", nil), 200)
	// A new gateway address without the tokens entered again is refused before any call: stored tokens never go there.
	elsewhere := hpfake.New(t)
	r = root.do("PUT", "/harbor-pilot/connection", map[string]any{"base_url": elsewhere.URL(), "allow_insecure_http": true}, "If-Match", `"2"`)
	if r.status != 422 || !strings.Contains(string(r.body), "re-enter the tokens when the gateway URL changes") || elsewhere.Count() != 0 {
		t.Fatalf("address change without tokens: %d %s (%d requests elsewhere)", r.status, r.body, elsewhere.Count())
	}
	for _, c := range []*client{spaceAdmin, op, viewer} {
		expect(t, c.do("DELETE", "/harbor-pilot/connection", nil, "If-Match", `"2"`), 403)
	}

	// The project decides who pays: setting or changing it needs account:write (ADR-0060 decision 2, the operator's
	// decision of 2026-11-07), so a space's operators and its own admins can't, and no probe reaches Harbor Pilot.
	base := "/spaces/" + a.ID + "/harbor-pilot"
	settings := map[string]any{"enabled": true, "project_id": "prj_web"}
	expect(t, viewer.do("GET", base, nil), 200)
	expect(t, other.do("GET", base, nil), 404) // another space is invisible
	expect(t, viewer.do("PUT", base, settings), 403)
	expect(t, other.do("PUT", base, settings), 404)
	probes := f.Count()
	expect(t, op.do("PUT", base, settings), 403)
	expect(t, spaceAdmin.do("PUT", base, settings), 403)
	if f.Count() != probes {
		t.Fatal("a refused project write probed Harbor Pilot")
	}
	r = global.do("PUT", base, settings)
	expect(t, r, 200)
	var sp harborpilot.SpaceSettings
	r.json(t, &sp)
	if !sp.Enabled || sp.ProjectID != "prj_web" || sp.TTLSeconds != 86400 || r.header.Get("ETag") != `"1"` {
		t.Fatalf("settings %+v %q", sp, r.header.Get("ETag"))
	}
	expect(t, spaceAdmin.do("PUT", base, settings), 409)
	expect(t, spaceAdmin.do("PUT", base, map[string]any{"enabled": true, "project_id": "prj_web", "ttl_seconds": 600}, "If-Match", `"1"`), 422)
	expect(t, spaceAdmin.do("PUT", base, map[string]any{"enabled": true, "project_id": "prj_web", "on_budget_status_unavailable": "fallback"}, "If-Match", `"1"`), 422)
	// The rest of the settings are space configuration: the same project, or none sent, is kept.
	expect(t, spaceAdmin.do("PUT", base, map[string]any{"enabled": true, "project_id": "prj_web", "ttl_seconds": 3600}, "If-Match", `"1"`), 200)
	r = op.do("PUT", base, map[string]any{"enabled": true, "ttl_seconds": 7200}, "If-Match", `"2"`)
	expect(t, r, 200)
	r.json(t, &sp)
	if sp.ProjectID != "prj_web" || sp.TTLSeconds != 7200 {
		t.Fatalf("omitted project %+v", sp)
	}
	for _, c := range []*client{op, spaceAdmin} {
		expect(t, c.do("PUT", base, map[string]any{"enabled": true, "project_id": "prj_data"}, "If-Match", `"3"`), 403)
	}
	expect(t, root.do("PUT", base, map[string]any{"enabled": true, "project_id": "prj_data"}, "If-Match", `"3"`), 200)
	expect(t, global.do("PUT", base, map[string]any{"enabled": true, "project_id": "prj_web"}, "If-Match", `"4"`), 200)
	expect(t, op.do("PUT", base, map[string]any{"enabled": false}, "If-Match", `"5"`), 200)
	expect(t, op.do("PUT", base, map[string]any{"enabled": true}, "If-Match", `"6"`), 200)

	// Deleting the connection is refused while a space routes runs through it.
	expect(t, root.do("DELETE", "/harbor-pilot/connection", nil), 428)
	if r := root.do("DELETE", "/harbor-pilot/connection", nil, "If-Match", `"2"`); r.status != 409 || !strings.Contains(string(r.body), "route runs through Harbor Pilot") {
		t.Fatalf("delete while enabled: %d %s", r.status, r.body)
	}
	expect(t, viewer.do("DELETE", base, nil, "If-Match", `"7"`), 403)
	expect(t, op.do("DELETE", base, nil), 428)
	expect(t, op.do("DELETE", base, nil, "If-Match", `"7"`), 204)
	expect(t, global.do("DELETE", "/harbor-pilot/connection", nil, "If-Match", `"2"`), 204)
	expect(t, root.do("GET", "/harbor-pilot/connection", nil), 404)
}

// SM-GW-001 acceptance: a canary parent token and service-principal token are in no API answer, audit entry (API or
// table), change-feed row, space export, log line, or plain-text database file; Harbor Pilot's real refusal reasons
// are readable.
func TestHarborPilotCanaryTokensLeakNowhere(t *testing.T) {
	logs := &lockedBuffer{}
	h := newHarnessLogged(t, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), nil)
	f := hpfake.New(t)
	const parent, service = "hpk_CANARYparentZZ0123456789", "hpa_CANARYserviceZZ0123456789"
	f.Set(func(f *hpfake.HP) { f.Parent, f.Service = parent, service })
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, _ := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	sp, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	call := func(method, path string, body any, want int, hdr ...string) resp {
		t.Helper()
		r := root.do(method, path, body, hdr...)
		expect(t, r, want)
		bodies = append(bodies, string(r.body))
		return r
	}

	// Refusals first: a parent without tokens:mint, then a gateway older than 0.1.3.
	f.Set(func(f *hpfake.HP) { f.ParentScopes = []string{"gateway:invoke", "gateway:models"} })
	r := call("PUT", "/harbor-pilot/connection", map[string]any{"base_url": f.URL(), "allow_insecure_http": true, "parent_token": parent}, 422)
	if !strings.Contains(string(r.body), "parent token lacks tokens:mint") {
		t.Fatalf("refusal %s", r.body)
	}
	f.Set(func(f *hpfake.HP) {
		f.ParentScopes = []string{"gateway:invoke", "gateway:models", "tokens:mint"}
		f.Old = true
	})
	r = call("PUT", "/harbor-pilot/connection", map[string]any{"base_url": f.URL(), "allow_insecure_http": true, "parent_token": parent}, 422)
	if !strings.Contains(string(r.body), "older than Harbor Pilot 0.1.3") {
		t.Fatalf("old gateway %s", r.body)
	}
	f.Set(func(f *hpfake.HP) { f.Old = false })

	call("PUT", "/harbor-pilot/connection", map[string]any{"base_url": f.URL(), "allow_insecure_http": true, "parent_token": parent, "service_token": service}, 200)
	call("PUT", "/harbor-pilot/connection", map[string]any{"base_url": f.URL(), "allow_insecure_http": true, "parent_token": parent}, 200, "If-Match", `"1"`)
	call("GET", "/harbor-pilot/connection", nil, 200)
	call("PUT", "/spaces/"+sp.ID+"/harbor-pilot", map[string]any{"enabled": true, "project_id": "prj_data", "harnesses": []string{"codex"}}, 200)
	call("GET", "/spaces/"+sp.ID+"/harbor-pilot", nil, 200)
	call("GET", "/audit?limit=200", nil, 200)
	call("GET", "/openapi.json", nil, 200)

	needles := []string{parent, service, "CANARY"}
	for i, b := range bodies {
		for _, n := range needles {
			if strings.Contains(b, n) {
				t.Fatalf("answer %d contains %q: %s", i, n, b)
			}
		}
	}
	db := h.c.Store.ReadDB()
	for _, q := range []string{
		`SELECT count(*) FROM audit_events WHERE details_json LIKE '%CANARY%' OR target_id LIKE '%CANARY%'`,
		`SELECT count(*) FROM changes WHERE resource_id LIKE '%CANARY%' OR action LIKE '%CANARY%'`,
		`SELECT count(*) FROM harbor_pilot_connection WHERE CAST(parent_token_enc AS TEXT) LIKE '%CANARY%' OR CAST(service_token_enc AS TEXT) LIKE '%CANARY%'`,
	} {
		var n int
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d %v", q, n, err)
		}
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE action LIKE 'harbor_pilot.%'`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("audit entries %d %v", n, err)
	}

	// The space's export.
	ex := root.raw("GET", "/spaces/"+sp.ID+"/export", nil)
	expect(t, ex, 200)
	zr, err := zip.NewReader(bytes.NewReader(ex.body), int64(len(ex.body)))
	if err != nil {
		t.Fatal(err)
	}
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, _ := io.ReadAll(rc)
		_ = rc.Close()
		if bytes.Contains(content, []byte("CANARY")) {
			t.Fatalf("export file %s contains a token", zf.Name)
		}
	}

	// An internal failure is logged: a sealed token swapped into the other slot no longer opens, and the 500's log line
	// names the failure, never a token.
	if err := h.c.Store.W().TxNamed(ctx, "tamper", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE harbor_pilot_connection SET service_token_enc = parent_token_enc`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	call("PUT", "/harbor-pilot/connection", map[string]any{"base_url": f.URL(), "allow_insecure_http": true}, 500, "If-Match", `"2"`)
	if !strings.Contains(logs.String(), "open the service-principal token") {
		t.Fatalf("the failure was not logged: %s", logs.String())
	}
	if strings.Contains(logs.String(), "CANARY") {
		t.Fatal("a token is in central's logs")
	}

	// The database files on disk.
	for _, name := range []string{store.DBFile, store.DBFile + "-wal"} {
		raw, err := os.ReadFile(h.c.Store.Path(name))
		if err != nil && name == store.DBFile {
			t.Fatalf("the database file can't be read, so it isn't checked: %v", err)
		} else if err != nil {
			continue // no WAL at the moment
		}
		if bytes.Contains(raw, []byte("CANARY")) {
			t.Fatalf("a token is in plain text in %s", name)
		}
	}
	// Central itself can open them.
	if p, err := h.c.HarborPilot.ParentToken(ctx); err != nil || p != parent {
		t.Fatalf("parent token: %v", err)
	}
}
