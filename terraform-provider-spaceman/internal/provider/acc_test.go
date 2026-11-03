package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// Acceptance tests run against the dev server the harness starts (TF_ACC=1). Secrets are generated per run.

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func suffix() string { return randHex(4) }

func accSteps(t *testing.T, steps ...resource.TestStep) {
	t.Helper()
	accCase(t, nil, steps...)
}

func accCase(t *testing.T, checkDestroy resource.TestCheckFunc, steps ...resource.TestStep) {
	t.Helper()
	testAccPreCheck(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_11_0)},
		CheckDestroy:             checkDestroy,
		Steps:                    steps,
	})
}

func stateAttr(s *terraform.State, res, key string) string {
	r := s.RootModule().Resources[res]
	if r == nil {
		return ""
	}
	return r.Primary.Attributes[key]
}

// apiCheck runs fn against the API with the resource's ID.
func apiCheck(res string, fn func(ctx context.Context, c *client.Client, id string, s *terraform.State) error) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		return fn(context.Background(), acc.client, stateAttr(s, res, "id"), s)
	}
}

func spaceHCL(slug, name, goals, nodeID string) string {
	return fmt.Sprintf(`
resource "spaceman_space" "s" {
  slug             = %q
  name             = %q
  goals            = %q
  node_id          = %q
  purge_on_destroy = true
}
`, slug, name, goals, nodeID)
}

// TestAccSpace covers create, update, import, drift of a managed field, no drift for an unmanaged field, and a node_id
// change planned in place and sent to SpaceMan as a relocation.
func TestAccSpace(t *testing.T) {
	testAccPreCheck(t)
	slug := "tf-space-" + suffix()
	var spaceID string
	accSteps(t,
		resource.TestStep{
			Config: spaceHCL(slug, "TF space", "ship it", acc.nodeA.ID),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_space.s", "slug", slug),
				resource.TestCheckResourceAttr("spaceman_space.s", "state", "active"),
				resource.TestCheckResourceAttr("spaceman_space.s", "disconnect_policy", "interrupt"),
				resource.TestCheckResourceAttrSet("spaceman_space.s", "workdir"),
				resource.TestCheckResourceAttr("spaceman_space.s", "node_id", acc.nodeA.ID),
				func(s *terraform.State) error { spaceID = stateAttr(s, "spaceman_space.s", "id"); return nil },
			),
		},
		resource.TestStep{
			Config: spaceHCL(slug, "TF space renamed", "ship it twice", acc.nodeA.ID),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction("spaceman_space.s", plancheck.ResourceActionUpdate)}},
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_space.s", "name", "TF space renamed"),
				resource.TestCheckResourceAttr("spaceman_space.s", "goals", "ship it twice"),
			),
		},
		resource.TestStep{
			ResourceName: "spaceman_space.s", ImportState: true, ImportStateVerify: true,
			ImportStateVerifyIgnore: []string{"purge_on_destroy"},
		},
		// A managed field edited outside Terraform (the UI uses the same API) is drift.
		resource.TestStep{
			PreConfig: func() {
				ctx := context.Background()
				s, err := acc.client.GetSpace(ctx, spaceID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := acc.client.UpdateSpace(ctx, spaceID, s.Version, client.UpdateSpaceInput{Name: s.Name, Description: s.Description,
					Goals: "edited in the UI", StandingInstructions: s.StandingInstructions, DisconnectPolicy: s.DisconnectPolicy}); err != nil {
					t.Fatal(err)
				}
			},
			Config:             spaceHCL(slug, "TF space renamed", "ship it twice", acc.nodeA.ID),
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		},
		// Applying corrects the drift (the If-Match version is the one read during refresh).
		resource.TestStep{
			Config: spaceHCL(slug, "TF space renamed", "ship it twice", acc.nodeA.ID),
			Check: apiCheck("spaceman_space.s", func(ctx context.Context, c *client.Client, id string, _ *terraform.State) error {
				s, err := c.GetSpace(ctx, id)
				if err != nil {
					return err
				}
				if s.Goals != "ship it twice" {
					return fmt.Errorf("goals = %q after correcting drift", s.Goals)
				}
				return nil
			}),
		},
		// A field the configuration does not set is not owned by Terraform: editing it in the UI is not drift.
		resource.TestStep{
			PreConfig: func() {
				ctx := context.Background()
				s, err := acc.client.GetSpace(ctx, spaceID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := acc.client.UpdateSpace(ctx, spaceID, s.Version, client.UpdateSpaceInput{Name: s.Name, Description: "described in the UI",
					Goals: s.Goals, StandingInstructions: s.StandingInstructions, DisconnectPolicy: s.DisconnectPolicy}); err != nil {
					t.Fatal(err)
				}
			},
			Config:   spaceHCL(slug, "TF space renamed", "ship it twice", acc.nodeA.ID),
			PlanOnly: true,
		},
		// A node_id change plans an in-place update (never a destroy and recreate) and applies as a SpaceMan relocation.
		// This harness runs no node agents, so SpaceMan refuses it (the destination never enrolled) and the space stays
		// where it is; relocations that complete are covered by internal/app TestRelocation* and TestRelocationOnNodeChange.
		resource.TestStep{
			Config: spaceHCL(slug, "TF space renamed", "ship it twice", acc.nodeB.ID),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction("spaceman_space.s", plancheck.ResourceActionUpdate)}},
			ExpectError: regexp.MustCompile(`(?s)SpaceMan refused to relocate the space.*destination node tf-node-b is pending`),
		},
		resource.TestStep{
			Config: spaceHCL(slug, "TF space renamed", "ship it twice", acc.nodeA.ID),
			Check: apiCheck("spaceman_space.s", func(ctx context.Context, c *client.Client, id string, _ *terraform.State) error {
				s, err := c.GetSpace(ctx, id)
				if err != nil {
					return err
				}
				if s.NodeID != acc.nodeA.ID || s.DesiredNodeID != "" {
					return fmt.Errorf("a refused relocation moved the space: node %s desired %s", s.NodeID, s.DesiredNodeID)
				}
				if _, err := c.GetRelocation(ctx, id); !client.IsNotFound(err) {
					return fmt.Errorf("a refused relocation was recorded: %w", err)
				}
				return nil
			}),
		},
		resource.TestStep{
			Config:      spaceHCL("tf-other-"+suffix(), "TF space renamed", "ship it twice", acc.nodeA.ID),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`A space's slug cannot be changed`),
		},
	)
	// Destroy archived and purged it.
	if _, err := acc.client.GetSpace(context.Background(), spaceID); !client.IsNotFound(err) {
		t.Fatalf("space after destroy: %v", err)
	}
}

