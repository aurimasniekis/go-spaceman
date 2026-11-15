package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/harness"
)

// The test binary is also the agent: with SM_TEST_AGENT_MAIN=1 it runs main() with its arguments.
func TestMain(m *testing.M) {
	if os.Getenv("SM_TEST_AGENT_MAIN") == "1" {
		os.Args = append([]string{"spaceman-agent"}, os.Args[1:]...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// N1 (ADR-0068): `spaceman-agent hook permission-request` is wired to the deny-only hook: it exits 0 and its stdout is
// exactly one JSON deny decision, nothing else, for well-formed, malformed and over-1 MiB input.
func TestHookPermissionRequestCommand(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{
		"request": `{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"ls"}}`,
		"garbage": "{oops",
		"big":     `{"tool_name":"Read","x":"` + strings.Repeat("y", 2<<20) + `"}`,
	} {
		cmd := exec.Command(self, "hook", "permission-request") //nolint:gosec // the test binary itself
		cmd.Env = append(os.Environ(), "SM_TEST_AGENT_MAIN=1")
		cmd.Stdin = strings.NewReader(in)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v (stderr %q)", name, err, stderr.String())
		}
		var o struct {
			HookSpecificOutput struct {
				HookEventName string `json:"hookEventName"`
				Decision      struct {
					Behavior string `json:"behavior"`
					Message  string `json:"message"`
				} `json:"decision"`
			} `json:"hookSpecificOutput"`
		}
		// json.Unmarshal refuses anything after the one value (other than white space).
		if err := json.Unmarshal(stdout.Bytes(), &o); err != nil || o.HookSpecificOutput.HookEventName != "PermissionRequest" ||
			o.HookSpecificOutput.Decision.Behavior != "deny" || !strings.HasSuffix(o.HookSpecificOutput.Decision.Message, harness.ClaudePermissionDenyMessage) {
			t.Fatalf("%s: %+v %v", name, o, err)
		}
	}
}
