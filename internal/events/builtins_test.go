package events

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/events/template"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// SeedBuiltins publishes every built-in template once, as version 1 of exactly its definition, and an installation
// seeded before a template was added gets the new one at its next start without touching the others.
func TestSeedBuiltinsSeedsEveryTemplate(t *testing.T) {
	// Not parallel: it swaps the package-level builtins.
	ctx := context.Background()
	s := storetest.Open(t, storetest.WithClock(clock.System{}))
	kr := keyringtest.Load(t, s)
	aw := audit.NewWriter(kr)
	ch := changefeed.New(nil)
	att := attachments.New(s, attachments.NewLocal(t.TempDir()), aw, attachments.Options{})
	svc := New(s, kr, aw, ch, inbox.New(s, aw, ch, att), att, Options{Metrics: metrics.New()})

	// An older installation: only the first two built-ins exist.
	all := builtins
	builtins = all[:2]
	must(t, svc.SeedBuiltins(ctx))
	builtins = all
	t.Cleanup(func() { builtins = all })
	before, err := svc.ListTemplates(ctx)
	must(t, err)
	if len(before) != 2 {
		t.Fatalf("%d templates before the upgrade", len(before))
	}
	must(t, svc.SeedBuiltins(ctx))
	must(t, svc.SeedBuiltins(ctx)) // idempotent
	ts, err := svc.ListTemplates(ctx)
	must(t, err)
	bySlug := map[string]*Template{}
	for _, x := range ts {
		bySlug[x.Slug] = x
	}
	want := []string{"github-issue", "github-pull-request", "github-pr-comment", "github-pr-review", "github-pr-review-comment",
		"github-issue-comment", "github-workflow-run", "github-check-suite", "harbor-pilot-budget", "harbor-pilot-quota", "harbor-pilot-access-blocked"}
	if len(ts) != len(want) || len(builtins) != len(want) {
		t.Fatalf("%d templates, %d built-ins", len(ts), len(builtins))
	}
	for i, slug := range want {
		x := bySlug[slug]
		later := builtinLaterVersions[slug]
		if x == nil || !x.Builtin || len(x.Versions) != 1+len(later) {
			t.Fatalf("%s: %+v", slug, x)
		}
		// Versions are newest first: version 1 is last.
		for j, v := range x.Versions {
			k := len(x.Versions) - j
			def, notes := builtins[i].def, builtins[i].notes
			if k > 1 {
				def, notes = later[k-2].def, later[k-2].notes
			}
			got, _ := json.Marshal(v.Definition)
			exp, _ := json.Marshal(def())
			if v.Version != int64(k) || v.Status != "published" || builtins[i].slug != slug || string(got) != string(exp) {
				t.Errorf("%s v%d: stored version differs from the built-in", slug, k)
			}
			if k > 1 && v.Notes != notes {
				t.Errorf("%s v%d: notes %q", slug, k, v.Notes)
			}
		}
		if notes := x.Versions[len(x.Versions)-1].Notes; strings.HasPrefix(slug, "harbor-pilot-") != strings.Contains(notes, "Harbor Pilot v0.1.3") {
			t.Errorf("%s: notes %q", slug, notes)
		}
	}
	if n := len(bySlug["github-pull-request"].Versions); n != 2 {
		t.Errorf("github-pull-request has %d versions", n)
	}
	for _, b := range before {
		got := bySlug[b.Slug]
		if got.ID != b.ID || got.Versions[len(got.Versions)-1].ID != b.Versions[len(b.Versions)-1].ID {
			t.Errorf("%s was seeded again", b.Slug)
		}
	}
}

// A user's template that already uses a built-in's slug keeps it: the built-in is skipped, with a warning naming the
// slug, and the other built-ins are seeded without one.
func TestSeedBuiltinsWarnsWhenASlugIsTaken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t, storetest.WithClock(clock.System{}))
	kr := keyringtest.Load(t, s)
	aw := audit.NewWriter(kr)
	ch := changefeed.New(nil)
	att := attachments.New(s, attachments.NewLocal(t.TempDir()), aw, attachments.Options{})
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := New(s, kr, aw, ch, inbox.New(s, aw, ch, att), att, Options{Metrics: metrics.New(), Log: log})

	mine, err := svc.CreateTemplate(ctx, admin, "github-check-suite", "My check suites", "", template.Definition{Subject: "evt.ci.suite", Title: "Suite"})
	must(t, err)
	must(t, svc.SeedBuiltins(ctx))
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 || !strings.Contains(logs.String(), "slug=github-check-suite") {
		t.Fatalf("warnings: %q", logs.String())
	}
	ts, err := svc.ListTemplates(ctx)
	must(t, err)
	if len(ts) != len(builtins) {
		t.Fatalf("%d templates, %d built-ins", len(ts), len(builtins))
	}
	for _, x := range ts {
		if (x.Slug == "github-check-suite") != (x.ID == mine.ID) || x.Builtin == (x.ID == mine.ID) {
			t.Errorf("%s: id %s, builtin %v", x.Slug, x.ID, x.Builtin)
		}
	}
}

