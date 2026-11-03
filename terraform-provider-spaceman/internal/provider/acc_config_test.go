package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// Phase-2 acceptance tests (ADR-0041): limits, queue policy, schedules, cross-space configuration, human-action
// policies, users, grants and linked identities.

func spaceIDBySlug(t *testing.T, slug string) string {
	t.Helper()
	for _, sp := range mustSpaces(t) {
		if sp.Slug == slug {
			return sp.ID
		}
	}
	t.Fatalf("no space %s", slug)
	return ""
}

func limitsHCL(slug, nodeID, limits, queue string, withLimits bool) string {
	s := fmt.Sprintf(`
resource "spaceman_space" "s" {
  slug             = %q
  name             = "Limits"
  node_id          = %q
  purge_on_destroy = true
}
`, slug, nodeID)
	if withLimits {
		s += fmt.Sprintf(`
resource "spaceman_space_limits" "l" {
  space_id = spaceman_space.s.id
%s
}

resource "spaceman_space_queue_policy" "q" {
  space_id = spaceman_space.s.id
%s
}

data "spaceman_space_limits" "e" {
  space_id   = spaceman_space.s.id
  depends_on = [spaceman_space_limits.l, spaceman_space_queue_policy.q]
}
`, limits, queue)
	}
	return s
}

func overridesOf(t *testing.T, spaceID string) map[string]int64 {
	t.Helper()
	l, err := acc.client.GetSpaceLimits(context.Background(), spaceID)
	if err != nil {
		t.Fatal(err)
	}
	return l.Overrides()
}

func setOverride(t *testing.T, spaceID, key string, v int64) {
	t.Helper()
	ctx := context.Background()
	l, err := acc.client.GetSpaceLimits(ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	o := l.Overrides()
	o[key] = v
	if _, err := acc.client.SetSpaceLimits(ctx, spaceID, l.Version, client.SpaceLimitsInput{Values: o, OnLower: l.OnLower}); err != nil {
		t.Fatal(err)
	}
}

func expectOverrides(t *testing.T, slug string, want map[string]int64) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got := overridesOf(t, spaceIDBySlug(t, slug))
		if fmt.Sprint(got) != fmt.Sprint(want) {
			return fmt.Errorf("overrides = %v, want %v", got, want)
		}
		return nil
	}
}