func accountHCL(spaceSlug, nodeID, name, secret string, version int) string {
	return fmt.Sprintf(`
resource "spaceman_space" "s" {
  slug             = %q
  name             = "Accounts"
  node_id          = %q
  purge_on_destroy = true
}

resource "spaceman_account" "a" {
  name              = %q
  kind              = "anthropic_api_key"
  description       = "managed by terraform"
  metadata          = { team = "core" }
  secret_wo         = %q
  secret_wo_version = %d
}

resource "spaceman_space_accounts" "b" {
  space_id = spaceman_space.s.id
  binding = [{
    account_id = spaceman_account.a.id
    default    = true
  }]
}
`, spaceSlug, nodeID, name, secret, version)
}

// TestAccAccount covers the write-only secret: never in state, rotated only when its version changes.
func TestAccAccount(t *testing.T) {
	testAccPreCheck(t)
	slug, name := "tf-acct-"+suffix(), "tf-acct-"+suffix()
	s1, s2 := "sk-test-"+randHex(16), "sk-test-"+randHex(16)
	accSteps(t,
		resource.TestStep{
			Config: accountHCL(slug, acc.nodeA.ID, name, s1, 1),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_account.a", "secret_version", "1"),
				resource.TestCheckResourceAttr("spaceman_account.a", "harness", "claude-code"),
				resource.TestCheckNoResourceAttr("spaceman_account.a", "secret_wo"),
				resource.TestCheckResourceAttr("spaceman_space_accounts.b", "binding.#", "1"),
				resource.TestCheckResourceAttr("spaceman_space_accounts.b", "binding.0.default", "true"),
			),
		},
		// A new secret without a new version is not sent: Terraform cannot see write-only values.
		resource.TestStep{Config: accountHCL(slug, acc.nodeA.ID, name, s2, 1), PlanOnly: true},
		resource.TestStep{
			Config: accountHCL(slug, acc.nodeA.ID, name, s2, 2),
			Check:  resource.TestCheckResourceAttr("spaceman_account.a", "secret_version", "2"),
		},
		resource.TestStep{
			ResourceName: "spaceman_account.a", ImportState: true, ImportStateVerify: true,
			ImportStateVerifyIgnore: []string{"secret_wo", "secret_wo_version"},
		},
		resource.TestStep{
			ResourceName: "spaceman_space_accounts.b", ImportState: true, ImportStateVerify: true,
		},
		// A binding removed elsewhere is drift for the authoritative binding set.
		resource.TestStep{
			PreConfig: func() {
				ctx := context.Background()
				for _, sp := range mustSpaces(t) {
					if sp.Slug == slug {
						if _, err := acc.client.SetSpaceAccounts(ctx, sp.ID, nil); err != nil {
							t.Fatal(err)
						}
					}
				}
			},
			Config: accountHCL(slug, acc.nodeA.ID, name, s2, 2), PlanOnly: true, ExpectNonEmptyPlan: true,
		},
		resource.TestStep{Config: accountHCL(slug, acc.nodeA.ID, name, s2, 2),
			Check: resource.TestCheckResourceAttr("spaceman_space_accounts.b", "binding.#", "1")},
	)
}

