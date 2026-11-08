package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// SM-OBS-006 over HTTP: administrators read the instance's status (build, boot ID, schema and protocol versions,
// storage, counts); a global operator and a space viewer are refused; the version alone stays public. The status
// names no key material or key path.
func TestSystemStatusOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	en, _ := h.c.Fleet.CreateNode(ctx, admin, fleet.CreateNodeInput{Name: "node-a"})
	a, err := h.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.c.Store.W().TxNamed(ctx, "test.activate", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, "UPDATE nodes SET state = 'active' WHERE id = ?", en.Node.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	expect(t, root.do("POST", "/spaces/"+a.ID+"/human-actions", map[string]any{"type": "todo", "title": "Rotate the key"}), 201)

	r := root.do("GET", "/system", nil)
	expect(t, r, 200)
	var st api.SystemStatus
	r.json(t, &st)
	if st.Build.Version != buildinfo.Get().Version || st.BootID != h.c.BootID || !strings.HasPrefix(st.BootID, "boot_") || !st.StartedAt.Equal(h.c.StartedAt) {
		t.Fatalf("build/boot %+v", st)
	}
	if st.SchemaVersion.Error != "" || st.SchemaVersion.Current == 0 || st.SchemaVersion.Current != st.SchemaVersion.Latest {
		t.Fatalf("schema %+v", st.SchemaVersion)
	}
	if st.Protocol != (api.SystemProtocol{Major: proto.Major, MinMinor: proto.MinMinor, MaxMinor: proto.MaxMinor}) {
		t.Fatalf("protocol %+v", st.Protocol)
	}
	if st.Storage.StoreState != "ok" || st.Storage.DBBytes == 0 || st.Storage.DiskTotal == 0 || st.DataDir != h.c.Cfg.DataDir || st.PublicURL != h.c.Cfg.PublicURL {
		t.Fatalf("storage %+v data dir %q", st.Storage, st.DataDir)
	}
	if st.Counts != (api.SystemCounts{Spaces: 1, NodesActive: 1, NodesConnected: 0, RunsRunning: 0, HumanPending: 1, LiveStreams: 0}) {
		t.Fatalf("counts %+v", st.Counts)
	}
	body := strings.ToLower(string(r.body))
	for _, needle := range []string{"kek", "keyring", "spaceman_rt_", "spaceman_at_", "secret"} {
		if strings.Contains(body, needle) {
			t.Fatalf("status mentions %q: %s", needle, r.body)
		}
	}

	expect(t, root.do("POST", "/users", map[string]any{"email": "ops@example.com", "display_name": "Ops", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "operator", "domain": "*"}}}), 201)
	ops := h.client()
	ops.bearer = loginAndIssueToken(t, h, "ops@example.com", []map[string]string{{"role": "operator", "domain": "*"}})
	expect(t, ops.do("GET", "/system", nil), 403)
	expect(t, root.do("POST", "/users", map[string]any{"email": "vic@example.com", "display_name": "Vic", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "viewer", "domain": a.ID}}}), 201)
	viewer := h.client()
	viewer.bearer = loginAndIssueToken(t, h, "vic@example.com", []map[string]string{{"role": "viewer", "domain": a.ID}})
	expect(t, viewer.do("GET", "/system", nil), 403)
	expect(t, h.client().do("GET", "/system", nil), 401)

	// The version alone stays readable without signing in.
	var info api.SystemInfo
	h.client().do("GET", "/system/info", nil).json(t, &info)
	if info.Version != buildinfo.Get().Version {
		t.Fatalf("public version %+v", info)
	}
}
