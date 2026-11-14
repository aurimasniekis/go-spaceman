package cli

import (
	"strings"
	"testing"
)

// SM-TG-014 through the CLI: `telegram set` replaces the stored settings (a flag or --file field not given takes its
// default; the If-Match comes from the settings read first, none while nothing is stored); `reset` restores the
// defaults; a space's settings merge the flags given into the settings in force.
func TestTelegramCommands(t *testing.T) {
	c := startCentral(t)
	e := c.env()
	e.ok(t, "node", "create", "n1")
	e.ok(t, "space", "create", "payments", "--name", "Payments", "--node", "n1")

	if s := e.obj(t, "telegram", "settings"); s["stored"] != false || s["links_per_user"].(float64) != 1 {
		t.Fatalf("defaults %v", s)
	}
	doc := writeFile(t, "telegram.yaml", "send_limits:\n  global_per_second: 10\nnotification_defaults:\n  digest_time: \"07:30\"\n", 0o600)
	s := e.obj(t, "telegram", "set", "--file", doc, "--links-per-user", "2")
	sl, _ := s["send_limits"].(map[string]any)
	nd, _ := s["notification_defaults"].(map[string]any)
	if s["stored"] != true || s["links_per_user"].(float64) != 2 || sl["global_per_second"].(float64) != 10 || sl["chat_burst"].(float64) != 3 ||
		str(nd, "digest_time") != "07:30" || s["version"].(float64) != 1 {
		t.Fatalf("set %v", s)
	}
	// A replacement: what isn't given goes back to its default.
	s = e.obj(t, "telegram", "set", "--topics-mode-default")
	if s["links_per_user"].(float64) != 1 || s["topics_mode_default"] != true || s["version"].(float64) != 2 {
		t.Fatalf("replaced %v", s)
	}
	if r := e.run(t, "telegram", "set", "--links-per-user", "9"); r.code == 0 || !strings.Contains(r.err+r.out, "links_per_user") {
		t.Fatalf("out of range: %+v", r)
	}
	e.ok(t, "telegram", "reset", "--yes")
	if s := e.obj(t, "telegram", "settings"); s["stored"] != false || s["topics_mode_default"] != false {
		t.Fatalf("after reset %v", s)
	}

	sp := e.obj(t, "telegram", "set-space", "--space", "payments", "--enabled", "--terminal-watch")
	if sp["enabled"] != true || sp["terminal_watch"] != true || str(sp, "content_level") != "full" || sp["version"].(float64) != 1 {
		t.Fatalf("set-space %v", sp)
	}
	sp = e.obj(t, "telegram", "set-space", "--space", "payments", "--content-level", "summary")
	if sp["enabled"] != true || sp["terminal_watch"] != true || str(sp, "content_level") != "summary" || sp["version"].(float64) != 2 {
		t.Fatalf("merged set-space %v", sp)
	}
	if out := e.ok(t, "telegram", "spaces"); !strings.Contains(out, "summary") {
		t.Fatalf("spaces %s", out)
	}
	e.ok(t, "telegram", "unset-space", "--space", "payments")
	if sp := e.obj(t, "telegram", "space", "--space", "payments"); sp["stored"] != false || sp["enabled"] != false {
		t.Fatalf("after unset %v", sp)
	}
}
