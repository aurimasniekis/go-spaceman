package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/events"
	"github.com/aurimasniekis/go-spaceman/internal/events/pulsetest"
	eventtemplate "github.com/aurimasniekis/go-spaceman/internal/events/template"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hpfake"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hptest"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// runGatewayWaits runs central's deadline loop with the limit-wait job and the gateway re-check, as Serve does.
func runGatewayWaits(t *testing.T, e *fleetEnv) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.c.Human.Run(ctx, human.Deadline{Name: "limit_waits", Next: e.c.Fleet.NextLimitWait, Due: e.c.Fleet.LimitWaitsDue},
		human.Deadline{Name: "gateway_waits", Next: e.c.HarborPilotWaits.Next, Due: e.c.HarborPilotWaits.Due})
}

// quotaScript switches a gateway's accounts for gatewayQuotaWait.
type quotaScript struct {
	now func() time.Time
	// exhaust makes every account the plan could route to unavailable; afterRun runs once the run has its token.
	exhaust  func(t *testing.T)
	afterRun func(t *testing.T, token string)
	// recover brings the accounts back and lets central notice (the 15-minute re-check, or a kick).
	recover func(t *testing.T)
	// until is the estimate the board must give (nil: any time ahead).
	until *time.Time
}

// gatewayQuotaWait drives one Harbor Pilot run through an all-exhausted wait (ADR-0079 decision 9): the run reports
// the gateway's quota limit, central estimates the hold's reset from the quota board and refuses launches until then,
// the run ends and is parked until the estimate, the accounts recover, central clears the hold from the board, and the
// run resumes as a new run of its instance with --resume of its session and a fresh gateway token.
func gatewayQuotaWait(t *testing.T, h *hpRunEnv, s quotaScript) {
	t.Helper()
	ctx := context.Background()
	e := h.e
	runGatewayWaits(t, e)
	r, err := e.c.Fleet.LaunchRun(ctx, admin, h.sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: "claude-code", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	cmd, p := h.n.launchFor(t, r.InstanceID)
	tok := p.Secrets[proto.SecretGatewayToken]
	if !strings.HasPrefix(tok, "hpk_") {
		t.Fatalf("no gateway token: %v", keys(p.Secrets))
	}
	s.exhaust(t)
	if s.afterRun != nil {
		s.afterRun(t, tok)
	}
	lim := &proto.RunLimit{Kind: proto.LimitGatewayQuota, Since: s.now().UTC()}
	report := func(state, reason string) {
		h.n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.ID, SpaceID: r.SpaceID, Generation: cmd.Generation, State: state, Reason: reason,
			Status: "idle", Turn: 1, SessionID: p.SessionID, Limit: lim, ObservedAt: time.Now().UTC()})
	}
	report(proto.RunRunning, "")

	var hold fleet.GatewayHold
	eventually(t, "the gateway hold estimated from the quota board", func() bool {
		hs, _ := e.c.Fleet.OpenGatewayHolds(ctx)
		if len(hs) == 1 && hs[0].Estimated() {
			hold = hs[0]
			return true
		}
		return false
	})
	if hold.Source != fleet.SourceQuotaBoard || hold.Ref != "harbor-pilot" || hold.SpaceID != h.sp.ID || hold.CheckAt == nil ||
		(s.until != nil && !hold.ResetsAt.Equal(*s.until)) || !hold.ResetsAt.After(s.now()) {
		t.Fatalf("hold %+v (want until %v)", hold, s.until)
	}
	_, err = e.c.Fleet.LaunchRun(ctx, admin, h.sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: "claude-code", Prompt: "go"})
	if he, ok := fleet.AsHold(err); !ok || !he.Known || !strings.Contains(err.Error(), "until about") || !strings.Contains(err.Error(), "quota board") {
		t.Fatalf("a launch while every account is exhausted: %v", err)
	}

	// The run ends at its limit: it is parked until the estimate.
	report(proto.RunExited, "exited")
	eventually(t, "parked", func() bool { return e.run1(r.ID).EndReason == fleet.EndUsageLimit })
	w, err := e.c.Fleet.LimitWaitForRun(ctx, r.ID)
	if err != nil || w == nil || w.State != fleet.WaitWaiting || w.UsageLimitID != hold.ID || !w.ResumeAt.Equal(hold.ResetsAt.Add(2*time.Minute)) {
		t.Fatalf("wait %+v %v", w, err)
	}

	s.recover(t)
	cmd2, p2 := h.n.launchFor(t, r.InstanceID, r.ID)
	if !p2.Resume || p2.SessionID != p.SessionID || p2.Kind != "worker" || !strings.Contains(p2.Prompt, "usage limit; it has reset") {
		t.Fatalf("resume launch %+v", p2)
	}
	if tok2 := p2.Secrets[proto.SecretGatewayToken]; !strings.HasPrefix(tok2, "hpk_") || tok2 == tok {
		t.Fatal("the resumed run has no fresh gateway token")
	}
	u, err := e.c.Store.R().GetUsageLimit(ctx, hold.ID)
	if err != nil || !u.ClearedAt.Valid || u.ClearedBy != fleet.SourceQuotaBoard {
		t.Fatalf("hold after recovery %+v %v", u, err)
	}
	if w, _ := e.c.Fleet.LimitWaitForRun(ctx, r.ID); w.State != fleet.WaitResumed || w.NewRunID != cmd2.RunID {
		t.Fatalf("wait after resume %+v", w)
	}
}