func mustSpaces(t *testing.T) []*client.Space {
	t.Helper()
	l, err := acc.client.ListSpaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func catalogHCL(sfx, spaceSlug, nodeID, body, model string) string {
	return fmt.Sprintf(`
resource "spaceman_space" "s" {
  slug             = %[2]q
  name             = "Catalog"
  node_id          = %[3]q
  purge_on_destroy = true
}

resource "spaceman_harness_profile" "p" {
  slug     = "tf-prof-%[1]s"
  name     = "TF profile"
  harness  = "claude-code"
  model    = %[5]q
  settings = { permission_mode = "default" }
}

resource "spaceman_prompt" "base" {
  slug = "tf-base-%[1]s"
  name = "TF base"
  kind = "general"
  body = "Be careful."
}

resource "spaceman_prompt" "p" {
  slug    = "tf-prompt-%[1]s"
  name    = "TF prompt"
  kind    = "role"
  body    = %[4]q
  extends = spaceman_prompt.base.latest_version_id
  notes   = "from terraform"
}

resource "spaceman_skill" "k" {
  slug = "tf-skill-%[1]s"
  name = "TF skill"
  files = {
    "SKILL.md" = "---\nname: tf-skill-%[1]s\ndescription: A test skill.\n---\n\nUse it with %[5]s.\n"
  }
}

resource "spaceman_role_template" "t" {
  slug        = "tf-tpl-%[1]s"
  name        = "TF worker"
  kind        = "worker"
  description = "Runs on %[5]s"
  profile_id  = spaceman_harness_profile.p.id
  prompt_id   = spaceman_prompt.p.id
  skills      = [{ skill_id = spaceman_skill.k.id }]
}

resource "spaceman_space_role" "r" {
  space_id     = spaceman_space.s.id
  name         = "implementor"
  template_id  = spaceman_role_template.t.id
  prompt_id    = spaceman_prompt.p.id
  prompt_pin   = spaceman_prompt.p.latest_version_id
  instructions = "Work in small steps."
}
`, sfx, spaceSlug, nodeID, body, model)
}

// TestAccCatalog covers profiles, prompts (immutable published versions), skills, role templates and space roles: a
// content change publishes one new version and an unchanged configuration never plans again.
func TestAccCatalog(t *testing.T) {
	testAccPreCheck(t)
	sfx, slug := suffix(), "tf-cat-"+suffix()
	var promptID string
	accSteps(t,
		resource.TestStep{
			Config: catalogHCL(sfx, slug, acc.nodeA.ID, "Version one.", "claude-sonnet-4-5"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_prompt.p", "latest_version", "1"),
				resource.TestCheckResourceAttrPair("spaceman_prompt.p", "extends", "spaceman_prompt.base", "latest_version_id"),
				resource.TestCheckResourceAttr("spaceman_skill.k", "latest_version", "1"),
				resource.TestCheckResourceAttr("spaceman_role_template.t", "skills.#", "1"),
				resource.TestCheckResourceAttrPair("spaceman_space_role.r", "prompt_pin", "spaceman_prompt.p", "latest_version_id"),
				func(s *terraform.State) error { promptID = stateAttr(s, "spaceman_prompt.p", "id"); return nil },
			),
		},
		// No perpetual diff: the same configuration plans nothing.
		resource.TestStep{Config: catalogHCL(sfx, slug, acc.nodeA.ID, "Version one.", "claude-sonnet-4-5"), PlanOnly: true},
		resource.TestStep{
			Config: catalogHCL(sfx, slug, acc.nodeA.ID, "Version two.", "claude-opus-4-1"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_prompt.p", "latest_version", "2"),
				resource.TestCheckResourceAttr("spaceman_prompt.p", "body", "Version two."),
				resource.TestCheckResourceAttr("spaceman_harness_profile.p", "revision", "2"),
				resource.TestCheckResourceAttr("spaceman_skill.k", "latest_version", "2"),
				resource.TestCheckResourceAttr("spaceman_role_template.t", "description", "Runs on claude-opus-4-1"),
				resource.TestCheckResourceAttr("spaceman_role_template.t", "revision", "2"),
				resource.TestCheckResourceAttrPair("spaceman_space_role.r", "prompt_pin", "spaceman_prompt.p", "latest_version_id"),
			),
		},
		resource.TestStep{ResourceName: "spaceman_prompt.p", ImportState: true, ImportStateVerify: true},
		resource.TestStep{ResourceName: "spaceman_skill.k", ImportState: true, ImportStateVerify: true},
		resource.TestStep{ResourceName: "spaceman_harness_profile.p", ImportState: true, ImportStateVerify: true},
		resource.TestStep{ResourceName: "spaceman_role_template.t", ImportState: true, ImportStateVerify: true},
		resource.TestStep{ResourceName: "spaceman_space_role.r", ImportState: true, ImportStateVerify: true},
		// A version published outside Terraform is drift; an unpublished draft is not.
		resource.TestStep{
			PreConfig: func() {
				ctx := context.Background()
				if _, err := acc.client.SavePromptDraft(ctx, promptID, 0, client.PromptDraftInput{Body: "A draft someone started."}); err != nil {
					t.Fatal(err)
				}
			},
			Config: catalogHCL(sfx, slug, acc.nodeA.ID, "Version two.", "claude-opus-4-1"), PlanOnly: true,
		},
		resource.TestStep{
			PreConfig: func() {
				ctx := context.Background()
				p, err := acc.client.GetPrompt(ctx, promptID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := acc.client.PublishPrompt(ctx, promptID, p.Draft().Version); err != nil {
					t.Fatal(err)
				}
			},
			Config: catalogHCL(sfx, slug, acc.nodeA.ID, "Version two.", "claude-opus-4-1"), PlanOnly: true, ExpectNonEmptyPlan: true,
		},
		resource.TestStep{
			Config: catalogHCL(sfx, slug, acc.nodeA.ID, "Version two.", "claude-opus-4-1"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_prompt.p", "latest_version", "4"),
				resource.TestCheckResourceAttr("spaceman_prompt.p", "body", "Version two."),
			),
		},
		// The API cannot rename a prompt: the change is refused at plan time instead of replacing it.
		resource.TestStep{
			Config: strings.Replace(catalogHCL(sfx, slug, acc.nodeA.ID, "Version two.", "claude-opus-4-1"),
				`name    = "TF prompt"`, `name    = "TF prompt renamed"`, 1),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`A prompt's name cannot be changed`),
		},
	)
}

func mcpHCL(sfx, spaceSlug, nodeID, token string, version int, tools string) string {
	return fmt.Sprintf(`
resource "spaceman_space" "s" {
  slug             = %[2]q
  name             = "MCP"
  node_id          = %[3]q
  purge_on_destroy = true
}

resource "spaceman_mcp_server" "m" {
  slug               = "tf%[1]s"
  name               = "Tracker"
  url                = "http://127.0.0.1:9/mcp"
  allow_private      = true
  tools              = %[6]s
  headers_wo         = { Authorization = "Bearer %[4]s" }
  headers_wo_version = %[5]d
}

resource "spaceman_mcp_grant" "g" {
  server_id = spaceman_mcp_server.m.id
  space_id  = spaceman_space.s.id
  role      = "implementor"
  tools     = ["list_tickets", "get_ticket"]
}
`, sfx, spaceSlug, nodeID, token, version, tools)
}

// TestAccMCP covers MCP servers with write-only headers and grants (replaced on change).
func TestAccMCP(t *testing.T) {
	testAccPreCheck(t)
	sfx, slug := suffix(), "tf-mcp-"+suffix()
	tok1, tok2 := randHex(16), randHex(16)
	accSteps(t,
		resource.TestStep{
			Config: mcpHCL(sfx, slug, acc.nodeA.ID, tok1, 1, `["list_tickets", "get_ticket"]`),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_mcp_server.m", "header_names.#", "1"),
				resource.TestCheckResourceAttr("spaceman_mcp_server.m", "header_names.0", "Authorization"),
				resource.TestCheckResourceAttr("spaceman_mcp_server.m", "headers_version", "1"),
				resource.TestCheckTypeSetElemAttr("spaceman_mcp_grant.g", "tools.*", "list_tickets"),
				resource.TestCheckResourceAttrSet("spaceman_mcp_server.m", "call_timeout_ms"),
			),
		},
		resource.TestStep{Config: mcpHCL(sfx, slug, acc.nodeA.ID, tok2, 1, `["list_tickets", "get_ticket"]`), PlanOnly: true},
		resource.TestStep{
			Config: mcpHCL(sfx, slug, acc.nodeA.ID, tok2, 2, `["list_tickets"]`),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_mcp_server.m", "headers_version", "2"),
				resource.TestCheckResourceAttr("spaceman_mcp_server.m", "tools.#", "1"),
			),
		},
		resource.TestStep{ResourceName: "spaceman_mcp_server.m", ImportState: true, ImportStateVerify: true,
			ImportStateVerifyIgnore: []string{"headers_wo", "headers_wo_version"}},
		resource.TestStep{ResourceName: "spaceman_mcp_grant.g", ImportState: true, ImportStateVerify: true,
			ImportStateIdFunc: func(s *terraform.State) (string, error) {
				return stateAttr(s, "spaceman_mcp_grant.g", "server_id") + "/" + stateAttr(s, "spaceman_mcp_grant.g", "id"), nil
			}},
	)
}

