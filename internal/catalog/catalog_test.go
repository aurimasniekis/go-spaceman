package catalog

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var op = audit.Actor{Type: audit.ActorUser, ID: "usr_op"}

type fx struct {
	t   *testing.T
	ctx context.Context
	s   *store.Store
	svc *Service
}

func newFx(t *testing.T) *fx {
	t.Helper()
	s := storetest.Open(t, storetest.WithClock(clock.NewFake(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))))
	kr := keyringtest.Load(t, s)
	f := &fx{t: t, ctx: context.Background(), s: s, svc: New(s, audit.NewWriter(kr), changefeed.New(nil), Options{})}
	must(t, s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, goals, standing_instructions, created_at, created_by, updated_at)
			 VALUES ('spc_a', 'aaa', 'Payments', 'nod_1', 'active', 'a', 'Ship the retry budget.', 'Never push to main.', 0, 't', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(f.ctx, q); err != nil {
				return err
			}
		}
		return nil
	}))
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func code(t *testing.T, err error, want apperr.Code) {
	t.Helper()
	if apperr.CodeOf(err) != want {
		t.Fatalf("got %v (%s), want %s", err, apperr.CodeOf(err), want)
	}
}

func (f *fx) draft(p *Prompt) *PromptVersion {
	for _, v := range p.Versions {
		if v.State == "draft" {
			return v
		}
	}
	f.t.Fatal("no draft")
	return nil
}

func (f *fx) publish(id string) *PromptVersion {
	f.t.Helper()
	p, err := f.svc.GetPrompt(f.ctx, id)
	must(f.t, err)
	v, err := f.svc.Publish(f.ctx, op, id, f.draft(p).Version)
	must(f.t, err)
	return v
}

func (f *fx) space() db.Space {
	sp, err := f.s.R().GetSpace(f.ctx, "spc_a")
	must(f.t, err)
	return sp
}

// SM-PROMPT-001: drafts, immutable publication, extend, fork with provenance, diff, rollback through a new version.
func TestPromptLifecycle(t *testing.T) {
	f := newFx(t)
	base, err := f.svc.CreatePrompt(f.ctx, op, "engineering-base", "Engineering base", "role", "", "Write tests first.")
	must(t, err)
	_, err = f.svc.CreatePrompt(f.ctx, op, "engineering-base", "Dup", "role", "", "")
	code(t, err, apperr.CodeAlreadyExists)
	for _, bad := range [][3]string{{"Bad Slug", "x", "role"}, {"ok", "", "role"}, {"ok", "x", "poem"}} {
		_, err := f.svc.CreatePrompt(f.ctx, op, bad[0], bad[1], bad[2], "", "")
		code(t, err, apperr.CodeInvalid)
	}
	b1 := f.publish(base.ID)
	if b1.Number != 1 || b1.State != "published" || b1.PublishedBy != "usr_op" {
		t.Fatalf("published %+v", b1)
	}
	// A published version is immutable: editing creates a new draft, which publishes as v2.
	_, err = f.svc.Publish(f.ctx, op, base.ID, 1)
	code(t, err, apperr.CodePrecondition) // no draft
	d, err := f.svc.SaveDraft(f.ctx, op, base.ID, 0, "Write tests first.\nKeep functions small.", "", "second")
	must(t, err)
	_, err = f.svc.SaveDraft(f.ctx, op, base.ID, 0, "x", "", "")
	code(t, err, apperr.CodePrecondition) // a draft exists: give its version
	d, err = f.svc.SaveDraft(f.ctx, op, base.ID, d.Version, "Write tests first.\nKeep functions small.\nUse table tests.", "", "second")
	must(t, err)
	b2, err := f.svc.Publish(f.ctx, op, base.ID, d.Version)
	must(t, err)
	if b2.Number != 2 {
		t.Fatalf("v2 %+v", b2)
	}
	got, _ := f.svc.GetPrompt(f.ctx, base.ID)
	if got.Latest != 2 || len(got.Versions) != 2 || got.Versions[1].Body != "Write tests first." {
		t.Fatalf("versions %+v", got.Versions)
	}
	// Extend: a backend prompt builds on base v1; its content is base then its own text.
	be, err := f.svc.CreatePrompt(f.ctx, op, "backend", "Backend implementor", "role", "", "")
	must(t, err)
	_, err = f.svc.SaveDraft(f.ctx, op, be.ID, f.draft(be).Version, "Go services.", b2.ID, "")
	must(t, err)
	beV1 := f.publish(be.ID)
	c, err := content(f.ctx, f.s.R(), mustVersion(t, f, beV1.ID), 0)
	must(t, err)
	if c != "Write tests first.\nKeep functions small.\nUse table tests.\n\nGo services." {
		t.Fatalf("extended content %q", c)
	}
	_, err = f.svc.SaveDraft(f.ctx, op, be.ID, 0, "x", beV1.ID, "")
	code(t, err, apperr.CodeInvalid) // cannot extend itself
	_, err = f.svc.SaveDraft(f.ctx, op, be.ID, 0, "x", "prv_missing", "")
	code(t, err, apperr.CodeInvalid)
	// Diff between versions.
	lines, err := f.svc.Diff(f.ctx, b1.ID, b2.ID)
	must(t, err)
	var ops []string
	for _, l := range lines {
		ops = append(ops, l.Op)
	}
	if strings.Join(ops, ",") != "same,add,add" {
		t.Fatalf("diff %+v", lines)
	}
	// Rollback publishes v1's content as v3; v1 and v2 stay.
	r, err := f.svc.Rollback(f.ctx, op, base.ID, b1.ID)
	must(t, err)
	if r.Number != 3 || r.Body != "Write tests first." || r.Notes != "Rollback to version 1" {
		t.Fatalf("rollback %+v", r)
	}
	_, err = f.svc.Rollback(f.ctx, op, base.ID, "prv_missing")
	code(t, err, apperr.CodeNotFound)
	// Fork records provenance and keeps what the source extends.
	fk, err := f.svc.Fork(f.ctx, op, beV1.ID, "backend-payments", "Backend (payments)")
	must(t, err)
	if fk.ForkedFrom != beV1.ID || f.draft(fk).Extends != b2.ID || f.draft(fk).Body != "Go services." {
		t.Fatalf("fork %+v", fk)
	}
	// Discard a draft; published versions never go away.
	_, err = f.svc.SaveDraft(f.ctx, op, base.ID, 0, "temp", "", "")
	must(t, err)
	must(t, f.svc.DiscardDraft(f.ctx, op, base.ID))
	code(t, f.svc.DiscardDraft(f.ctx, op, base.ID), apperr.CodeNotFound)
	l, err := f.svc.ListPrompts(f.ctx)
	must(t, err)
	if len(l) != 3 {
		t.Fatalf("list %d", len(l))
	}
	if p, err := f.svc.GetPrompt(f.ctx, "engineering-base"); err != nil || p.ID != base.ID {
		t.Fatalf("by slug %v", err)
	}
}

