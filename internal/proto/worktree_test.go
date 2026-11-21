package proto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ADR-0107 decision 4: space.worktree.remove (valid without a run id) and worktree.removed round-trip, the stored
// payload never holds the key, and bad input is refused before any side effect.
func TestWorktreeRemoveMessages(t *testing.T) {
	space, cmdID := "spc_"+strings.Repeat("b", 26), "cmd_"+strings.Repeat("c", 26)
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	if MaxMinor < MinorWorktreeRemove || MinorWorktreeRemove != 16 {
		t.Fatalf("MaxMinor %d, MinorWorktreeRemove %d", MaxMinor, MinorWorktreeRemove)
	}
	p := WorktreeRemovePayload{Repository: RepoSource{ID: "rep_api", Path: "repos/api", URL: "https://example.com/o/api.git", SSHKey: "secret"}, Name: "spaceman-x"}
	cmd := Command{CommandID: cmdID, Kind: CmdWorktreeRemove, SpaceID: space, Generation: 2, IdempotencyKey: "wt:1", Payload: mustMarshal(t, p)}
	if err := cmd.Validate(); err != nil || IsRelocation(CmdWorktreeRemove) {
		t.Fatalf("command: %v", err)
	}
	var back Command
	roundTrip(t, TypeCommand, now, cmd, &back)
	var got WorktreeRemovePayload
	if err := json.Unmarshal(back.Payload, &got); err != nil || got != p || got.Validate() != nil {
		t.Fatalf("payload %+v %v", got, err)
	}
	if r := p.Redacted(); r.Repository.SSHKey != "" || r.Name != p.Name || p.Repository.SSHKey == "" {
		t.Fatalf("redacted %+v", r)
	}
	for name, mut := range map[string]func(*WorktreeRemovePayload){
		"id":        func(p *WorktreeRemovePayload) { p.Repository.ID = "" },
		"path":      func(p *WorktreeRemovePayload) { p.Repository.Path = "" },
		"name":      func(p *WorktreeRemovePayload) { p.Name = "" },
		"slash":     func(p *WorktreeRemovePayload) { p.Name = "a/b" },
		"backslash": func(p *WorktreeRemovePayload) { p.Name = "a\\b" },
		"nul":       func(p *WorktreeRemovePayload) { p.Name = "a\x00b" },
		"long":      func(p *WorktreeRemovePayload) { p.Name = strings.Repeat("a", 256) },
	} {
		q := p
		mut(&q)
		if q.Validate() == nil {
			t.Errorf("%s: invalid payload accepted", name)
		}
	}

	rep := WorktreeRemoved{CommandID: cmdID, SpaceID: space, Repo: "repos/api", Name: "spaceman-x", Result: WorktreeRefused, Code: "dirty", Reason: "changed files", CheckedAt: now}
	var gotRep WorktreeRemoved
	roundTrip(t, TypeWorktreeRemoved, now, rep, &gotRep)
	if gotRep != rep || gotRep.Validate() != nil {
		t.Fatalf("report %+v", gotRep)
	}
	for name, bad := range map[string]WorktreeRemoved{
		"command": {CommandID: "x", SpaceID: space, Result: WorktreeRemovedOK},
		"space":   {CommandID: cmdID, SpaceID: "x", Result: WorktreeRemovedOK},
		"result":  {CommandID: cmdID, SpaceID: space, Result: "gone"},
		"no code": {CommandID: cmdID, SpaceID: space, Result: WorktreeRefused},
	} {
		if bad.Validate() == nil {
			t.Errorf("%s: invalid report accepted", name)
		}
	}
}