func eventsHCL(sfx, spaceSlug, nodeID, pulseURL, creds string, credsVersion int, title string) string {
	return fmt.Sprintf(`
resource "spaceman_space" "s" {
  slug             = %[2]q
  name             = "Events"
  node_id          = %[3]q
  purge_on_destroy = true
}

resource "spaceman_event_connection" "c" {
  name             = "tf-pulse-%[1]s"
  url              = %[4]q
  creds_wo         = %[5]q
  creds_wo_version = %[6]d
}

resource "spaceman_event_template" "t" {
  slug = "tf-tmpl-%[1]s"
  name = "TF deploys"
  definition = jsonencode({
    subject   = "evt.deploy.{{vars.env}}.>"
    variables = [{ name = "env", required = true }]
    filters   = [{ path = "data.status", op = "eq", value = "failed" }]
    title     = %[7]q
    body      = "{{data.message}}"
    urgency   = "high"
  })
}

resource "spaceman_event_subscription" "sub" {
  space_id            = spaceman_space.s.id
  name                = "deploy failures"
  connection_id       = spaceman_event_connection.c.id
  template_version_id = spaceman_event_template.t.latest_version_id
  follow              = "latest"
  vars                = { env = "prod" }
}

data "spaceman_event_template" "gh" {
  slug = "github-issue"
}
`, sfx, spaceSlug, nodeID, pulseURL, creds, credsVersion, title)
}

