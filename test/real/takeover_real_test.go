//go:build real

package real

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/recording"
)

// TestRealTakeover (SM-UI-004): the real central, spaceman-agent and an isolated herdr-core core. An operator's
// takeover types into the run's terminal through the node's interactive attach (bytes, including characters
// terminal.keys has no name for); Message the agent is refused while it lasts; after the release automated input
// reaches the program again. The harness is a stub that echoes each line it reads.
func TestRealTakeover(t *testing.T) {
	dir := t.TempDir()
	echo := filepath.Join(dir, "echo-claude.sh")
	must(t, os.WriteFile(echo, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo '0.0.0 (stub)'; exit 0; fi\necho echo-harness-ready\n"+
		"while IFS= read -r l; do echo \"got:[$l]\"; done\n"), 0o700))
	e := newEnvWith(t, echo)
	_, spaceID, runID := e.launchRun(t)
	runDir := filepath.Join(e.dir, "agent", "runs", runID)
	recorded := func(want string) bool {
		res, err := recording.Read(filepath.Join(runDir, "recording"), 0)
		if err != nil {
			return false
		}
		var out strings.Builder
		for _, rec := range res.Records {
			out.Write(rec.Data)
		}
		return strings.Contains(out.String(), want)
	}
	eventually(t, 15*time.Second, "harness ready", func() bool { return recorded("echo-harness-ready") })

	base := "/spaces/" + spaceID + "/runs/" + runID
	var tk struct {
		ID     string `json:"id"`
		Holder string `json:"holder"`
	}
	if st := e.api("POST", base+"/takeover", nil, &tk); st != 201 || tk.Holder != "root" {
		t.Fatalf("start takeover: %d %+v", st, tk)
	}
	var shown struct {
		Takeover *struct {
			ID string `json:"id"`
		} `json:"takeover"`
	}
	e.api("GET", base, nil, &shown)
	if shown.Takeover == nil || shown.Takeover.ID != tk.ID {
		t.Fatalf("run does not show the takeover: %+v", shown)
	}
	time.Sleep(time.Second) // the node attaches interactively
	if st := e.api("POST", base+"/takeover/"+tk.ID+"/input", map[string]string{"data": "typed $HOME ~ \"q\"\r"}, nil); st != 202 {
		t.Fatalf("type: %d", st)
	}
	eventually(t, 15*time.Second, "keystrokes reached the program", func() bool { return recorded(`got:[typed $HOME ~ "q"]`) })
	if st := e.api("POST", base+"/input", map[string]any{"text": "automated", "force": true}, nil); st != 409 {
		t.Fatalf("Message the agent during the takeover: %d, want 409", st)
	}
	if st := e.api("POST", base+"/takeover/"+tk.ID+"/end", nil, nil); st != 200 {
		t.Fatalf("release: %d", st)
	}
	if st := e.api("POST", base+"/input", map[string]any{"text": "after-release", "force": true}, nil); st != 202 {
		t.Fatalf("Message the agent after release: %d", st)
	}
	eventually(t, 20*time.Second, "automated input after the release", func() bool { return recorded("got:[after-release]") })
	if recorded("got:[automated]") {
		t.Fatal("refused input reached the program")
	}
	if n := e.hcTerminals(t, "spaceman.run="+runID); n != 1 {
		t.Fatalf("terminals for the run = %d, want 1", n)
	}
}
