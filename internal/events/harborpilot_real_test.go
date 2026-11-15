package events

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hptest"
)

// SM-GW-007 (live, opt-in: SPACEMAN_HARBOR_PILOT=1, make test-harbor-pilot): a real `harbor-pilot serve --demo` v0.1.3
// relays its events to a Pulse-shaped JetStream server (pulsetest, which devpulse also runs) through its own Pulse
// destination and go-pulse client, and SpaceMan's subscriptions turn them into inbox items with the shipped templates.
// A request over a fresh blocking budget on the first project yields budget.threshold.exceeded (routed to that
// project's space only; the other space subscribes to the second project) and access.request.blocked (routed to the
// space that subscribed to SpaceMan's binding).
func TestRealHarborPilotEventsThroughPulse(t *testing.T) {
	t.Parallel()
	g := hptest.Start(t)
	f := newFx(t)
	ctx := context.Background()
	mine := f.subscribeSlug("spc_a", "hp budgets", "harbor-pilot-budget", map[string]string{"project_id": g.ProjectID})
	other := f.subscribeSlug("spc_b", "hp budgets", "harbor-pilot-budget", map[string]string{"project_id": g.AltProjectID})
	blocked := f.subscribeSlug("spc_a", "hp blocked", "harbor-pilot-access-blocked", map[string]string{"binding_id": g.BindingID, "budget_only": "true"})
	dst, err := g.CreatePulseDestination(ctx, "spaceman-test-pulse", f.nats.URL, f.nats.Creds("harbor-pilot"),
		[]string{"budget.*", "quota.*", "access.request.blocked", "access.credential.issued"})
	if err != nil {
		t.Fatal(err)
	}
	// The relay starts a new destination at the outbox head when it first sees it (it polls every second): earlier
	// events are never sent there. Mint probe credentials until one's access.credential.issued has been delivered.
	for probe := 0; ; probe++ {
		if _, _, err := g.Mint(ctx, hptest.MintRequest{Name: "events probe", Scopes: []string{"gateway:models"}, TTLSeconds: 900}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		code, body, err := g.Do(ctx, http.MethodGet, "/api/v1/event-destinations/"+dst+"/deliveries", g.ServiceToken, nil)
		if err != nil || code != http.StatusOK {
			t.Fatalf("destination deliveries: %d %v", code, err)
		}
		if strings.Contains(string(body), `"harborpilot.access.credential_issued.v1"`) {
			break
		}
		if probe == 40 {
			t.Fatal("the relay never picked up the destination")
		}
	}
	// Harbor Pilot reports a budget's exhaustion once per period, so the budget is new: the demo's own traffic cannot
	// have used up its event before the destination was live.
	budgetID, err := g.CreateBudget(ctx, "spaceman-test-events", g.ProjectID, "0.000001")
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := g.Mint(ctx, hptest.MintRequest{Name: "events over budget", Scopes: []string{"gateway:invoke"}, TTLSeconds: 900,
		BoundRefs: map[string]string{"spaceman.run": "run_hpevents", "project_id": g.ProjectID}})
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := g.Do(ctx, http.MethodPost, "/v1/messages", tok, map[string]any{
		"model": "coding/default", "max_tokens": 16, "messages": []map[string]any{{"role": "user", "content": "hello"}}})
	if err != nil || code != http.StatusTooManyRequests {
		t.Fatalf("request over the blocking budget: %d %v (want 429)", code, err)
	}

	var budget, refusal bool
	deadline := time.Now().Add(60 * time.Second) // the relay retries with backoff
	for !budget || !refusal {
		if time.Now().After(deadline) {
			t.Fatalf("relayed items in A: budget %v, refusal %v (deliveries %v / %v)", budget, refusal,
				f.outcomes("spc_a", mine.ID), f.outcomes("spc_a", blocked.ID))
		}
		time.Sleep(250 * time.Millisecond)
		for _, it := range f.items("spc_a") {
			full, err := f.inbox.Get(f.ctx, "spc_a", it.ID)
			must(t, err)
			if len(full.Attachments) != 1 || full.Trusted["subject"] == nil || !strings.HasPrefix(full.Trusted["subject"].(string), "evt.harborpilot.") {
				t.Fatalf("item %q: attachments %d, trusted %v", full.Title, len(full.Attachments), full.Trusted)
			}
			switch full.SourceID {
			case mine.ID:
				if !strings.Contains(full.Body, "project `"+g.ProjectID+"`") {
					t.Fatalf("space A got another project's budget: %q", full.Title)
				}
				budget = budget || (strings.HasPrefix(full.Title, "Harbor Pilot budget.threshold.exceeded: spaceman-test-events (project, ") &&
					strings.HasPrefix(full.CorrelationID, "harborpilot:budget:"+budgetID+":"))
			case blocked.ID:
				refusal = refusal || (strings.Contains(full.Title, "(binding "+g.BindingID+")") && full.CorrelationID == "harborpilot:blocked:"+g.BindingID &&
					strings.Contains(full.Body, "budget spaceman-test-events is exhausted"))
			}
		}
	}
	// Space B follows the second project: whatever the demo's traffic did there, it never sees the first project's.
	for _, it := range f.items("spc_b") {
		full, err := f.inbox.Get(f.ctx, "spc_b", it.ID)
		must(t, err)
		if full.SourceID != other.ID || !strings.Contains(full.Body, "project `"+g.AltProjectID+"`") {
			t.Fatalf("space B got %q", full.Title)
		}
	}
}