// TestAccEvents covers a connection with write-only creds, a template publishing versions and a subscription that
// follows the latest version (the move is not drift).
func TestAccEvents(t *testing.T) {
	testAccPreCheck(t)
	sfx, slug := suffix(), "tf-evt-"+suffix()
	var firstVersion string
	accSteps(t,
		resource.TestStep{
			Config: eventsHCL(sfx, slug, acc.nodeA.ID, acc.pulseURL, acc.creds, 1, "Deploy failed"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_event_connection.c", "client_id", "spaceman"),
				resource.TestCheckResourceAttr("spaceman_event_template.t", "latest_version", "1"),
				resource.TestCheckResourceAttr("spaceman_event_subscription.sub", "subject", "evt.deploy.prod.>"),
				resource.TestCheckResourceAttrSet("spaceman_event_subscription.sub", "durable"),
				resource.TestCheckResourceAttr("data.spaceman_event_template.gh", "builtin", "true"),
				resource.TestCheckResourceAttrSet("data.spaceman_event_template.gh", "latest_version_id"),
				func(s *terraform.State) error {
					firstVersion = stateAttr(s, "spaceman_event_template.t", "latest_version_id")
					return nil
				},
			),
		},
		resource.TestStep{Config: eventsHCL(sfx, slug, acc.nodeA.ID, acc.pulseURL, acc.creds2, 1, "Deploy failed"), PlanOnly: true},
		// Rotating the creds (another Pulse client) is visible through client_id.
		resource.TestStep{
			Config: eventsHCL(sfx, slug, acc.nodeA.ID, acc.pulseURL, acc.creds2, 2, "Deploy failed"),
			Check:  resource.TestCheckResourceAttr("spaceman_event_connection.c", "client_id", "pulse-github"),
		},
		// A new template version: the following subscription moves to it without planning a change of its own.
		resource.TestStep{
			Config: eventsHCL(sfx, slug, acc.nodeA.ID, acc.pulseURL, acc.creds2, 2, "Deploy failed again"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_event_template.t", "latest_version", "2"),
				apiCheck("spaceman_event_subscription.sub", func(ctx context.Context, c *client.Client, id string, s *terraform.State) error {
					sub, err := c.GetSubscription(ctx, stateAttr(s, "spaceman_event_subscription.sub", "space_id"), id)
					if err != nil {
						return err
					}
					if sub.Template == nil || sub.Template.VersionID != stateAttr(s, "spaceman_event_template.t", "latest_version_id") {
						return fmt.Errorf("subscription did not follow the new version: %+v", sub.Template)
					}
					return nil
				}),
			),
		},
		resource.TestStep{Config: eventsHCL(sfx, slug, acc.nodeA.ID, acc.pulseURL, acc.creds2, 2, "Deploy failed again"), PlanOnly: true},
		resource.TestStep{ResourceName: "spaceman_event_template.t", ImportState: true, ImportStateVerify: true},
		resource.TestStep{ResourceName: "spaceman_event_connection.c", ImportState: true, ImportStateVerify: true,
			ImportStateVerifyIgnore: []string{"creds_wo", "creds_wo_version"}},
		resource.TestStep{ResourceName: "spaceman_event_subscription.sub", ImportState: true, ImportStateVerify: true,
			// After import the stored version is the one SpaceMan moved to, not the configured starting version.
			ImportStateVerifyIgnore: []string{"template_version_id"},
			ImportStateIdFunc: func(s *terraform.State) (string, error) {
				return stateAttr(s, "spaceman_event_subscription.sub", "space_id") + "/" + stateAttr(s, "spaceman_event_subscription.sub", "id"), nil
			}},
	)
	if firstVersion == "" {
		t.Fatal("no first template version recorded")
	}
}

