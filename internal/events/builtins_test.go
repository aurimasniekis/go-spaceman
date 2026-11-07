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
		"github-issue-comment", "github-workflow-run", "github-check-suite"}
	if len(ts) != len(want) || len(builtins) != len(want) {
		t.Fatalf("%d templates, %d built-ins", len(ts), len(builtins))
	}
	for i, slug := range want {
		x := bySlug[slug]
		if x == nil || !x.Builtin || len(x.Versions) != 1 || x.Versions[0].Version != 1 || x.Versions[0].Status != "published" {
			t.Fatalf("%s: %+v", slug, x)
		}
		got, _ := json.Marshal(x.Versions[0].Definition)
		exp, _ := json.Marshal(builtins[i].def())
		if builtins[i].slug != slug || string(got) != string(exp) {
			t.Errorf("%s: stored definition differs from the built-in", slug)
		}
	}
	for _, b := range before {
		if bySlug[b.Slug].ID != b.ID || bySlug[b.Slug].Versions[0].ID != b.Versions[0].ID {
			t.Errorf("%s was seeded again", b.Slug)
		}
	}
}

// A user's template that already uses a built-in's slug keeps it: the built-in is skipped, with a warning naming the
// slug, and the other built-ins are seeded without one.
func TestSeedBuiltinsWarnsWhenASlugIsTaken(t *testing.T) {
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