// ADR-0072: an installation seeded before github-pull-request version 2 existed gets it at its next start. Version 1
// stays as it was, a subscription pinned to it stays there, and one that follows the latest version moves to version 2
// with its variables unchanged.
func TestSeedBuiltinsAddsLaterVersions(t *testing.T) {
	// Not parallel: it swaps the package-level builtinLaterVersions.
	later := builtinLaterVersions
	builtinLaterVersions = nil
	t.Cleanup(func() { builtinLaterVersions = later })
	f := newFx(t)
	pr := templateBySlug(t, f.svc, "github-pull-request")
	if len(pr.Versions) != 1 {
		t.Fatalf("%d versions before the upgrade", len(pr.Versions))
	}
	v1 := pr.Versions[0]
	vars := map[string]string{"account_key": "example-org", "repository": "example-org/tool"}
	pinned, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "prs-pinned", ConnectionID: f.conn.ID, TemplateVersionID: v1.ID, Vars: vars})
	must(t, err)
	follows, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "prs-latest", ConnectionID: f.conn.ID, TemplateVersionID: v1.ID,
		Follow: "latest", Vars: vars})
	must(t, err)

	builtinLaterVersions = later
	must(t, f.svc.SeedBuiltins(f.ctx))
	must(t, f.svc.SeedBuiltins(f.ctx)) // idempotent
	pr = templateBySlug(t, f.svc, "github-pull-request")
	if len(pr.Versions) != 2 {
		t.Fatalf("%d versions after the upgrade", len(pr.Versions))
	}
	v2, old := pr.Versions[0], pr.Versions[1]
	gotOld, _ := json.Marshal(old.Definition)
	wantOld, _ := json.Marshal(v1.Definition)
	if old.ID != v1.ID || string(gotOld) != string(wantOld) || old.Notes != v1.Notes || !old.PublishedAt.Equal(*v1.PublishedAt) {
		t.Fatalf("version 1 changed: %+v", old)
	}
	gotV2, _ := json.Marshal(v2.Definition)
	wantV2, _ := json.Marshal(template.GitHubPullRequestV2())
	if v2.Version != 2 || v2.Status != "published" || string(gotV2) != string(wantV2) || !strings.Contains(v2.Notes, "ADR-0072") {
		t.Fatalf("version 2: %+v", v2)
	}
	if g, _ := f.svc.GetSubscription(f.ctx, "spc_a", pinned.ID); g.Template.VersionID != v1.ID || g.Version != pinned.Version {
		t.Errorf("pinned subscription changed: %+v", g.Template)
	}
	g, _ := f.svc.GetSubscription(f.ctx, "spc_a", follows.ID)
	if g.Template.VersionID != v2.ID || g.Subject != follows.Subject || g.Vars["repository"] != "example-org/tool" {
		t.Errorf("following subscription did not move to version 2: %+v %q", g.Template, g.Subject)
	}
	for _, x := range []*Template{templateBySlug(t, f.svc, "github-issue"), templateBySlug(t, f.svc, "github-pr-comment")} {
		if len(x.Versions) != 1 {
			t.Errorf("%s: %d versions", x.Slug, len(x.Versions))
		}
	}
}

// An administrator's own version 2 of a built-in (here a draft) is never replaced or renumbered: seeding skips the
// built-in's later versions with a warning and leaves the draft as it is.
func TestSeedBuiltinsKeepsAnAdministratorsVersion(t *testing.T) {
	// Not parallel: it swaps the package-level builtinLaterVersions.
	ctx := context.Background()
	s := storetest.Open(t, storetest.WithClock(clock.System{}))
	kr := keyringtest.Load(t, s)
	aw := audit.NewWriter(kr)
	ch := changefeed.New(nil)
	att := attachments.New(s, attachments.NewLocal(t.TempDir()), aw, attachments.Options{})
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := New(s, kr, aw, ch, inbox.New(s, aw, ch, att), att, Options{Metrics: metrics.New(), Log: log})

	later := builtinLaterVersions
	builtinLaterVersions = nil
	t.Cleanup(func() { builtinLaterVersions = later })
	must(t, svc.SeedBuiltins(ctx))
	pr := templateBySlug(t, svc, "github-pull-request")
	def := pr.Versions[0].Definition
	def.Urgency = "high"
	draft, err := svc.SaveDraft(ctx, admin, pr.ID, def, "mine")
	must(t, err)

	builtinLaterVersions = later
	must(t, svc.SeedBuiltins(ctx))
	pr = templateBySlug(t, svc, "github-pull-request")
	if len(pr.Versions) != 2 || pr.Versions[0].ID != draft.ID || pr.Versions[0].Status != "draft" || pr.Versions[0].Definition.Urgency != "high" {
		t.Fatalf("versions: %+v", pr.Versions)
	}
	if !strings.Contains(logs.String(), "slug=github-pull-request") || !strings.Contains(logs.String(), "version=2") {
		t.Fatalf("warnings: %q", logs.String())
	}
}

func templateBySlug(t *testing.T, svc *Service, slug string) *Template {
	t.Helper()
	ts, err := svc.ListTemplates(context.Background())
	must(t, err)
	for _, x := range ts {
		if x.Slug == slug {
			return x
		}
	}
	t.Fatalf("no template %s", slug)
	return nil
}