// TestAccLimits covers spaceman_space_limits (only configured overrides are owned; removing one clears it; destroy
// clears the owned ones), spaceman_space_queue_policy and the effective-limits data source.
func TestAccLimits(t *testing.T) {
	testAccPreCheck(t)
	slug := "tf-lim-" + suffix()
	l1 := "  max_concurrent_runs = 4\n  support_depth = 2\n  on_lower = \"stop\""
	l2 := "  max_concurrent_runs = 6\n  budget_warn_percent = 50\n  on_lower = \"stop\""
	q1 := "  max_top_level_tasks = 3"
	accSteps(t,
		resource.TestStep{
			Config: limitsHCL(slug, acc.nodeA.ID, l1, q1, true),
			Check: resource.ComposeAggregateTestCheckFunc(
				expectOverrides(t, slug, map[string]int64{"max_concurrent_runs": 4, "support_depth": 2}),
				resource.TestCheckResourceAttr("spaceman_space_limits.l", "max_concurrent_runs", "4"),
				resource.TestCheckNoResourceAttr("spaceman_space_limits.l", "max_kb_sessions"),
				resource.TestCheckResourceAttr("spaceman_space_queue_policy.q", "max_top_level_tasks", "3"),
				resource.TestCheckResourceAttr("spaceman_space_queue_policy.q", "incident_insertion", "top"),
				resource.TestCheckResourceAttr("data.spaceman_space_limits.e", "values.max_concurrent_runs", "4"),
				resource.TestCheckResourceAttr("data.spaceman_space_limits.e", "values.max_top_level_tasks", "3"),
				resource.TestCheckResourceAttr("data.spaceman_space_limits.e", "overrides.support_depth", "2"),
				resource.TestCheckResourceAttr("data.spaceman_space_limits.e", "on_lower", "stop"),
			),
		},
		resource.TestStep{Config: limitsHCL(slug, acc.nodeA.ID, l1, q1, true), PlanOnly: true},
		// Removing support_depth clears that override; a new attribute adds one.
		resource.TestStep{
			Config: limitsHCL(slug, acc.nodeA.ID, l2, q1, true),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction("spaceman_space_limits.l", plancheck.ResourceActionUpdate)}},
			Check: resource.ComposeAggregateTestCheckFunc(
				expectOverrides(t, slug, map[string]int64{"max_concurrent_runs": 6, "budget_warn_percent": 50}),
				resource.TestCheckNoResourceAttr("spaceman_space_limits.l", "support_depth"),
			),
		},
		resource.TestStep{ResourceName: "spaceman_space_limits.l", ImportState: true, ImportStateVerify: true},
		resource.TestStep{ResourceName: "spaceman_space_queue_policy.q", ImportState: true, ImportStateVerify: true},
		// An override of a limit the configuration does not set is not Terraform's: no drift.
		resource.TestStep{
			PreConfig: func() { setOverride(t, spaceIDBySlug(t, slug), "max_kb_sessions", 7) },
			Config:    limitsHCL(slug, acc.nodeA.ID, l2, q1, true), PlanOnly: true,
		},
		// An owned override edited elsewhere is drift, and applying restores it (keeping the foreign override).
		resource.TestStep{
			PreConfig: func() { setOverride(t, spaceIDBySlug(t, slug), "max_concurrent_runs", 9) },
			Config:    limitsHCL(slug, acc.nodeA.ID, l2, q1, true), PlanOnly: true, ExpectNonEmptyPlan: true,
		},
		resource.TestStep{
			Config: limitsHCL(slug, acc.nodeA.ID, l2, q1, true),
			Check:  expectOverrides(t, slug, map[string]int64{"max_concurrent_runs": 6, "budget_warn_percent": 50, "max_kb_sessions": 7}),
		},
		// Queue policy: an unconfigured field edited in the UI is not drift; a configured one is.
		resource.TestStep{
			PreConfig: func() {
				id := spaceIDBySlug(t, slug)
				p, err := acc.client.GetQueuePolicy(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				p.IncidentPreempts = true
				if _, err := acc.client.SetQueuePolicy(context.Background(), id, *p); err != nil {
					t.Fatal(err)
				}
			},
			Config: limitsHCL(slug, acc.nodeA.ID, l2, q1, true), PlanOnly: true,
		},
		resource.TestStep{
			PreConfig: func() {
				id := spaceIDBySlug(t, slug)
				p, err := acc.client.GetQueuePolicy(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				p.MaxActive = 5
				if _, err := acc.client.SetQueuePolicy(context.Background(), id, *p); err != nil {
					t.Fatal(err)
				}
			},
			Config: limitsHCL(slug, acc.nodeA.ID, l2, q1, true), PlanOnly: true, ExpectNonEmptyPlan: true,
		},
		resource.TestStep{
			Config: limitsHCL(slug, acc.nodeA.ID, l2, q1, true),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_space_queue_policy.q", "max_top_level_tasks", "3"),
				resource.TestCheckResourceAttr("spaceman_space_queue_policy.q", "incident_preempts", "true"),
			),
		},
		// Destroying the limits resource clears the overrides it owns; the foreign one stays.
		resource.TestStep{
			Config: limitsHCL(slug, acc.nodeA.ID, "", "", false),
			Check: resource.ComposeAggregateTestCheckFunc(
				expectOverrides(t, slug, map[string]int64{"max_kb_sessions": 7}),
				func(*terraform.State) error {
					l, err := acc.client.GetSpaceLimits(context.Background(), spaceIDBySlug(t, slug))
					if err != nil {
						return err
					}
					if l.OnLower != "drain" {
						return fmt.Errorf("on_lower = %s after destroy, want drain", l.OnLower)
					}
					return nil
				},
			),
		},
	)
}

func scheduleHCL(slug, nodeID, title, cron string, interval int, anchor string) string {
	return fmt.Sprintf(`
resource "spaceman_space" "s" {
  slug             = %[1]q
  name             = "Schedules"
  node_id          = %[2]q
  purge_on_destroy = true
}

resource "spaceman_schedule" "daily" {
  space_id = spaceman_space.s.id
  name     = "daily-triage"
  kind     = "cron"
  cron     = %[4]q
  timezone = "Europe/Vilnius"
  target   = "inbox"
  title    = %[3]q
  body     = "Look at the open pull requests."
  priority = "high"
}

resource "spaceman_schedule" "every" {
  space_id         = spaceman_space.s.id
  name             = "rotate-logs"
  kind             = "interval"
  interval_seconds = %[5]d
  anchor           = %[6]q
  missed           = "run_once"
  target           = "task"
  title            = "Rotate the logs"
  assignee_role    = "implementor"
}
`, slug, nodeID, title, cron, interval, anchor)
}

