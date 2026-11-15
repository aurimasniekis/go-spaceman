package app_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/agent"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hpfake"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
)

// SM-GW-003 (simulated: hpfake, the real node agent with a fake runner): gateway runs on a node at the newest minor get
// the gateway's address and their child token only in the shim's secrets file, under the harness's own names, and no
// other credential, although the space has direct accounts bound:
//   - Claude Code: exactly ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN; a login stored in its config dir is removed;
//   - Codex: HARBOR_PILOT_TOKEN and the five model-provider overrides in argv; no auth.json (an API-key one left in
//     CODEX_HOME is removed);
//   - neither the token nor the address is in the runner's Spec.Env (herdr-core persists it in terminals.json) or argv,
//     nor anywhere in the node's journal or data directory outside the secrets file.
func TestHarborPilotGatewayDeliveryOnNode(t *testing.T) {
	t.Parallel()
	f := hpfake.New(t)
	e := newFleetEnv(t)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "gw-node", Name: "Gateway node", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	if _, err := e.c.HarborPilot.PutConnection(ctx, admin, 0, harborpilot.ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true, ParentToken: f.Parent,
		ServiceToken: f.Service}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.HarborPilot.PutSpace(ctx, admin, sp.ID, 0, harborpilot.SpaceInput{Enabled: true, ProjectID: "prj_web"}, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })

	launch := func(harness, role, model, instance string) *fleet.Run {
		t.Helper()
		r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: role, Harness: harness, Model: model, Prompt: "go", InstanceID: instance})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, harness+" running", func() bool { return e.run1(r.ID).Observed == "running" })
		return e.run1(r.ID)
	}
	stop := func(r *fleet.Run) {
		t.Helper()
		if _, err := e.c.Fleet.StopRun(ctx, admin, r.ID, "kill", "test"); err != nil {
			t.Fatal(err)
		}
		eventually(t, "stopped", func() bool { o := e.run1(r.ID).Observed; return o == "exited" || o == "failed" })
	}
	secrets := func(r *fleet.Run) map[string]string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(e.agentDir, "runs", r.ID, agent.SecretsFile))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if m["SPACEMAN_RUN_TOKEN"] == "" {
			t.Fatal("no run token")
		}
		delete(m, "SPACEMAN_RUN_TOKEN")
		return m
	}
	instance := func(r *fleet.Run, harness string) string {
		return filepath.Join(e.agentDir, "spaces", sp.Workdir, "instances", r.InstanceID, harness)
	}
	// nowhere checks a token is in no runner spec and in no file of the node outside the runs' secrets files.
	nowhere := func(tok string) {
		t.Helper()
		for _, spec := range e.run.Specs {
			all := strings.Join(spec.Argv, " ")
			for k, v := range spec.Env {
				all += " " + k + "=" + v
			}
			if strings.Contains(all, tok) {
				t.Fatalf("the token reached the runner's spec of %s", spec.Name)
			}
			for k := range spec.Env {
				if strings.HasPrefix(k, "ANTHROPIC_") || k == hns.CodexGatewayEnv {
					t.Fatalf("%s in the runner's env", k)
				}
			}
		}
		_ = filepath.Walk(e.agentDir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() || fi.Name() == agent.SecretsFile {
				return nil //nolint:nilerr // files that go away while walking are not the token
			}
			if b, _ := os.ReadFile(p); strings.Contains(string(b), tok) {
				t.Errorf("the token is stored in the node's %s", strings.TrimPrefix(p, e.agentDir))
			}
			return nil
		})
	}

	// Claude Code: the first run of the role, then a stored login is planted in its config dir; the next run of the
	// role on the same instance removes it.
	c1 := launch(hns.KindClaude, "implementor", "", "")
	sec := secrets(c1)
	tok := sec["ANTHROPIC_AUTH_TOKEN"]
	if !strings.HasPrefix(tok, "hpk_child_") || !reflect.DeepEqual(sec, map[string]string{"ANTHROPIC_BASE_URL": f.URL(), "ANTHROPIC_AUTH_TOKEN": tok}) {
		t.Fatalf("claude secrets %v", keys(sec))
	}
	nowhere(tok)
	stop(c1)
	creds := filepath.Join(instance(c1, "claude"), ".credentials.json")
	if err := os.WriteFile(creds, []byte(`{"claudeAiOauth":{"accessToken":"stub-login"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c2 := launch(hns.KindClaude, "implementor", "", c1.InstanceID)
	if _, err := os.Lstat(creds); !os.IsNotExist(err) {
		t.Fatalf("the stored Claude login is still there: %v", err)
	}
	stop(c2)

	// Codex: an API-key auth.json an earlier direct-account launch left in CODEX_HOME is removed.
	x1 := launch(hns.KindCodex, "reviewer", "coding/default", "")
	stop(x1)
	auth := filepath.Join(instance(x1, "codex"), "auth.json")
	if err := os.WriteFile(auth, []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"stub-openai-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// HP-T4 review F1: so is a shell snapshot an earlier Codex version wrote with the account's API key in it.
	snaps := filepath.Join(instance(x1, "codex"), "shell_snapshots")
	if err := os.MkdirAll(snaps, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snaps, "old.sh"), []byte("export OPENAI_API_KEY=stub-snapshot-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	x2 := launch(hns.KindCodex, "reviewer", "coding/default", x1.InstanceID)
	sec = secrets(x2)
	ctok := sec[hns.CodexGatewayEnv]
	if !strings.HasPrefix(ctok, "hpk_child_") || len(sec) != 1 || ctok == tok {
		t.Fatalf("codex secrets %v", keys(sec))
	}
	if _, err := os.Lstat(auth); !os.IsNotExist(err) {
		t.Fatalf("auth.json is still in CODEX_HOME: %v", err)
	}
	if _, err := os.Lstat(snaps); !os.IsNotExist(err) {
		t.Fatalf("the shell snapshots are still in CODEX_HOME: %v", err)
	}
	procs := e.run.LiveByLabel("spaceman.run", x2.ID)
	if len(procs) != 1 {
		t.Fatalf("codex process %+v", procs)
	}
	argv := strings.Join(procs[0].Argv, " ")
	for _, want := range []string{`model_provider="harbor-pilot"`, `model_providers.harbor-pilot.base_url="` + f.URL() + `/v1"`,
		`model_providers.harbor-pilot.env_key="HARBOR_PILOT_TOKEN"`, "-m coding/default"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("codex argv %q lacks %q", argv, want)
		}
	}
	nowhere(ctok)
	nowhere("stub-openai-key")
	nowhere("stub-snapshot-key")
	nowhere("stub-claude-token")
}