func boardBody(t *testing.T, accounts ...harborpilot.AccountHealth) string {
	t.Helper()
	b, err := json.Marshal(harborpilot.QuotaBoard{AccountHealth: accounts})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ADR-0079 decision 9, UL-8 (simulated: hpfake's quota board, a scripted node, central on a fake clock with its deadline
// loop running). Every account the board lists as unavailable carries an until; a local model stays available and is
// ignored. The estimate is the earliest until. When the accounts recover, the 15-minute re-check clears the hold and
// the parked run resumes.
func TestGatewayQuotaWaitResumes(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	f := hpfake.New(t)
	h := newHPRunEnvOn(t, newFleetEnvClock(t, clk), f.URL(), f.Parent, f.Service, "prj_web")
	until, later := clk.Now().Add(3*time.Hour), clk.Now().Add(5*time.Hour)
	gatewayQuotaWait(t, h, quotaScript{
		now: clk.Now,
		exhaust: func(*testing.T) {
			f.Set(func(x *hpfake.HP) {
				x.Board = boardBody(t, harborpilot.AccountHealth{AccountID: "acc_a", Name: "team-1", Until: &later},
					harborpilot.AccountHealth{AccountID: "acc_b", Name: "team-2", Until: &until},
					harborpilot.AccountHealth{AccountID: "acc_l", Name: "local", Available: true})
			})
		},
		recover: func(*testing.T) {
			f.Set(func(x *hpfake.HP) {
				x.Board = boardBody(t, harborpilot.AccountHealth{AccountID: "acc_a", Name: "team-1", Until: &later},
					harborpilot.AccountHealth{AccountID: "acc_b", Name: "team-2", Available: true},
					harborpilot.AccountHealth{AccountID: "acc_l", Name: "local", Available: true})
			})
			clk.Advance(fleet.GatewayRecheck)
			// The jump outlived the lease: the node's hello renews it (and brings forward a resume put off meanwhile).
			h.dial(t)
			h.e.c.Human.Kick()
		},
		until: &until,
	})
}

// ADR-0079 decision 9, UL-8 (simulated): Harbor Pilot's quota.window.recovered, accepted by a subscription on the
// harbor-pilot-quota template, brings the re-check forward. No time passes: without the event the hold would be read
// again only at its 15-minute check_at; with it, central reads the board at once, the hold clears and the run resumes.
func TestGatewayQuotaRecoveredEventResumes(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	f := hpfake.New(t)
	h := newHPRunEnvOn(t, newFleetEnvClock(t, clk), f.URL(), f.Parent, f.Service, "prj_web")
	ctx := context.Background()
	until := clk.Now().Add(3 * time.Hour)
	gatewayQuotaWait(t, h, quotaScript{
		now: clk.Now,
		exhaust: func(*testing.T) {
			f.Set(func(x *hpfake.HP) {
				x.Board = boardBody(t, harborpilot.AccountHealth{AccountID: "acc_a", Name: "team-1", Until: &until},
					harborpilot.AccountHealth{AccountID: "acc_l", Name: "local", Available: true})
			})
		},
		recover: func(t *testing.T) {
			f.Set(func(x *hpfake.HP) {
				x.Board = boardBody(t, harborpilot.AccountHealth{AccountID: "acc_a", Name: "team-1", Available: true},
					harborpilot.AccountHealth{AccountID: "acc_l", Name: "local", Available: true})
			})
			if next, err := h.e.c.HarborPilotWaits.Next(ctx); err != nil || !next.After(clk.Now()) {
				t.Fatalf("the hold is due before the event: %v %v", next, err)
			}
			nats := pulsetest.Start(t)
			conn, err := h.e.c.Events.CreateConnection(ctx, admin, events.ConnectionInput{Name: "pulse", URL: nats.URL, Creds: string(nats.Creds("spaceman"))})
			if err != nil {
				t.Fatal(err)
			}
			var tv string
			tpls, err := h.e.c.Events.ListTemplates(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, x := range tpls {
				if x.Slug == "harbor-pilot-quota" {
					tv = x.Versions[0].ID
				}
			}
			sub, err := h.e.c.Events.CreateSubscription(ctx, admin, h.sp.ID, events.SubscriptionInput{Name: "quota", ConnectionID: conn.ID, TemplateVersionID: tv})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile("../events/template/testdata/harbor-pilot-quota-window-recovered.json")
			if err != nil {
				t.Fatal(err)
			}
			var env struct{ ID, Name, Type string }
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
			ev := eventtemplate.Event{ID: env.ID, Subject: "evt.harborpilot." + env.Name, Type: env.Type, Data: raw}
			if err := h.e.c.Events.Process(ctx, sub.ID, ev, 1, events.Origin{}); err != nil {
				t.Fatal(err)
			}
			// The kick made the hold due now (or the deadline loop already read the board and cleared it).
			if next, err := h.e.c.HarborPilotWaits.Next(ctx); err != nil || next.After(clk.Now()) {
				t.Fatalf("the event didn't bring the re-check forward: next in %v (%v)", next.Sub(clk.Now()), err)
			}
		},
		until: &until,
	})
}

// ADR-0079 decision 9, UL-8, against a real `harbor-pilot serve --demo` (opt-in: SPACEMAN_HARBOR_PILOT=1; the quota
// board is in v0.1.3 and later; recorded with v0.1.6). Every demo quota monitor is set to exhausted_below 0.99, so all
// monitored accounts are unavailable, and the run's own token gets Harbor Pilot's 429 "no eligible account". Central
// estimates the hold from the real board; when the monitors go back to 0 (recovered), the test calls Kick() directly
// (as central does when a quota.window.recovered event is accepted; there is no Pulse relay here, see
// TestGatewayQuotaRecoveredEventResumes), central reads the board again, the hold clears and the run resumes.
func TestRealHarborPilotQuotaWait(t *testing.T) {
	t.Parallel()
	g := hptest.Start(t)
	ctx := context.Background()
	readBoard := func() harborpilot.QuotaBoard {
		t.Helper()
		code, body, err := g.Do(ctx, http.MethodGet, "/api/v1/quota/board", g.ServiceToken, nil)
		var qb harborpilot.QuotaBoard
		if err != nil || code != http.StatusOK || json.Unmarshal(body, &qb) != nil {
			t.Fatalf("quota board: %d %v", code, err)
		}
		return qb
	}
	monitored := func(qb harborpilot.QuotaBoard, available bool) bool {
		for _, a := range qb.AccountHealth {
			if a.Billing != "local" && a.Available != available {
				return false
			}
		}
		return true
	}
	// The demo starts with its Anthropic accounts cooling down for about 30 s; an estimate taken then would be the
	// cooldown's end.
	eventuallyFor(t, "demo accounts out of their start-up cooldown", time.Minute, func() bool { return monitored(readBoard(), true) })
	h := newHPRunEnv(t, g.URL, g.ParentToken, g.ServiceToken, g.ProjectID)
	gatewayQuotaWait(t, h, quotaScript{
		now: time.Now,
		exhaust: func(t *testing.T) {
			if err := g.SetQuotaExhaustedBelow(ctx, 0.99); err != nil {
				t.Fatal(err)
			}
			eventually(t, "every monitored account unavailable", func() bool { return monitored(readBoard(), false) })
		},
		afterRun: func(t *testing.T, token string) {
			code, body, err := g.Do(ctx, http.MethodPost, "/v1/messages", token, map[string]any{"model": "claude-opus-5-5", "max_tokens": 16,
				"messages": []map[string]any{{"role": "user", "content": "hi"}}})
			if err != nil || code != http.StatusTooManyRequests || !strings.Contains(string(body), "no eligible account is available for model") {
				t.Fatalf("the run's request while every account is exhausted: %d %v %.200s", code, err, body)
			}
		},
		recover: func(t *testing.T) {
			if err := g.SetQuotaExhaustedBelow(ctx, 0); err != nil {
				t.Fatal(err)
			}
			eventually(t, "every monitored account available", func() bool { return monitored(readBoard(), true) })
			h.e.c.HarborPilotWaits.Kick()
		},
	})
}
