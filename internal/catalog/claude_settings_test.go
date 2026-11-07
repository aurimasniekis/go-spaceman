package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// An update without Claude settings keeps the stored ones, read in the save's own transaction (the API no longer
// reads them first, where an error used to clear them); an empty object still clears them.
func TestClaudeSettingsKeptWhenLeftOut(t *testing.T) {
	f := newFx(t)
	cs := proto.ClaudeSettings{"permissions": map[string]any{"deny": []any{"Read(./secrets/**)"}}}
	p, err := f.svc.SaveProfile(f.ctx, op, "", 0, Profile{Slug: "claude", Name: "Claude", Harness: "claude-code", ClaudeSettings: cs})
	must(t, err)
	p, err = f.svc.SaveProfile(f.ctx, op, p.ID, p.Version, Profile{Name: "Renamed"})
	must(t, err)
	if p.Name != "Renamed" || p.ClaudeSettings["permissions"] == nil {
		t.Fatalf("settings left out were not kept: %+v", p.ClaudeSettings)
	}
	p, err = f.svc.SaveProfile(f.ctx, op, p.ID, p.Version, Profile{Name: "Renamed", ClaudeSettings: proto.ClaudeSettings{}})
	must(t, err)
	if len(p.ClaudeSettings) != 0 {
		t.Fatalf("{} did not clear: %+v", p.ClaudeSettings)
	}
	_, err = f.svc.SaveProfile(f.ctx, op, "hpf_missing", 1, Profile{Name: "X"})
	code(t, err, apperr.CodePrecondition)
}

// A settings change is audited with the changed top-level keys and the sha256 of the new settings, never the values.
func TestClaudeSettingsChangeAudited(t *testing.T) {
	f := newFx(t)
	secret := "value-that-must-not-be-audited"
	p, err := f.svc.SaveProfile(f.ctx, op, "", 0, Profile{Slug: "claude", Name: "Claude", Harness: "claude-code",
		ClaudeSettings: proto.ClaudeSettings{"env": map[string]any{"FOO": secret}, "model": "opus"}})
	must(t, err)
	next := proto.ClaudeSettings{"env": map[string]any{"FOO": secret + "-2"}, "permissions": map[string]any{"deny": []any{secret}}, "model": "opus"}
	_, err = f.svc.SaveProfile(f.ctx, op, p.ID, p.Version, Profile{Name: "Claude", ClaudeSettings: next})
	must(t, err)
	var details string
	var enc []byte
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT details_json, details_enc FROM audit_events WHERE action = 'profile.updated'`).Scan(&details, &enc))
	stored, _ := json.Marshal(next)
	sum := sha256.Sum256(stored)
	var got map[string]any
	must(t, json.Unmarshal([]byte(details), &got))
	if keys, _ := json.Marshal(got["claude_settings_changed"]); string(keys) != `["env","permissions"]` || got["claude_settings_sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("audit details %s", details)
	}
	if strings.Contains(details, secret) || strings.Contains(details, "opus") || len(enc) != 0 {
		t.Fatalf("audit holds settings values: %s", details)
	}
	// A rename that leaves the settings alone records no settings change.
	p, err = f.svc.GetProfile(f.ctx, p.ID)
	must(t, err)
	_, err = f.svc.SaveProfile(f.ctx, op, p.ID, p.Version, Profile{Name: "Renamed"})
	must(t, err)
	var n int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action = 'profile.updated' AND details_json LIKE '%claude_settings%'`).Scan(&n))
	if n != 1 {
		t.Fatalf("%d settings changes audited, want 1", n)
	}
}
