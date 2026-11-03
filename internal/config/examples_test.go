package config

import (
	"path/filepath"
	"testing"
)

// SM-INV-017: the example configurations shipped in deploy/examples (and in the release archives) load and validate
// as they are, with no environment, so a documented example never drifts from what central and the agent accept.
func TestDeployExamplesLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "deploy", "examples")
	for _, name := range []string{"spaceman.example.yaml", "spaceman.container.yaml"} {
		cfg, err := Load(filepath.Join(dir, name), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !cfg.IsProduction() {
			t.Errorf("%s is not a production example", name)
		}
	}
	cfg, err := LoadAgent(filepath.Join(dir, "agent.example.yaml"), nil)
	if err != nil {
		t.Fatalf("agent.example.yaml: %v", err)
	}
	if !cfg.IsProduction() {
		t.Error("agent.example.yaml is not a production example")
	}
}
