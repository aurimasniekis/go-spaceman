package proto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	b, err := Encode("m1", TypeAck, now, Ack{CommandID: "cmd_x", Result: AckDuplicate})
	if err != nil {
		t.Fatal(err)
	}
	e, err := Decode(b)
	if err != nil || e.Type != TypeAck || !e.TS.Equal(now) {
		t.Fatalf("%+v %v", e, err)
	}
	var a Ack
	if err := e.Decode(&a); err != nil || a.Result != AckDuplicate {
		t.Fatalf("%+v %v", a, err)
	}
	for _, bad := range []string{`{}`, `{"v":2,"id":"x","type":"t"}`, `{"v":1,"type":"t"}`, `nope`} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := Decode([]byte(strings.Repeat(" ", MaxMessage+1))); err == nil {
		t.Error("oversized message accepted")
	}
	if err := (Envelope{}).Decode(&a); err == nil {
		t.Error("empty body decoded")
	}
}

func TestCommandValidate(t *testing.T) {
	ok := Command{CommandID: "cmd_" + strings.Repeat("a", 26), Kind: CmdLaunch, SpaceID: "spc_" + strings.Repeat("b", 26),
		RunID: "run_" + strings.Repeat("c", 26), Generation: 1, IdempotencyKey: "k"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Command){
		"cmd id":     func(c *Command) { c.CommandID = "cmd_short" },
		"space":      func(c *Command) { c.SpaceID = "../etc" },
		"generation": func(c *Command) { c.Generation = 0 },
		"key":        func(c *Command) { c.IdempotencyKey = "" },
		"run":        func(c *Command) { c.RunID = "" },
		"kind":       func(c *Command) { c.Kind = "rm -rf" },
	} {
		c := ok
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("%s: invalid command accepted", name)
		}
	}
	prep := ok
	prep.Kind, prep.RunID = CmdPrepare, ""
	if err := prep.Validate(); err != nil {
		t.Fatalf("prepare without run: %v", err)
	}
}

func TestNegotiate(t *testing.T) {
	if m, err := Negotiate(0, MaxMinor); err != nil || m != MaxMinor {
		t.Fatalf("%d %v", m, err)
	}
	// An agent from before memory.flush (minor 3) still connects, at minor 3 (ADR-0045).
	if m, err := Negotiate(0, 3); err != nil || m != 3 || MaxMinor < MinorMemoryFlush {
		t.Fatalf("%d %v", m, err)
	}
	if _, err := Negotiate(MaxMinor+1, MaxMinor+2); err == nil {
		t.Fatal("incompatible range accepted")
	}
}

func TestLaunchPayloadRedacted(t *testing.T) {
	p := LaunchPayload{Harness: "claude-code", AccountRef: "acc_x", Secrets: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "s3cret"}, RunToken: "rt"}
	b, _ := json.Marshal(p.Redacted())
	if strings.Contains(string(b), "s3cret") || strings.Contains(string(b), `"rt"`) || !strings.Contains(string(b), "acc_x") {
		t.Fatalf("redacted payload leaks: %s", b)
	}
	if p.Secrets == nil {
		t.Fatal("Redacted must not modify the original")
	}
}

// The repository sync (minor 4) is a space-level command without a run, and not a relocation step.
func TestReposSyncCommand(t *testing.T) {
	c := Command{CommandID: "cmd_" + strings.Repeat("a", 26), SpaceID: "spc_" + strings.Repeat("b", 26), Generation: 1, IdempotencyKey: "k", Kind: CmdReposSync}
	if err := c.Validate(); err != nil || IsRelocation(CmdReposSync) || MaxMinor < MinorRepositories {
		t.Fatalf("repos sync: %v", err)
	}
}

// Relocation steps (minor 3) need no run and are recognized; relocation file names are single safe path elements.
func TestRelocationCommandsAndBlobNames(t *testing.T) {
	c := Command{CommandID: "cmd_" + strings.Repeat("a", 26), SpaceID: "spc_" + strings.Repeat("b", 26), Generation: 1, IdempotencyKey: "k"}
	for _, k := range []string{CmdRelocateCheck, CmdRelocateExport, CmdRelocatePrepare, CmdRelocateRelease} {
		c.Kind = k
		if err := c.Validate(); err != nil || !IsRelocation(k) {
			t.Fatalf("%s: %v", k, err)
		}
	}
	if IsRelocation(CmdLaunch) || MaxMinor < MinorRelocation {
		t.Fatal("relocation detection or protocol range wrong")
	}
	for name, ok := range map[string]bool{"repo-0.bundle": true, "loose.tar.gz": true, "memory-control.bundle": true, "../x": false, "a/b": false,
		"": false, ".hidden": false, "a..b": false, strings.Repeat("a", 65): false} {
		if ValidBlobName(name) != ok {
			t.Errorf("ValidBlobName(%q) != %v", name, ok)
		}
	}
}