func humanHCL(sfx, spaceSlug, nodeID, label string, required string) string {
	return fmt.Sprintf(`
resource "spaceman_space" "s" {
  slug             = %[2]q
  name             = "Human"
  node_id          = %[3]q
  purge_on_destroy = true
}

resource "spaceman_human_action_type" "t" {
  slug = "tf-review-%[1]s"
  name = "Design review"
  request_schema = jsonencode({
    type       = "object"
    properties = { doc = { type = "string" } }
    required   = [%[5]s]
  })
  response_schema  = jsonencode({ type = "object", properties = { verdict = { type = "string" } } })
  outcomes         = ["approved", "changes_requested"]
  default_priority = "high"
}

resource "spaceman_space_human_action_type" "e" {
  space_id = spaceman_space.s.id
  type_id  = spaceman_human_action_type.t.id
  label    = %[4]q
}

resource "spaceman_approval_policy" "p" {
  space_id      = spaceman_space.s.id
  operation     = "mcp.tracker.*"
  uses          = 2
  valid_seconds = 600
  note          = "tracker writes need a person"
}
`, sfx, spaceSlug, nodeID, label, required)
}

// TestAccHumanActionTypes covers human-action types, space enablement and approval policies.
func TestAccHumanActionTypes(t *testing.T) {
	testAccPreCheck(t)
	sfx, slug := suffix(), "tf-hum-"+suffix()
	accSteps(t,
		resource.TestStep{
			Config: humanHCL(sfx, slug, acc.nodeA.ID, "Review", ""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_human_action_type.t", "latest_version", "1"),
				resource.TestCheckResourceAttr("spaceman_space_human_action_type.e", "version_number", "1"),
				resource.TestCheckResourceAttr("spaceman_space_human_action_type.e", "label", "Review"),
				resource.TestCheckResourceAttr("spaceman_approval_policy.p", "request_ttl_seconds", "86400"),
			),
		},
		resource.TestStep{Config: humanHCL(sfx, slug, acc.nodeA.ID, "Review", ""), PlanOnly: true},
		resource.TestStep{
			Config: humanHCL(sfx, slug, acc.nodeA.ID, "Design review", `"doc"`),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_human_action_type.t", "latest_version", "2"),
				resource.TestCheckResourceAttr("spaceman_space_human_action_type.e", "version_number", "2"),
				resource.TestCheckResourceAttr("spaceman_space_human_action_type.e", "label", "Design review"),
			),
		},
		resource.TestStep{Config: humanHCL(sfx, slug, acc.nodeA.ID, "Design review", `"doc"`), PlanOnly: true},
		resource.TestStep{ResourceName: "spaceman_human_action_type.t", ImportState: true, ImportStateVerify: true},
		resource.TestStep{ResourceName: "spaceman_approval_policy.p", ImportState: true, ImportStateVerify: true},
		resource.TestStep{ResourceName: "spaceman_space_human_action_type.e", ImportState: true, ImportStateVerify: true,
			ImportStateVerifyIgnore: []string{"label"},
			ImportStateIdFunc: func(s *terraform.State) (string, error) {
				return stateAttr(s, "spaceman_space_human_action_type.e", "space_id") + "/" + stateAttr(s, "spaceman_space_human_action_type.e", "type_id"), nil
			}},
	)
}