func mustVersion(t *testing.T, f *fx, id string) db.PromptVersion {
	v, err := f.s.R().GetPromptVersion(f.ctx, id)
	must(t, err)
	return v
}

// SM-PROMPT-002/003/004: layers, field-specific merging, origins, follow vs pin, policy ceilings, manifests.
func TestResolution(t *testing.T) {
	f := newFx(t)
	prof, err := f.svc.SaveProfile(f.ctx, op, "", 0, Profile{Slug: "claude-sonnet", Name: "Claude Sonnet", Harness: "claude-code", Model: "sonnet",
		Settings: map[string]string{"permission_mode": "acceptEdits", "effort": "medium"}})
	must(t, err)
	p, _ := f.svc.CreatePrompt(f.ctx, op, "implementor", "Implementor", "role", "", "You implement one task at a time.")
	v1 := f.publish(p.ID)
	tpl, err := f.svc.SaveTemplate(f.ctx, op, "", 0, RoleTemplate{Slug: "implementor", Name: "Implementor", Kind: "worker", ProfileID: prof.ID, PromptID: p.ID,
		MemoryConventions: "Record decisions in memory/decisions.md.", Settings: map[string]string{"effort": "high"}})
	must(t, err)
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "implementor-backend", TemplateID: tpl.ID, Settings: map[string]string{"permission_mode": "bypassPermissions"},
		Instructions: "Services live in services/."})
	must(t, err)

	r, err := f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: "implementor-backend", Model: "opus"})
	must(t, err)
	if r.Kind != "worker" || r.Harness != "claude-code" || r.Model != "opus" {
		t.Fatalf("resolved %+v", r)
	}
	want := map[string]Field{
		"kind":                     {"worker", OriginTemplate},
		"harness":                  {"claude-code", OriginProfile},
		"model":                    {"opus", OriginLaunch},
		"settings.effort":          {"high", OriginTemplate},
		"settings.permission_mode": {"auto", OriginPolicy}, // the space role asked for bypassPermissions; the ceiling is auto
	}
	for k, w := range want {
		if r.Effective[k] != w {
			t.Errorf("%s = %+v, want %+v", k, r.Effective[k], w)
		}
	}
	if r.Settings["permission_mode"] != "auto" {
		t.Fatalf("ceiling not applied: %v", r.Settings)
	}
	order := []string{"SpaceMan space \"Payments\"", "You implement one task at a time.", "Record decisions in memory/decisions.md.",
		"Ship the retry budget.", "Never push to main.", "Services live in services/."}
	last := -1
	for _, s := range order {
		i := strings.Index(r.SystemText, s)
		if i <= last {
			t.Fatalf("system text order: %q at %d (after %d)\n%s", s, i, last, r.SystemText)
		}
		last = i
	}
	if r.Prompt == nil || r.Prompt.Number != 1 || r.Prompt.Pinned || r.Manifest["template"] == nil || r.Manifest["profile"] == nil {
		t.Fatalf("manifest %+v", r.Manifest)
	}
	// Following: a new version applies to the next launch; pinning keeps the pinned one.
	_, err = f.svc.SaveDraft(f.ctx, op, p.ID, 0, "You implement one task at a time, test first.", "", "")
	must(t, err)
	f.publish(p.ID)
	r2, _ := f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: "implementor-backend"})
	if r2.Prompt.Number != 2 || !strings.Contains(r2.SystemText, "test first") {
		t.Fatalf("follow %+v", r2.Prompt)
	}
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "implementor-backend", TemplateID: tpl.ID, PromptID: p.ID, PromptPin: v1.ID})
	must(t, err)
	r3, _ := f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: "implementor-backend"})
	if r3.Prompt.Number != 1 || !r3.Prompt.Pinned || r3.Effective["settings.permission_mode"].Origin != OriginProfile {
		t.Fatalf("pin %+v %+v", r3.Prompt, r3.Effective)
	}
	// A launch may not change the role's kind or its profile's harness.
	_, err = f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: "implementor-backend", Kind: "control"})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: "implementor-backend", Harness: "codex"})
	code(t, err, apperr.CodeInvalid)
	// Without a configured role, the launch defines kind and harness; built-ins and space instructions still apply.
	r4, err := f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: "reviewer", Kind: "worker", Harness: "codex", SystemText: "Be brief."})
	must(t, err)
	if r4.Effective["harness"].Origin != OriginLaunch || !strings.Contains(r4.SystemText, "Never push to main.") || !strings.HasSuffix(r4.SystemText, "Be brief.") {
		t.Fatalf("no role %+v", r4)
	}
	_, err = f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: "reviewer"})
	code(t, err, apperr.CodeInvalid)
	// Revisions: an update bumps the profile's revision, recorded in later manifests.
	prof2, err := f.svc.SaveProfile(f.ctx, op, prof.ID, prof.Version, Profile{Name: "Claude Sonnet", Model: "sonnet", Settings: map[string]string{}})
	must(t, err)
	if prof2.Revision != 2 {
		t.Fatalf("revision %d", prof2.Revision)
	}
	_, err = f.svc.SaveProfile(f.ctx, op, prof.ID, prof.Version, Profile{Name: "x"})
	code(t, err, apperr.CodePrecondition)
	// A prompt without a published version cannot be used.
	empty, _ := f.svc.CreatePrompt(f.ctx, op, "empty", "Empty", "role", "", "")
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "x-role", TemplateID: tpl.ID, PromptID: empty.ID})
	must(t, err)
	_, err = f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: "x-role"})
	code(t, err, apperr.CodePrecondition)
	must(t, f.svc.DeleteSpaceRole(f.ctx, op, "spc_a", "x-role"))
	code(t, f.svc.DeleteSpaceRole(f.ctx, op, "spc_a", "x-role"), apperr.CodeNotFound)
	roles, _ := f.svc.ListSpaceRoles(f.ctx, "spc_a")
	if len(roles) != 1 || roles[0].Revision != 2 {
		t.Fatalf("roles %+v", roles)
	}
}