// TestAccSchedules covers spaceman_schedule: cron and interval schedules, update, import, drift and destroy.
func TestAccSchedules(t *testing.T) {
	testAccPreCheck(t)
	slug := "tf-sch-" + suffix()
	anchor := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Hour).Format(time.RFC3339)
	var dailyID, spaceID string
	accSteps(t,
		resource.TestStep{
			Config: scheduleHCL(slug, acc.nodeA.ID, "Morning triage", "0 9 * * 1-5", 3600, anchor),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_schedule.daily", "enabled", "true"),
				resource.TestCheckResourceAttr("spaceman_schedule.daily", "missed", "skip"),
				resource.TestCheckResourceAttr("spaceman_schedule.daily", "skip_if_open", "true"),
				resource.TestCheckResourceAttr("spaceman_schedule.daily", "interval_seconds", "0"),
				resource.TestCheckResourceAttrSet("spaceman_schedule.daily", "anchor"),
				resource.TestCheckResourceAttr("spaceman_schedule.every", "anchor", anchor),
				resource.TestCheckResourceAttr("spaceman_schedule.every", "catch_up_max", "3"),
				func(s *terraform.State) error {
					dailyID, spaceID = stateAttr(s, "spaceman_schedule.daily", "id"), stateAttr(s, "spaceman_space.s", "id")
					return nil
				},
			),
		},
		resource.TestStep{Config: scheduleHCL(slug, acc.nodeA.ID, "Morning triage", "0 9 * * 1-5", 3600, anchor), PlanOnly: true},
		resource.TestStep{
			Config: scheduleHCL(slug, acc.nodeA.ID, "Morning triage v2", "@daily", 7200, anchor),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction("spaceman_schedule.daily", plancheck.ResourceActionUpdate),
				plancheck.ExpectResourceAction("spaceman_schedule.every", plancheck.ResourceActionUpdate)}},
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_schedule.daily", "cron", "@daily"),
				resource.TestCheckResourceAttr("spaceman_schedule.daily", "title", "Morning triage v2"),
				resource.TestCheckResourceAttr("spaceman_schedule.every", "interval_seconds", "7200"),
				resource.TestCheckResourceAttr("spaceman_schedule.every", "anchor", anchor),
			),
		},
		resource.TestStep{Config: scheduleHCL(slug, acc.nodeA.ID, "Morning triage v2", "@daily", 7200, anchor), PlanOnly: true},
		resource.TestStep{ResourceName: "spaceman_schedule.daily", ImportState: true, ImportStateVerify: true,
			ImportStateIdFunc: func(s *terraform.State) (string, error) {
				return stateAttr(s, "spaceman_schedule.daily", "space_id") + "/" + stateAttr(s, "spaceman_schedule.daily", "id"), nil
			}},
		resource.TestStep{ResourceName: "spaceman_schedule.every", ImportState: true, ImportStateVerify: true,
			ImportStateIdFunc: func(s *terraform.State) (string, error) {
				return stateAttr(s, "spaceman_schedule.every", "space_id") + "/" + stateAttr(s, "spaceman_schedule.every", "id"), nil
			}},
		// A field the configuration does not set (description) edited in the UI is not drift; a configured one is.
		resource.TestStep{
			PreConfig: func() {
				editSchedule(t, spaceID, dailyID, func(in *client.ScheduleInput) { in.Description = "from the UI" })
			},
			Config: scheduleHCL(slug, acc.nodeA.ID, "Morning triage v2", "@daily", 7200, anchor), PlanOnly: true,
		},
		resource.TestStep{
			PreConfig: func() {
				editSchedule(t, spaceID, dailyID, func(in *client.ScheduleInput) { in.Title = "Renamed in the UI" })
			},
			Config: scheduleHCL(slug, acc.nodeA.ID, "Morning triage v2", "@daily", 7200, anchor), PlanOnly: true, ExpectNonEmptyPlan: true,
		},
		resource.TestStep{
			Config: scheduleHCL(slug, acc.nodeA.ID, "Morning triage v2", "@daily", 7200, anchor),
			Check: apiCheck("spaceman_schedule.daily", func(ctx context.Context, c *client.Client, id string, _ *terraform.State) error {
				s, err := c.GetSchedule(ctx, spaceID, id)
				if err != nil {
					return err
				}
				if s.Title != "Morning triage v2" || s.Description != "from the UI" {
					return fmt.Errorf("title %q description %q after correcting drift", s.Title, s.Description)
				}
				return nil
			}),
		},
		// Destroying a schedule deletes it.
		resource.TestStep{
			Config: limitsHCL(slug, acc.nodeA.ID, "", "", false),
			Check: func(*terraform.State) error {
				if _, err := acc.client.GetSchedule(context.Background(), spaceID, dailyID); !client.IsNotFound(err) {
					return fmt.Errorf("schedule after destroy: %w", err)
				}
				return nil
			},
		},
	)
}