func kbHCL(sfx, spaceSlug, nodeID string, maxSessions int) string {
	return fmt.Sprintf(`
resource "spaceman_space" "s" {
  slug    = %[2]q
  name    = "Knowledge"
  node_id = %[3]q
}

resource "spaceman_role_template" "kb" {
  slug = "tf-kb-%[1]s"
  name = "KB keeper"
  kind = "kb"
}

resource "spaceman_role_template" "dreamer" {
  slug = "tf-dreamer-%[1]s"
  name = "Dreamer"
  kind = "dreamer"
}

resource "spaceman_space_role" "kb" {
  space_id    = spaceman_space.s.id
  name        = "kb"
  template_id = spaceman_role_template.kb.id
}

resource "spaceman_space_role" "dreamer" {
  space_id    = spaceman_space.s.id
  name        = "dreamer"
  template_id = spaceman_role_template.dreamer.id
}

resource "spaceman_knowledge_base" "k" {
  space_id     = spaceman_space.s.id
  slug         = "docs"
  name         = "Docs"
  repo         = "kb/docs"
  space_role   = spaceman_space_role.kb.name
  validator    = ["python3", "tools/kb.py", "check"]
  max_sessions = %[4]d
}

resource "spaceman_dream" "d" {
  space_id       = spaceman_space.s.id
  slug           = "nightly"
  name           = "Nightly"
  space_role     = spaceman_space_role.dreamer.name
  interval_hours = %[4]d * 12
}
`, sfx, spaceSlug, nodeID, maxSessions)
}

// TestAccKnowledgeAndDreams covers knowledge bases and dream configurations (destroy disables them).
func TestAccKnowledgeAndDreams(t *testing.T) {
	testAccPreCheck(t)
	sfx, slug := suffix(), "tf-kb-"+suffix()
	var spaceID, kbID, dreamID string
	// SpaceMan cannot delete knowledge bases or dreams, nor a space without archiving it: destroy disables and archives.
	checkDestroy := func(*terraform.State) error {
		ctx := context.Background()
		sp, err := acc.client.GetSpace(ctx, spaceID)
		if err != nil {
			return err
		}
		if sp.State != "archived" {
			return fmt.Errorf("space state after destroy = %s, want archived", sp.State)
		}
		k, err := acc.client.GetKB(ctx, spaceID, kbID)
		if err != nil {
			return err
		}
		d, err := acc.client.GetDream(ctx, spaceID, dreamID)
		if err != nil {
			return err
		}
		if k.Enabled || d.Enabled {
			return fmt.Errorf("after destroy: kb enabled=%v, dream enabled=%v; want both disabled", k.Enabled, d.Enabled)
		}
		return nil
	}
	accCase(t, checkDestroy,
		resource.TestStep{
			Config: kbHCL(sfx, slug, acc.nodeA.ID, 2),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_knowledge_base.k", "base_branch", "main"),
				resource.TestCheckResourceAttr("spaceman_knowledge_base.k", "write_policy", "review"),
				resource.TestCheckResourceAttr("spaceman_knowledge_base.k", "enabled", "true"),
				resource.TestCheckResourceAttr("spaceman_dream.d", "max_minutes", "30"),
				resource.TestCheckResourceAttr("spaceman_dream.d", "memory_roles.0", "*"),
				func(s *terraform.State) error {
					spaceID, kbID, dreamID = stateAttr(s, "spaceman_space.s", "id"), stateAttr(s, "spaceman_knowledge_base.k", "id"),
						stateAttr(s, "spaceman_dream.d", "id")
					return nil
				},
			),
		},
		resource.TestStep{
			Config: kbHCL(sfx, slug, acc.nodeA.ID, 4),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_knowledge_base.k", "max_sessions", "4"),
				resource.TestCheckResourceAttr("spaceman_dream.d", "interval_hours", "48"),
			),
		},
		resource.TestStep{Config: kbHCL(sfx, slug, acc.nodeA.ID, 4), PlanOnly: true},
		resource.TestStep{ResourceName: "spaceman_knowledge_base.k", ImportState: true, ImportStateVerify: true,
			ImportStateIdFunc: func(s *terraform.State) (string, error) {
				return stateAttr(s, "spaceman_knowledge_base.k", "space_id") + "/" + stateAttr(s, "spaceman_knowledge_base.k", "id"), nil
			}},
		resource.TestStep{ResourceName: "spaceman_dream.d", ImportState: true, ImportStateVerify: true,
			ImportStateIdFunc: func(s *terraform.State) (string, error) {
				return stateAttr(s, "spaceman_dream.d", "space_id") + "/" + stateAttr(s, "spaceman_dream.d", "id"), nil
			}},
	)
}