func TestValidation(t *testing.T) {
	f := newFx(t)
	_, err := f.svc.SaveProfile(f.ctx, op, "", 0, Profile{Slug: "x", Name: "x", Harness: "vim"})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.SaveProfile(f.ctx, op, "", 0, Profile{Slug: "x", Name: "x", Harness: "codex", Settings: map[string]string{"Bad Key": "v"}})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.SaveTemplate(f.ctx, op, "", 0, RoleTemplate{Slug: "t", Name: "t", Kind: "boss"})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.SaveTemplate(f.ctx, op, "", 0, RoleTemplate{Slug: "t", Name: "t", Kind: "worker", ProfileID: "hpf_missing"})
	code(t, err, apperr.CodeInvalid)
	tpl, err := f.svc.SaveTemplate(f.ctx, op, "", 0, RoleTemplate{Slug: "t", Name: "t", Kind: "worker"})
	must(t, err)
	_, err = f.svc.SaveTemplate(f.ctx, op, "", 0, RoleTemplate{Slug: "t", Name: "t", Kind: "worker"})
	code(t, err, apperr.CodeAlreadyExists)
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "Bad Name", TemplateID: tpl.ID})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_missing", SpaceRole{Name: "ok", TemplateID: tpl.ID})
	code(t, err, apperr.CodeNotFound)
	ts, _ := f.svc.ListTemplates(f.ctx)
	ps, _ := f.svc.ListProfiles(f.ctx)
	if len(ts) != 1 || len(ps) != 0 {
		t.Fatalf("lists %d %d", len(ts), len(ps))
	}
}
