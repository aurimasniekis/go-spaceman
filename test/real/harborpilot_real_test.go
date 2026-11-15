//go:build real

package real

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hptest"
)

// hpHarness wraps a real harness binary for the Harbor Pilot runs: the wrapper records, in a private folder outside the
// stack's directories, the names of the variables the harness was started with, the gateway token it got (so the test
// can check the token stops working after the run) and its run token, then execs the real binary. It also turns off Claude Code's
// auto-updater, which would otherwise install into the node user's HOME.
func hpHarness(t *testing.T, real string) (wrapper, record string) {
	t.Helper()
	record = t.TempDir()
	wrapper = filepath.Join(record, "harness.sh")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = --version ]; then exec %[1]q --version; fi
umask 077
env | cut -d= -f1 | sort > %[2]q/env-names
printf %%s "$ANTHROPIC_AUTH_TOKEN$HARBOR_PILOT_TOKEN" > %[2]q/token
printf %%s "$SPACEMAN_RUN_TOKEN" > %[2]q/run-token
export DISABLE_AUTOUPDATER=1
exec %[1]q "$@"
`, real, record)
	must(t, os.WriteFile(wrapper, []byte(script), 0o700))
	return wrapper, record
}

// hpRequest is the part of a Harbor Pilot request (list row and detail) the tests read.
type hpRequest struct {
	ID           string            `json:"id"`
	BindingID    string            `json:"binding_id"`
	SessionID    string            `json:"session_id"`
	Harness      string            `json:"harness"`
	CredentialID string            `json:"credential_id"`
	TrustedRefs  map[string]string `json:"trusted_refs"`
	Status       string            `json:"status"`
}

func hpGet(t *testing.T, g *hptest.Gateway, path string, out any) {
	t.Helper()
	code, body, err := g.Do(context.Background(), http.MethodGet, path, g.ServiceToken, nil)
	if err != nil || code != http.StatusOK {
		t.Fatalf("GET %s: %d %v %s", path, code, err, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("GET %s: %v: %s", path, err, body)
	}
}

// hpRunRequests lists the requests Harbor Pilot recorded with a trusted ref (filtered on the test binding).
func hpRunRequests(t *testing.T, g *hptest.Gateway, key, value string) []hpRequest {
	t.Helper()
	var out struct {
		Items    []hpRequest `json:"items"`
		Requests []hpRequest `json:"requests"`
		Data     []hpRequest `json:"data"`
	}
	hpGet(t, g, "/api/v1/activity/requests?"+url.Values{"ref": {key + ":" + value}, "binding_id": {g.BindingID}}.Encode(), &out)
	return slices.Concat(out.Items, out.Requests, out.Data)
}

// TestRealHarborPilotGatewayRuns (SM-GW-003/005, ADR-0060; real: Claude Code 2.1.317 and codex-cli 0.166.0, a real
// SpaceMan central, node agent, herdr-core and launcher shim, and `harbor-pilot serve --demo` v0.1.3 with synthetic
// upstreams, so no provider credentials and no cost). Opt-in: SPACEMAN_REAL=1 and SPACEMAN_HARBOR_PILOT=1; Codex with
// SPACEMAN_REAL_CODEX_BIN (or codex on PATH). For each harness, a run in a space routed through Harbor Pilot (with a
// direct account bound, which it must not get):
//   - the harness starts with the gateway's variables and no other provider credential;
//   - Harbor Pilot's request detail shows the test binding and the trusted refs spaceman.space and spaceman.run;
//   - the session the run's token is bound to groups the run's requests;
//   - neither the token nor the direct account's secret is at rest in herdr-core's state, the logs or central's data;
//   - after the run ends, its token gets 401.
func TestRealHarborPilotGatewayRuns(t *testing.T) {
	if os.Getenv("SPACEMAN_REAL") != "1" {
		t.Skip("set SPACEMAN_REAL=1 and SPACEMAN_HARBOR_PILOT=1 to run real Claude Code and Codex through Harbor Pilot serve --demo")
	}
	g := hptest.Start(t)
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not installed")
	}
	codex := os.Getenv("SPACEMAN_REAL_CODEX_BIN")
	if codex == "" {
		codex, _ = exec.LookPath("codex")
	}
	cases := []struct {
		harness, real string
		launch        map[string]any
		env           []string
	}{
		{harness: "claude-code", real: claude, launch: map[string]any{}, env: []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"}},
		// Codex's own default model is not in the demo plan: the run names a plan alias (the HP-T1 spike).
		{harness: "codex", real: codex, launch: map[string]any{"model": "coding/default", "settings": map[string]string{"sandbox": "read-only", "approval": "never"}},
			env: []string{"HARBOR_PILOT_TOKEN"}},
	}
	for _, tc := range cases {
		t.Run(tc.harness, func(t *testing.T) {
			if tc.real == "" {
				t.Skip("codex not installed: set SPACEMAN_REAL_CODEX_BIN (npm install --prefix <dir> @openai/codex@0.166.0)")
			}
			wrapper, record := hpHarness(t, tc.real)
			var e *env
			if tc.harness == "claude-code" {
				e = newEnvHarnesses(t, wrapper, "")
			} else {
				e = newEnvHarnesses(t, "", wrapper)
			}
			const direct = "direct-account-must-not-be-used"
			kind := "claude_oauth_token"
			if tc.harness == "codex" {
				kind = "openai_api_key"
			}
			_, spaceID := e.setupSpaceWith(t, kind, direct)
			if st := e.api("PUT", "/harbor-pilot/connection", map[string]any{"base_url": g.URL, "allow_insecure_http": true,
				"parent_token": g.ParentToken, "service_token": g.ServiceToken}, nil); st != 200 {
				t.Fatalf("connection: %d", st)
			}
			if st := e.api("PUT", "/spaces/"+spaceID+"/harbor-pilot", map[string]any{"enabled": true, "project_id": g.ProjectID}, nil); st != 200 {
				t.Fatalf("space settings: %d", st)
			}
			launch := map[string]any{"kind": "worker", "role": "reviewer", "harness": tc.harness,
				"prompt": "Reply with the word READY and nothing else. Do not use any tools or run any commands."}
			for k, v := range tc.launch {
				launch[k] = v
			}
			runID := e.launch(t, spaceID, launch)

			// The harness's environment: the gateway's variables, no other provider credential.
			var names []string
			eventually(t, 30*time.Second, "the harness started", func() bool {
				b, err := os.ReadFile(filepath.Join(record, "env-names"))
				names = strings.Fields(string(b))
				return err == nil && len(names) > 0
			})
			for _, want := range tc.env {
				if !slices.Contains(names, want) {
					t.Fatalf("the harness has no %s (variables: %v)", want, names)
				}
			}
			for _, n := range names {
				if (strings.HasPrefix(n, "ANTHROPIC_") || strings.HasPrefix(n, "CLAUDE_CODE_OAUTH") || strings.HasPrefix(n, "OPENAI_") ||
					n == "CODEX_API_KEY" || n == "SPACEMAN_GATEWAY_TOKEN" || n == "HARBOR_PILOT_TOKEN") && !slices.Contains(tc.env, n) {
					t.Fatalf("the harness got %s next to the gateway's credential", n)
				}
			}
			tokB, err := os.ReadFile(filepath.Join(record, "token"))
			tok := string(tokB)
			if err != nil || !strings.HasPrefix(tok, "hpk_") {
				t.Fatalf("the harness's gateway token: %v (%d bytes)", err, len(tok))
			}

			// Harbor Pilot recorded the run's requests, attributed to the run (trusted refs, not claims).
			var reqs []hpRequest
			eventually(t, 180*time.Second, "Harbor Pilot recorded the run's requests", func() bool {
				reqs = hpRunRequests(t, g, "spaceman.run", runID)
				return len(reqs) > 0
			})
			// A second turn of the same conversation: safe input, delivered once the harness is idle after its first.
			eventually(t, 120*time.Second, "the second prompt was delivered", func() bool {
				return e.api("POST", "/spaces/"+spaceID+"/runs/"+runID+"/input", map[string]any{"text": "Now reply with the word DONE and nothing else."},
					&struct{}{}) == http.StatusAccepted
			})
			eventually(t, 180*time.Second, "Harbor Pilot recorded the second turn", func() bool {
				reqs = hpRunRequests(t, g, "spaceman.run", runID)
				return len(reqs) >= 2
			})
			var d hpRequest
			hpGet(t, g, "/api/v1/activity/requests/"+reqs[0].ID, &d)
			if d.BindingID != g.BindingID || d.TrustedRefs["spaceman.space"] != spaceID || d.TrustedRefs["spaceman.run"] != runID {
				t.Fatalf("request detail: binding %q (want %q), trusted refs %v", d.BindingID, g.BindingID, d.TrustedRefs)
			}
			session := d.TrustedRefs["session"]
			if session == "" {
				t.Fatalf("no bound session in the trusted refs %v", d.TrustedRefs)
			}
			var r struct {
				SessionID string `json:"session_id"`
			}
			e.api("GET", "/spaces/"+spaceID+"/runs/"+runID, nil, &r)
			if tc.harness == "claude-code" && session != r.SessionID {
				t.Fatalf("bound session %q, the run's Claude session %q", session, r.SessionID)
			}
			// The bound session groups the run's requests: Harbor Pilot finds one session for it, and every request of the
			// run belongs to it.
			var sess struct {
				Items    []map[string]any `json:"items"`
				Sessions []map[string]any `json:"sessions"`
			}
			hpGet(t, g, "/api/v1/activity/sessions?"+url.Values{"ref": {"session:" + session}, "binding_id": {g.BindingID}}.Encode(), &sess)
			found := append(sess.Items, sess.Sessions...)
			if len(found) != 1 {
				t.Fatalf("sessions for the bound session: %v", found)
			}
			hpSession, _ := found[0]["id"].(string)
			reqs = hpRunRequests(t, g, "spaceman.run", runID)
			for _, q := range reqs {
				if q.SessionID != hpSession {
					t.Fatalf("request %s is in session %q, not the run's %q", q.ID, q.SessionID, hpSession)
				}
			}
			bySession := hpRunRequests(t, g, "session", session)
			if len(bySession) != len(reqs) {
				t.Fatalf("%d requests by the bound session, %d by the run", len(bySession), len(reqs))
			}
			t.Logf("%s run %s: %d requests, harness label %q, Harbor Pilot session %s (%d requests by the bound session), trusted refs %v",
				tc.harness, runID, len(reqs), d.Harness, hpSession, len(bySession), d.TrustedRefs)

			// Nothing at rest holds the token or the direct account's secret.
			if _, err := os.Stat(filepath.Join(e.dir, "agent", "runs", runID, "secrets.json")); !os.IsNotExist(err) {
				t.Fatalf("secret file not unlinked by the launcher shim: %v", err)
			}
			runTok, err := os.ReadFile(filepath.Join(record, "run-token"))
			if err != nil || len(runTok) < 16 {
				t.Fatalf("the harness's run token: %v (%d bytes)", err, len(runTok))
			}
			for _, probe := range []string{tok, direct, string(runTok)} {
				for _, f := range []string{filepath.Join(e.hcHome, "state", "herdr-core", e.session, "terminals.json"),
					filepath.Join(e.dir, "central.log"), filepath.Join(e.dir, "agent.log")} {
					if b, _ := os.ReadFile(f); strings.Contains(string(b), probe) {
						t.Fatalf("a credential is stored in clear in %s", filepath.Base(f))
					}
				}
				for _, root := range []string{filepath.Join(e.dir, "central"), filepath.Join(e.dir, "agent")} {
					_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
						if err == nil && !fi.IsDir() && fi.Mode().IsRegular() {
							if b, _ := os.ReadFile(p); strings.Contains(string(b), probe) {
								t.Errorf("a credential is stored in clear in %s", strings.TrimPrefix(p, e.dir))
							}
						}
						return nil
					})
				}
			}

			// The run ends: its token is revoked and gets 401.
			if st := e.api("POST", "/spaces/"+spaceID+"/runs/"+runID+"/stop", map[string]any{"mode": "kill", "reason": "real test done"}, nil); st != 200 {
				t.Fatalf("stop: %d", st)
			}
			eventually(t, 30*time.Second, "run ended", func() bool {
				o := e.getRun(spaceID, runID).Observed
				return o == "exited" || o == "failed"
			})
			eventually(t, 30*time.Second, "the run's token gets 401", func() bool {
				code, _, err := g.Do(context.Background(), http.MethodGet, "/v1/models", tok, nil)
				return err == nil && code == http.StatusUnauthorized
			})
			if cred, err := g.CredentialInfo(context.Background(), d.CredentialID); err != nil || cred.RevokedAt == nil {
				t.Fatalf("credential %s: %+v %v", d.CredentialID, cred, err)
			}
		})
	}
}
