# Terraform

SpaceMan's durable configuration can be managed with Terraform. The provider lives in this repository
(`terraform-provider-spaceman/`, a nested Go module) and talks to the same administrative API as the web UI. It covers
everything the API supports today and is not published to a registry yet.

## Setup

Build the provider and point Terraform at it with a development override:

```sh
cd terraform-provider-spaceman
go build -o "$HOME/.terraform.d/spaceman-dev/terraform-provider-spaceman" .
cat > "$HOME/.terraformrc-spaceman" <<EOF
provider_installation {
  dev_overrides {
    "aurimasniekis/spaceman" = "$HOME/.terraform.d/spaceman-dev"
  }
  direct {}
}
EOF
export TF_CLI_CONFIG_FILE="$HOME/.terraformrc-spaceman"
```

Terraform 1.11 or later is required: credentials are write-only arguments.

```hcl
terraform {
  required_providers {
    spaceman = { source = "aurimasniekis/spaceman" }
  }
}

provider "spaceman" {
  endpoint = "https://spaceman.example.com" # or SPACEMAN_ENDPOINT
  # token: SPACEMAN_TOKEN, an API token (spaceman_at_…)
}
```

Sign in as a dedicated automation user, create an API token on the **API tokens** page with the grants of what Terraform
manages (see [Sign-in and access](./access#api-tokens)) and pass it in `SPACEMAN_TOKEN`. Some operations (event connections,
OIDC providers, users and their grants, installation-wide human-action policies) need administrator grants. The bootstrap root token works too, but should stay a break-glass credential. Plain `http://`
endpoints are accepted only on loopback addresses unless `allow_insecure_http` is set, and the provider never follows a
redirect with the token.

## What it manages

| Area | Resources |
|---|---|
| Spaces | `spaceman_space`, `spaceman_space_role`, `spaceman_space_accounts` |
| Credentials | `spaceman_account` |
| Roles and prompts | `spaceman_harness_profile`, `spaceman_role_template`, `spaceman_prompt`, `spaceman_skill` |
| MCP gateway | `spaceman_mcp_server`, `spaceman_mcp_grant`, `spaceman_approval_policy` |
| Events | `spaceman_event_connection`, `spaceman_event_template`, `spaceman_event_subscription` |
| Human actions | `spaceman_human_action_type`, `spaceman_space_human_action_type`, `spaceman_human_action_policy` |
| Knowledge and memory | `spaceman_knowledge_base`, `spaceman_dream` |
| Limits and schedules | `spaceman_space_limits`, `spaceman_space_queue_policy`, `spaceman_schedule` |
| Cross-space | `spaceman_space_xspace_settings`, `spaceman_xspace_peer` |
| Users and sign-in | `spaceman_user`, `spaceman_user_grants`, `spaceman_user_identity`, `spaceman_oidc_provider`, `spaceman_oidc_group_mapping` |

Data sources read nodes (`spaceman_node`, `spaceman_nodes`), spaces (`spaceman_space`, `spaceman_spaces`), catalog
templates such as the built-in GitHub presets (`spaceman_event_template`), a space's effective limits
(`spaceman_space_limits`) and users (`spaceman_user`). Every resource can be imported.

Terraform never manages runtime state: runs, tasks, inbox items, messages, schedule occurrences, cross-space requests,
human-action requests and responses, approvals and recordings stay in SpaceMan. Nodes are enrolled with a one-time token
(see [Nodes and leases](./nodes)) and read with the node data sources. API tokens, password reset tokens and SSO link
invitations are shown once and are never handled by Terraform, and neither are passwords or MFA enrollment.

## Edits in the UI

The UI and Terraform use the same API, so an edit made in the UI to a field your configuration sets shows up as ordinary
drift in the next plan, and the next apply restores the configured value. Fields your configuration leaves out are not
owned by Terraform: whatever they hold (edited in the UI, or SpaceMan's default) is kept and never shows as drift. Removing
such a field from the configuration keeps its last value rather than clearing it.

Two kinds of resources work differently on purpose:

- **Limits.** `spaceman_space_limits` owns exactly the limits your configuration sets. Removing an attribute clears that
  override, so the installation default applies again; overrides of other limits set in the UI are kept and are not drift;
  destroying the resource clears only the overrides it owns. After an import it owns every stored override, so the next
  plan clears those your configuration does not set.
- **Grants.** A cross-space peer configuration (`spaceman_xspace_peer`) and a user's grants (`spaceman_user_grants`) are
  authorization, so Terraform owns them whole: a permission turned on in the UI is drift, and the next apply turns it off.

Every update carries the object version Terraform read during its refresh. If someone changes the object between the plan
and the apply, SpaceMan refuses the update with a conflict instead of overwriting it; plan again to see the change. The
queue policy and a user's grants have no such version in the API: the provider re-reads them just before writing and
refuses when they changed since the refresh.

## Secrets

Account secrets, MCP request headers, Pulse credentials and OIDC client secrets are write-only: Terraform sends them,
SpaceMan seals them, and neither the plan nor the state ever holds them. Because Terraform cannot see a write-only value
change, each one has a version number next to it:

```hcl
resource "spaceman_account" "claude" {
  name              = "claude-team"
  kind              = "anthropic_api_key"
  secret_wo         = var.anthropic_api_key # an ephemeral variable
  secret_wo_version = 2                     # bumped from 1: rotates the key
}
```

Runs that are already going keep the credential they started with; later launches use the new one.

## Versioned catalogs

Prompts, skills, event templates and human-action types keep immutable published versions. In Terraform their content
describes the latest published version: changing it publishes exactly one new version, and an unchanged configuration never
publishes again. A draft someone started in the UI is not drift (Terraform replaces it when it publishes, with a warning).
Roles pin a version with `prompt_pin = spaceman_prompt.worker.latest_version_id`, or follow the latest by leaving it out.
Event subscriptions with `follow = "latest"` move to new template versions on their own, and that move does not show as a
change.

## Limits, schedules and cross-space access

```hcl
resource "spaceman_space_limits" "payments" {
  space_id            = spaceman_space.payments.id
  max_concurrent_runs = 4
  budget_tokens_24h   = 20000000 # usage-based and reported late: never a hard limit
}

resource "spaceman_space_queue_policy" "payments" {
  space_id            = spaceman_space.payments.id
  max_top_level_tasks = 2
}

resource "spaceman_schedule" "triage" {
  space_id = spaceman_space.payments.id
  name     = "morning-triage"
  kind     = "cron"
  cron     = "0 9 * * 1-5"
  timezone = "Europe/Vilnius"
  target   = "inbox"
  title    = "Triage open pull requests"
}

# Platform may ask Payments questions: Payments accepts them, Platform's control may send them.
resource "spaceman_xspace_peer" "payments_from_platform" {
  space_id = spaceman_space.payments.id
  peer_id  = spaceman_space.platform.id
  inbound  = { questions = true, disclosure = "summary" }
}

resource "spaceman_xspace_peer" "platform_to_payments" {
  space_id = spaceman_space.platform.id
  peer_id  = spaceman_space.payments.id
  outbound = { questions = true }
}
```

The top-level task limit lives in the queue policy, not in `spaceman_space_limits`; the `spaceman_space_limits` data
source shows every effective value with its source. See [Limits](/guide/limits), [Schedules](/guide/schedules)
and [Cross-space requests](/guide/cross-space) for what each setting does.

## Users

```hcl
resource "spaceman_user" "ada" {
  email        = "ada@example.com" # lower case, as SpaceMan stores it
  display_name = "Ada Lovelace"
}

resource "spaceman_user_grants" "ada" {
  user_id = spaceman_user.ada.id
  grant   = [{ role = "operator", domain = spaceman_space.payments.id }]
}

resource "spaceman_user_identity" "ada_porch" {
  user_id     = spaceman_user.ada.id
  provider_id = spaceman_oidc_provider.porch.id
  subject     = "u_01j0000000000000000000000" # the provider's sub claim
}
```

The user then signs in through the provider, or sets a password with a reset token an administrator issues in the UI.
Grants from OIDC group mappings are separate and are not part of `spaceman_user_grants`. SpaceMan ends a user's sessions
when their grants change and always keeps one enabled global administrator.

## Moving a space

A space is pinned to its node. Changing `node_id` plans an in-place update; the apply runs SpaceMan's
[relocation](/operators/relocation) and waits for it. The space must be paused with no active runs, or set
`relocation_stop_runs = true` to pause it and stop its runs. Unpushed, changed or untracked state on the source is refused
by default: the apply fails with the findings, the relocation stays blocked in SpaceMan, and applying again with
`relocation_dirty_state = "export"` carries that state through central. The space stays paused on the new node.

```hcl
resource "spaceman_space" "payments" {
  slug    = "payments"
  name    = "Payments platform"
  node_id = data.spaceman_node.build_02.id # was build_01

  relocation_stop_runs   = true
  relocation_dirty_state = "refuse" # or "export"
  relocation_recovery    = "retain" # keep the old folder on the source
  relocation_retain_days = 14
  relocation_timeout     = "30m"
}
```

The unavailable-source flow (the source is offline or revoked, and its local state may be lost) needs an operator's
explicit acknowledgement and is not offered through Terraform; use the UI or the API.

## Things the provider will not do

- **Move a space by replacing it.** A `node_id` change is always a relocation, never a destroy and recreate.
- **Silently replace what cannot be deleted.** Prompts, skills, templates, types, profiles and role templates have no delete
  API, so their identifying fields (slug, kind, a prompt's name) are refused at plan time instead of being replaced.
  Destroying such a resource removes it from Terraform's state only, with a warning.
- **Overwrite what already exists.** Creating a cross-space peer configuration or a human-action policy that SpaceMan
  already has fails; import it instead.
- **Delete data on destroy by default.** Destroying a space archives it; set `purge_on_destroy = true` to purge its records
  too (upstream repositories are never touched). Knowledge bases, dream configurations and users are disabled on destroy
  (SpaceMan cannot delete a user, rename one or change their email: those changes are refused at plan time). A space's
  cross-space settings are reset to the defaults; its queue policy stays as it is.

## Not yet covered

Space repositories (no API yet), the backup target and schedule (set in the configuration file today), and editing a user's
email, display name or MFA requirement after creation.
