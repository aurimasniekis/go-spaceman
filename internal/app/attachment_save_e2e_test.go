package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/agent"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// ADR-0073 end to end: a run saves attachments over 512 KiB (2 MB and 10 MB) through the real stdio MCP bridge, the
// node agent and central. The file holds exactly the uploaded bytes (size and SHA-256 checked), sits 0600 in the run's
// own folder under its instance, has a sanitized name whatever the uploader called it, and is removed when the run
// ends; a later launch of the instance prunes folders of runs that ended without cleaning up. The inline read of a
// large binary says it is not returned and points to the save tool.
func TestAttachmentSaveOverBridge(t *testing.T) {
	// Not parallel: t.Setenv (the bridge reads SPACEMAN_RUN_TOKEN).
	e := newFleetEnv(t, func(c *config.Config) { c.HTTP.MaxUploadBytes = 64 << 20 })
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "files", Name: "Files", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	wrk, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "run running", func() bool { return e.run1(wrk.ID).Observed == "running" })
	space, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
	agentReal, err := filepath.EvalSymlinks(e.agentDir)
	if err != nil {
		t.Fatal(err)
	}
	inst := filepath.Join(agentReal, "spaces", space.Workdir, "instances", wrk.InstanceID)

	upload := func(name string, n int) ([]byte, string) {
		b := make([]byte, n)
		_, _ = rand.Read(b)
		copy(b, "\x89PNG\r\n\x1a\n")
		a, err := e.c.Attachments.Upload(ctx, admin, sp.ID, name, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return b, a.ID
	}
	img, imgID := upload("Screenshot 2026-11-08 at 10.00.00.png", 2<<20)
	big, bigID := upload("../../../claude/settings.json", 10<<20)
	abs, absID := upload("/etc/passwd", 600<<10)
	_, hugeID := upload("huge.bin", 50<<20+1)

	// The harness side: an MCP client on the real stdio bridge.
	t.Setenv("SPACEMAN_RUN_TOKEN", e.runToken(wrk.ID))
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	bctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- agent.MCPBridge(bctx, sr, sw, wrk.ID, e.bridge) }()
	t.Cleanup(func() {
		cancel()
		_ = cw.Close()
		_ = sw.Close()
		if err := <-done; err != nil && bctx.Err() == nil {
			t.Errorf("bridge: %v", err)
		}
	})
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "harness", Version: "test"}, nil).Connect(ctx, &mcp.IOTransport{Reader: cr, Writer: cw}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(tools.Tools, func(x *mcp.Tool) bool { return x.Name == proto.ToolAttachmentSave }) {
		t.Fatal("the bridge does not offer " + proto.ToolAttachmentSave)
	}
	call := func(tool string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if !res.IsError {
			if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &m); err != nil {
				t.Fatal(err)
			}
		}
		return res, m
	}
	save := func(ref string, want []byte, wantName string) string {
		t.Helper()
		res, out := call(proto.ToolAttachmentSave, map[string]any{"attachment": ref})
		if res.IsError {
			t.Fatalf("save %s: %s", ref, res.Content[0].(*mcp.TextContent).Text)
		}
		p, _ := out["path"].(string)
		dir := filepath.Join(inst, "attachments", wrk.ID)
		if filepath.Dir(filepath.Dir(p)) != dir || filepath.Base(p) != wantName || int(out["size"].(float64)) != len(want) {
			t.Fatalf("saved %v, want %s/<id>/%s", out, dir, wantName)
		}
		got, err := os.ReadFile(p) //nolint:gosec // the test's own temp folder
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("saved content differs (%d of %d bytes): %v", len(got), len(want), err)
		}
		fi, _ := os.Stat(p)
		di, _ := os.Stat(filepath.Dir(p))
		if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
			t.Fatalf("modes: file %v, folder %v", fi.Mode(), di.Mode())
		}
		return p
	}
	save("spaceman://attachment/"+imgID, img, "Screenshot_2026-11-08_at_10.00.00.png")
	save(bigID, big, "settings.json")
	save(absID, abs, "passwd")

	// The inline read never cuts a file silently.
	res, out := call(proto.ToolAttachmentGet, map[string]any{"attachment": bigID})
	if res.IsError || out["truncated"] != true || out["content"] != nil || !strings.Contains(out["note"].(string), proto.ToolAttachmentSave) {
		t.Fatalf("inline read of 10 MB: %v", out)
	}
	// Nothing outside the attachments folder was written.
	if b, _ := os.ReadFile(filepath.Join(inst, "claude", "settings.json")); bytes.Equal(b, big) { //nolint:gosec // the test's own temp folder
		t.Fatal("a name escaped the attachments folder")
	}
	res, _ = call(proto.ToolAttachmentSave, map[string]any{"attachment": "att_missing"})
	if !res.IsError {
		t.Fatal("saving an unknown attachment succeeded")
	}
	// Another space's attachment: central refuses it as for the inline read, so the node can't be used to reach it.
	osp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "elsewhere", Name: "Elsewhere", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := e.c.Attachments.Upload(ctx, admin, osp.ID, "secret.png", bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	res, _ = call(proto.ToolAttachmentSave, map[string]any{"attachment": foreign.ID})
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "not found") {
		t.Fatalf("saving another space's attachment: %+v", res)
	}
	// Over the per-save cap: refused, nothing written.
	res, _ = call(proto.ToolAttachmentSave, map[string]any{"attachment": hugeID})
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "at most") {
		t.Fatalf("saving over the cap: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(inst, "attachments", wrk.ID, hugeID)); err == nil {
		if entries, _ := os.ReadDir(filepath.Join(inst, "attachments", wrk.ID, hugeID)); len(entries) > 0 {
			t.Fatalf("files left after a refused save: %v", entries)
		}
	}

	// A stale folder of a run the node no longer knows; the run's own folder goes when it ends.
	stale := filepath.Join(inst, "attachments", ids.New(ids.Run))
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Fleet.StopRun(ctx, admin, wrk.ID, "kill", "done"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "run's attachments removed", func() bool { _, err := os.Stat(filepath.Join(inst, "attachments", wrk.ID)); return os.IsNotExist(err) })
	again, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude, InstanceID: wrk.InstanceID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "relaunched", func() bool { return e.run1(again.ID).Observed == "running" })
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale attachments folder kept: %v", err)
	}
}