func editSchedule(t *testing.T, spaceID, id string, edit func(*client.ScheduleInput)) {
	t.Helper()
	ctx := context.Background()
	s, err := acc.client.GetSchedule(ctx, spaceID, id)
	if err != nil {
		t.Fatal(err)
	}
	in := client.ScheduleInput{Name: s.Name, Description: s.Description, Enabled: &s.Enabled, Kind: s.Kind, Cron: s.Cron, IntervalSeconds: s.IntervalSeconds,
		Timezone: s.Timezone, Missed: s.Missed, CatchUpMax: s.CatchUpMax, SkipIfOpen: &s.SkipIfOpen, Target: s.Target, Title: s.Title, Body: s.Body,
		Priority: s.Priority, AssigneeRole: s.AssigneeRole}
	edit(&in)
	if _, err := acc.client.UpdateSchedule(ctx, spaceID, id, s.Version, in); err != nil {
		t.Fatal(err)
	}
}

func xspaceHCL(slugA, slugB, nodeID, inbound string, withPeers bool) string {
	s := fmt.Sprintf(`
resource "spaceman_space" "a" {
  slug             = %q
  name             = "Payments"
  node_id          = %q
  purge_on_destroy = true
}

resource "spaceman_space" "b" {
  slug             = %q
  name             = "Platform"
  node_id          = %q
  purge_on_destroy = true
}
`, slugA, nodeID, slugB, nodeID)
	if withPeers {
		s += fmt.Sprintf(`
resource "spaceman_space_xspace_settings" "a" {
  space_id  = spaceman_space.a.id
  profile   = "Owns the payment API. Ask about refunds."
  max_depth = 2
}

# A accepts questions from B ...
resource "spaceman_xspace_peer" "a_from_b" {
  space_id = spaceman_space.a.id
  peer_id  = spaceman_space.b.id
  inbound = {
%s
  }
}

# ... and B's control may send them.
resource "spaceman_xspace_peer" "b_to_a" {
  space_id = spaceman_space.b.id
  peer_id  = spaceman_space.a.id
  outbound = {
    questions = true
  }
}
`, inbound)
	}
	return s
}

