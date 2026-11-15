package catalog

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
)

// ADR-0077: a claude-code profile can pin the Claude Code version: "", latest, stable or a strict X.Y.Z. The pin is kept
// on an update that says so (the API leaves harness_version out), cleared by "", audited, and resolved into the
// launch's configuration and manifest.
func TestProfileHarnessVersion(t *testing.T) {
	f := newFx(t)
	for _, bad := range []string{"v2.1.3", "2.1", "2.1.3-rc1", "02.1.3", "beta", "2.1.3 "} {
		_, err := f.svc.SaveProfile(f.ctx, op, "", 0, Profile{Slug: "bad", Name: "Bad", Harness: "claude-code", HarnessVersion: bad})
		code(t, err, apperr.CodeInvalid)
	}
	_, err := f.svc.SaveProfile(f.ctx, op, "", 0, Profile{Slug: "cx", Name: "Codex", Harness: "codex", HarnessVersion: "1.2.3"})
	code(t, err, apperr.CodeInvalid)
	for i, ok := range []string{"", "latest", "stable", "0.0.0", "2.1.322"} {
		p, err := f.svc.SaveProfile(f.ctx, op, "", 0, Profile{Slug: fmt.Sprintf("ok-%d", i), Name: "OK", Harness: "claude-code", HarnessVersion: ok})
		must(t, err)
		if p.HarnessVersion != ok {
			t.Fatalf("stored %q, want %q", p.HarnessVersion, ok)
		}
	}

	p, err := f.svc.SaveProfile(f.ctx, op, "", 0, Profile{Slug: "pinned", Name: "Pinned", Harness: "claude-code", HarnessVersion: "2.1.317"})
	must(t, err)
	p, err = f.svc.SaveProfile(f.ctx, op, p.ID, p.Version, Profile{Name: "Renamed", KeepHarnessVersion: true, HarnessVersion: "ignored"})
	must(t, err)
	if p.HarnessVersion != "2.1.317" {
		t.Fatalf("kept pin %q", p.HarnessVersion)
	}
	p, err = f.svc.SaveProfile(f.ctx, op, p.ID, p.Version, Profile{Name: "Renamed", HarnessVersion: "stable"})
	must(t, err)
	var details string
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT details_json FROM audit_events WHERE action = 'profile.updated' ORDER BY seq DESC LIMIT 1`).Scan(&details))
	var got map[string]any
	must(t, json.Unmarshal([]byte(details), &got))
	if hv, _ := got["harness_version"].(map[string]any); hv["from"] != "2.1.317" || hv["to"] != "stable" {
		t.Fatalf("audit %s", details)
	}

	tpl, err := f.svc.SaveTemplate(f.ctx, op, "", 0, RoleTemplate{Slug: "w", Name: "W", Kind: "worker", ProfileID: p.ID})
	must(t, err)
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "w", TemplateID: tpl.ID})
	must(t, err)
	r, err := f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: "w"})
	must(t, err)
	if r.HarnessVersion != "stable" || r.Manifest["harness_version"] != "stable" || r.Effective["harness_version"] != (Field{"stable", OriginProfile}) {
		t.Fatalf("resolved %q %v %+v", r.HarnessVersion, r.Manifest["harness_version"], r.Effective["harness_version"])
	}
	_, err = f.svc.SaveProfile(f.ctx, op, p.ID, p.Version, Profile{Name: "Renamed"})
	must(t, err)
	r, err = f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: "w"})
	must(t, err)
	if r.HarnessVersion != "" || r.Manifest["harness_version"] != nil {
		t.Fatalf("cleared pin still resolved: %q", r.HarnessVersion)
	}
}
