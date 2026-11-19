package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// harness.env_passthrough through a real launch: the variable lands in the run's secrets file, which the launcher shim
// applies after its scrub; an unlisted variable does not.
func TestPassthroughLaunchWritesSecretsFile(t *testing.T) {
	t.Setenv("CLAUDE_CODE_TMPDIR", "/data/tmp")
	t.Setenv("NOT_LISTED", "no")
	dir := t.TempDir()
	a, f := ccAgent(t, dir, nil, func(o *Options) {
		o.Claude = ClaudeOptions{}
		o.EnvPassthrough = []string{"CLAUDE_CODE_TMPDIR"}
	})
	ccLaunch(a, "run_pass")
	runSpec(t, f, "run_pass")
	b, err := os.ReadFile(filepath.Join(a.runDir("run_pass"), SecretsFile))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["CLAUDE_CODE_TMPDIR"] != "/data/tmp" {
		t.Errorf("secrets file lacks the passthrough variable: %v", keys(m))
	}
	if _, ok := m["NOT_LISTED"]; ok {
		t.Errorf("unlisted variable in the secrets file")
	}
}

func keys(m map[string]string) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	return k
}