// TestAccCrossSpace covers spaceman_space_xspace_settings and spaceman_xspace_peer (both sides of a grant).
func TestAccCrossSpace(t *testing.T) {
	testAccPreCheck(t)
	slugA, slugB := "tf-xa-"+suffix(), "tf-xb-"+suffix()
	in1 := `    discover        = true
    questions       = true
    disclosure      = "summary"
    knowledge_bases = ["docs"]`
	in2 := `    discover     = true
    questions    = true
    delegation   = true
    disclosure   = "summary"
    max_per_hour = 10`
	var aID, bID string
	accSteps(t,
		resource.TestStep{
			Config: xspaceHCL(slugA, slugB, acc.nodeA.ID, in1, true),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_space_xspace_settings.a", "max_depth", "2"),
				resource.TestCheckResourceAttr("spaceman_space_xspace_settings.a", "max_outgoing_per_hour", "60"),
				resource.TestCheckResourceAttr("spaceman_xspace_peer.a_from_b", "inbound.questions", "true"),
				resource.TestCheckResourceAttr("spaceman_xspace_peer.a_from_b", "inbound.delegation", "false"),
				resource.TestCheckResourceAttr("spaceman_xspace_peer.a_from_b", "inbound.max_pending", "5"),
				resource.TestCheckResourceAttr("spaceman_xspace_peer.a_from_b", "outbound.questions", "false"),
				resource.TestCheckResourceAttr("spaceman_xspace_peer.b_to_a", "outbound.questions", "true"),
				resource.TestCheckResourceAttr("spaceman_xspace_peer.b_to_a", "inbound.discover", "false"),
				func(s *terraform.State) error {
					aID, bID = stateAttr(s, "spaceman_space.a", "id"), stateAttr(s, "spaceman_space.b", "id")
					return nil
				},
			),
		},
		resource.TestStep{Config: xspaceHCL(slugA, slugB, acc.nodeA.ID, in1, true), PlanOnly: true},
		resource.TestStep{
			Config: xspaceHCL(slugA, slugB, acc.nodeA.ID, in2, true),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_xspace_peer.a_from_b", "inbound.delegation", "true"),
				resource.TestCheckResourceAttr("spaceman_xspace_peer.a_from_b", "inbound.max_per_hour", "10"),
				resource.TestCheckResourceAttr("spaceman_xspace_peer.a_from_b", "inbound.knowledge_bases.#", "0"),
			),
		},
		resource.TestStep{ResourceName: "spaceman_space_xspace_settings.a", ImportState: true, ImportStateVerify: true},
		resource.TestStep{ResourceName: "spaceman_xspace_peer.a_from_b", ImportState: true, ImportStateVerify: true},
		resource.TestStep{ResourceName: "spaceman_xspace_peer.b_to_a", ImportState: true, ImportStateVerify: true},
		// A grant widened outside Terraform is drift (the peer configuration is owned whole) ...
		resource.TestStep{
			PreConfig: func() {
				ctx := context.Background()
				p, err := acc.client.GetXSpacePeer(ctx, aID, bID)
				if err != nil {
					t.Fatal(err)
				}
				p.Inbound.Knowledge = true
				if _, err := acc.client.SetXSpacePeer(ctx, aID, bID, p.Version, client.XSpacePeerInput{Inbound: p.Inbound, Outbound: p.Outbound}); err != nil {
					t.Fatal(err)
				}
			},
			Config: xspaceHCL(slugA, slugB, acc.nodeA.ID, in2, true), PlanOnly: true, ExpectNonEmptyPlan: true,
		},
		resource.TestStep{
			Config: xspaceHCL(slugA, slugB, acc.nodeA.ID, in2, true),
			Check: func(*terraform.State) error {
				p, err := acc.client.GetXSpacePeer(context.Background(), aID, bID)
				if err != nil {
					return err
				}
				if p.Inbound.Knowledge {
					return fmt.Errorf("the widened grant was not reverted")
				}
				return nil
			},
		},
		// ... while a settings field the configuration leaves out is not.
		resource.TestStep{
			PreConfig: func() {
				ctx := context.Background()
				s, err := acc.client.GetXSpaceSettings(ctx, aID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := acc.client.SetXSpaceSettings(ctx, aID, s.Version, client.XSpaceSettingsInput{Profile: s.Profile, MaxDepth: s.MaxDepth,
					MaxOutgoingPerHour: 30}); err != nil {
					t.Fatal(err)
				}
			},
			Config: xspaceHCL(slugA, slugB, acc.nodeA.ID, in2, true), PlanOnly: true,
		},
		// Destroy removes both sides and resets the settings.
		resource.TestStep{
			Config: xspaceHCL(slugA, slugB, acc.nodeA.ID, "", false),
			Check: func(*terraform.State) error {
				ctx := context.Background()
				for _, pair := range [][2]string{{aID, bID}, {bID, aID}} {
					if _, err := acc.client.GetXSpacePeer(ctx, pair[0], pair[1]); !client.IsNotFound(err) {
						return fmt.Errorf("peer configuration after destroy: %w", err)
					}
				}
				s, err := acc.client.GetXSpaceSettings(ctx, aID)
				if err != nil {
					return err
				}
				if s.Profile != "" || s.MaxDepth != 3 || s.MaxOutgoingPerHour != 60 {
					return fmt.Errorf("settings after destroy: %+v", s)
				}
				return nil
			},
		},
	)
}