func oidcHCL(sfx, secret string, version int, display string) string {
	return fmt.Sprintf(`
resource "spaceman_oidc_provider" "p" {
  slug                     = "tf%[1]s"
  display_name             = %[4]q
  issuer                   = "https://idp.example.com/realms/tf"
  client_id                = "spaceman-tf"
  client_secret_wo         = %[2]q
  client_secret_wo_version = %[3]d
  groups_claim             = "groups"
  provisioning             = "jit"
  jit = {
    email_domains = ["example.com"]
  }
}

resource "spaceman_oidc_group_mapping" "m" {
  provider_id = spaceman_oidc_provider.p.id
  group_value = "spaceman-admins"
  role        = "admin"
  domain      = "*"
}
`, sfx, secret, version, display)
}

// TestAccOIDC covers sign-in providers (write-only client secret) and group mappings.
func TestAccOIDC(t *testing.T) {
	testAccPreCheck(t)
	sfx := suffix()
	accSteps(t,
		resource.TestStep{
			Config: oidcHCL(sfx, randHex(16), 1, "Example SSO"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_oidc_provider.p", "has_client_secret", "true"),
				resource.TestCheckResourceAttr("spaceman_oidc_provider.p", "scopes.#", "3"),
				resource.TestCheckResourceAttr("spaceman_oidc_provider.p", "jit.email_domains.0", "example.com"),
				resource.TestCheckResourceAttr("spaceman_oidc_group_mapping.m", "role", "admin"),
			),
		},
		resource.TestStep{
			Config: oidcHCL(sfx, "", 2, "Example SSO renamed"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_oidc_provider.p", "has_client_secret", "false"),
				resource.TestCheckResourceAttr("spaceman_oidc_provider.p", "display_name", "Example SSO renamed"),
			),
		},
		resource.TestStep{Config: oidcHCL(sfx, "", 2, "Example SSO renamed"), PlanOnly: true},
		resource.TestStep{ResourceName: "spaceman_oidc_provider.p", ImportState: true, ImportStateVerify: true,
			ImportStateVerifyIgnore: []string{"client_secret_wo", "client_secret_wo_version"}},
		resource.TestStep{ResourceName: "spaceman_oidc_group_mapping.m", ImportState: true, ImportStateVerify: true,
			ImportStateIdFunc: func(s *terraform.State) (string, error) {
				return stateAttr(s, "spaceman_oidc_group_mapping.m", "provider_id") + "/" + stateAttr(s, "spaceman_oidc_group_mapping.m", "id"), nil
			}},
	)
}

// TestAccDataSources covers the node and space data sources.
func TestAccDataSources(t *testing.T) {
	testAccPreCheck(t)
	slug := "tf-ds-" + suffix()
	accSteps(t, resource.TestStep{
		Config: fmt.Sprintf(`
resource "spaceman_space" "s" {
  slug             = %q
  name             = "Data"
  node_id          = %q
  purge_on_destroy = true
}

data "spaceman_node" "a" {
  name = %q
}

data "spaceman_nodes" "all" {}

data "spaceman_space" "s" {
  slug       = spaceman_space.s.slug
  depends_on = [spaceman_space.s]
}

data "spaceman_spaces" "all" {
  depends_on = [spaceman_space.s]
}
`, slug, acc.nodeA.ID, acc.nodeA.Name),
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("data.spaceman_node.a", "id", acc.nodeA.ID),
			resource.TestCheckResourceAttr("data.spaceman_node.a", "state", "pending"),
			func(s *terraform.State) error {
				n, _ := strconv.Atoi(stateAttr(s, "data.spaceman_nodes.all", "nodes.#"))
				if n < 2 {
					return fmt.Errorf("nodes.# = %d", n)
				}
				return nil
			},
			resource.TestCheckResourceAttrPair("data.spaceman_space.s", "id", "spaceman_space.s", "id"),
			resource.TestCheckResourceAttr("data.spaceman_space.s", "node_id", acc.nodeA.ID),
			resource.TestCheckResourceAttrSet("data.spaceman_spaces.all", "spaces.#"),
		),
	})
}