func humanPolicyHCL(slug, nodeID string, remind int, extra string) string {
	return fmt.Sprintf(`
resource "spaceman_space" "s" {
  slug             = %q
  name             = "Policies"
  node_id          = %q
  purge_on_destroy = true
}

resource "spaceman_human_action_policy" "todo_default" {
  action_type          = "todo"
  remind_after_minutes = %d
}

resource "spaceman_human_action_policy" "question" {
  space_id               = spaceman_space.s.id
  action_type            = "question"
  escalate_after_minutes = 60
  escalate_priority      = "urgent"
  requester_ended        = "next_launch"
  hold_minutes           = 30
}
%s`, slug, nodeID, remind, extra)
}

// TestAccHumanPolicies covers spaceman_human_action_policy: installation defaults and space overrides (SM-HUI-008).
func TestAccHumanPolicies(t *testing.T) {
	testAccPreCheck(t)
	slug := "tf-hp-" + suffix()
	var spaceID string
	accSteps(t,
		resource.TestStep{
			Config: humanPolicyHCL(slug, acc.nodeA.ID, 120, ""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_human_action_policy.todo_default", "space_id", ""),
				resource.TestCheckResourceAttr("spaceman_human_action_policy.todo_default", "remind_after_minutes", "120"),
				// Fields left out start from the policy in force (the built-in todo policy).
				resource.TestCheckResourceAttr("spaceman_human_action_policy.todo_default", "max_reminders", "1"),
				resource.TestCheckResourceAttr("spaceman_human_action_policy.todo_default", "rate_per_run_hour", "30"),
				resource.TestCheckResourceAttr("spaceman_human_action_policy.question", "escalate_priority", "urgent"),
				resource.TestCheckResourceAttr("spaceman_human_action_policy.question", "remind_after_minutes", "240"),
				resource.TestCheckResourceAttr("spaceman_human_action_policy.question", "max_reminders", "3"),
				func(s *terraform.State) error { spaceID = stateAttr(s, "spaceman_space.s", "id"); return nil },
			),
		},
		resource.TestStep{Config: humanPolicyHCL(slug, acc.nodeA.ID, 120, ""), PlanOnly: true},
		resource.TestStep{
			Config: humanPolicyHCL(slug, acc.nodeA.ID, 180, ""),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction("spaceman_human_action_policy.todo_default", plancheck.ResourceActionUpdate)}},
			Check: resource.TestCheckResourceAttr("spaceman_human_action_policy.todo_default", "remind_after_minutes", "180"),
		},
		resource.TestStep{ResourceName: "spaceman_human_action_policy.todo_default", ImportState: true, ImportStateVerify: true, ImportStateId: "todo"},
		resource.TestStep{ResourceName: "spaceman_human_action_policy.question", ImportState: true, ImportStateVerify: true,
			ImportStateIdFunc: func(s *terraform.State) (string, error) {
				return stateAttr(s, "spaceman_space.s", "id") + "/question", nil
			}},
		// An unconfigured field edited in the UI is not drift; a configured one is.
		resource.TestStep{
			PreConfig: func() {
				editPolicy(t, spaceID, "question", func(f *client.HumanPolicyFields) { f.RatePerRunHour = 12 })
			},
			Config: humanPolicyHCL(slug, acc.nodeA.ID, 180, ""), PlanOnly: true,
		},
		resource.TestStep{
			PreConfig: func() {
				editPolicy(t, spaceID, "question", func(f *client.HumanPolicyFields) { f.EscalatePriority = "high" })
			},
			Config: humanPolicyHCL(slug, acc.nodeA.ID, 180, ""), PlanOnly: true, ExpectNonEmptyPlan: true,
		},
		resource.TestStep{
			Config: humanPolicyHCL(slug, acc.nodeA.ID, 180, ""),
			Check: func(*terraform.State) error {
				p, err := acc.client.GetHumanPolicy(context.Background(), spaceID, "question")
				if err != nil {
					return err
				}
				if p.EscalatePriority != "urgent" || p.RatePerRunHour != 12 {
					return fmt.Errorf("policy after correcting drift: %+v", p.HumanPolicyFields)
				}
				return nil
			},
		},
		// A policy that already exists is never overwritten by a create.
		resource.TestStep{
			PreConfig: func() {
				if _, err := acc.client.SetHumanPolicy(context.Background(), spaceID, "message", 0, client.HumanPolicyFields{MaxReminders: 1,
					RequesterEnded: "reroute", HoldMinutes: 60, RatePerRunHour: 5, RatePerSpaceHour: 50}); err != nil {
					t.Fatal(err)
				}
			},
			Config: humanPolicyHCL(slug, acc.nodeA.ID, 180, `
resource "spaceman_human_action_policy" "message" {
  space_id          = spaceman_space.s.id
  action_type       = "message"
  rate_per_run_hour = 7
}
`),
			ExpectError: regexp.MustCompile(`The policy already exists`),
		},
		resource.TestStep{
			Config: humanPolicyHCL(slug, acc.nodeA.ID, 180, ""),
			Check: func(*terraform.State) error {
				p, err := acc.client.GetHumanPolicy(context.Background(), spaceID, "message")
				if err != nil {
					return err
				}
				if p.RatePerRunHour != 5 {
					return fmt.Errorf("an existing policy was overwritten: %+v", p.HumanPolicyFields)
				}
				return acc.client.DeleteHumanPolicy(context.Background(), spaceID, "message")
			},
		},
	)
	if _, err := acc.client.GetHumanPolicy(context.Background(), "", "todo"); !client.IsNotFound(err) {
		t.Fatalf("installation default after destroy: %v", err)
	}
}

func editPolicy(t *testing.T, spaceID, typ string, edit func(*client.HumanPolicyFields)) {
	t.Helper()
	ctx := context.Background()
	p, err := acc.client.GetHumanPolicy(ctx, spaceID, typ)
	if err != nil {
		t.Fatal(err)
	}
	f := p.HumanPolicyFields
	edit(&f)
	if _, err := acc.client.SetHumanPolicy(ctx, spaceID, typ, p.Version, f); err != nil {
		t.Fatal(err)
	}
}

func usersHCL(sfx, slug, nodeID, display, grants, enabled string) string {
	return fmt.Sprintf(`
resource "spaceman_space" "s" {
  slug             = %[2]q
  name             = "Users"
  node_id          = %[3]q
  purge_on_destroy = true
}

resource "spaceman_user" "u" {
  email        = "tf-user-%[1]s@example.com"
  display_name = %[4]q
%[6]s
}

# Created disabled (create, then disable).
resource "spaceman_user" "off" {
  email        = "tf-off-%[1]s@example.com"
  display_name = "TF disabled"
  enabled      = false
}

resource "spaceman_user_grants" "u" {
  user_id = spaceman_user.u.id
  grant   = [%[5]s]
}

resource "spaceman_oidc_provider" "p" {
  slug         = "tfu%[1]s"
  display_name = "Users SSO"
  issuer       = "https://idp.example.com/realms/tf-users"
  client_id    = "spaceman-users"
}

resource "spaceman_user_identity" "u" {
  user_id     = spaceman_user.u.id
  provider_id = spaceman_oidc_provider.p.id
  subject     = "u_tf%[1]s"
}

data "spaceman_user" "u" {
  email      = "tf-user-%[1]s@example.com"
  depends_on = [spaceman_user_grants.u]
}
`, sfx, slug, nodeID, display, grants, enabled)
}

// TestAccUsers covers spaceman_user, spaceman_user_grants, spaceman_user_identity and the user data source.
func TestAccUsers(t *testing.T) {
	testAccPreCheck(t)
	ctx := context.Background()
	// SpaceMan keeps one enabled global administrator once users exist; this one stays, so the users under test can
	// lose their grants and be disabled.
	if _, err := acc.client.CreateUser(ctx, client.CreateUserInput{Email: "tf-keeper-" + suffix() + "@example.com", DisplayName: "TF keeper",
		Grants: []client.GrantInput{{Role: "admin", Domain: "*"}}}); err != nil {
		t.Fatal(err)
	}
	sfx, slug := suffix(), "tf-usr-"+suffix()
	g1 := `{ role = "viewer", domain = "*" }, { role = "operator", domain = spaceman_space.s.id }`
	g2 := `{ role = "admin", domain = spaceman_space.s.id }`
	var userID string
	accCase(t, func(*terraform.State) error {
		u, err := acc.client.GetUser(ctx, userID)
		if err != nil {
			return err
		}
		if u.Enabled || len(u.ManualGrants()) != 0 {
			return fmt.Errorf("after destroy: enabled %v, grants %v", u.Enabled, u.ManualGrants())
		}
		if l, err := acc.client.ListUserIdentities(ctx, userID); err != nil || len(l) != 0 {
			return fmt.Errorf("identities after destroy: %v %w", l, err)
		}
		return nil
	},
		resource.TestStep{
			Config: usersHCL(sfx, slug, acc.nodeA.ID, "TF user", g1, ""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_user.u", "enabled", "true"),
				resource.TestCheckResourceAttr("spaceman_user.off", "enabled", "false"),
				resource.TestCheckResourceAttr("spaceman_user.u", "provisioned_via", "bootstrap"),
				resource.TestCheckResourceAttr("spaceman_user_grants.u", "grant.#", "2"),
				resource.TestCheckResourceAttrPair("spaceman_user_identity.u", "user_id", "spaceman_user.u", "id"),
				resource.TestCheckResourceAttr("spaceman_user_identity.u", "issuer", "https://idp.example.com/realms/tf-users"),
				resource.TestCheckResourceAttrPair("data.spaceman_user.u", "id", "spaceman_user.u", "id"),
				resource.TestCheckResourceAttr("data.spaceman_user.u", "grants.#", "2"),
				func(s *terraform.State) error { userID = stateAttr(s, "spaceman_user.u", "id"); return nil },
			),
		},
		resource.TestStep{Config: usersHCL(sfx, slug, acc.nodeA.ID, "TF user", g1, ""), PlanOnly: true},
		resource.TestStep{
			Config: usersHCL(sfx, slug, acc.nodeA.ID, "TF user", g2, "  enabled = false"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_user.u", "enabled", "false"),
				resource.TestCheckResourceAttr("spaceman_user_grants.u", "grant.#", "1"),
				resource.TestCheckResourceAttr("spaceman_user_grants.u", "grant.0.role", "admin"),
			),
		},
		resource.TestStep{ResourceName: "spaceman_user.u", ImportState: true, ImportStateVerify: true},
		resource.TestStep{ResourceName: "spaceman_user_grants.u", ImportState: true, ImportStateVerify: true},
		resource.TestStep{ResourceName: "spaceman_user_identity.u", ImportState: true, ImportStateVerify: true},
		// Grants edited in the UI are drift for the owned grant set.
		resource.TestStep{
			PreConfig: func() {
				u, err := acc.client.GetUser(ctx, userID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := acc.client.SetUserGrants(ctx, userID, u.Version, append(u.ManualGrants(), client.GrantInput{Role: "admin", Domain: "*"})); err != nil {
					t.Fatal(err)
				}
			},
			Config: usersHCL(sfx, slug, acc.nodeA.ID, "TF user", g2, "  enabled = false"), PlanOnly: true, ExpectNonEmptyPlan: true,
		},
		resource.TestStep{
			Config: usersHCL(sfx, slug, acc.nodeA.ID, "TF user", g2, "  enabled = false"),
			Check: func(*terraform.State) error {
				u, err := acc.client.GetUser(ctx, userID)
				if err != nil {
					return err
				}
				if g := u.ManualGrants(); len(g) != 1 || g[0].Role != "admin" || g[0].Domain == "*" {
					return fmt.Errorf("grants after correcting drift: %v", g)
				}
				return nil
			},
		},
		// SpaceMan cannot rename a user or change the email: refused at plan time.
		resource.TestStep{
			Config:      usersHCL(sfx, slug, acc.nodeA.ID, "TF user renamed", g2, "  enabled = false"),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`A user's display name cannot be changed`),
		},
		resource.TestStep{
			Config:      strings.Replace(usersHCL(sfx, slug, acc.nodeA.ID, "TF user", g2, ""), `email        = "tf-user-`, `email        = "TF-User-`, 1),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`must be an email address in lower case`),
		},
		resource.TestStep{Config: usersHCL(sfx, slug, acc.nodeA.ID, "TF user", g2, "  enabled = false"), PlanOnly: true},
	)
}
