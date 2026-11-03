# A space with its roles, account, skills, MCP tools, events and human-action types, managed together.
# Secrets come from ephemeral variables and are sent as write-only arguments: they never reach a plan or the state.

terraform {
  required_version = ">= 1.11"
  required_providers {
    spaceman = {
      source = "aurimasniekis/spaceman"
    }
  }
}

provider "spaceman" {} # SPACEMAN_ENDPOINT and SPACEMAN_TOKEN

variable "node_name" {
  type    = string
  default = "build-01"
}

variable "anthropic_api_key" {
  type      = string
  sensitive = true
  ephemeral = true
}

variable "anthropic_api_key_version" {
  type        = number
  default     = 1
  description = "Bump after changing anthropic_api_key to rotate it in SpaceMan."
}

variable "tracker_token" {
  type      = string
  sensitive = true
  ephemeral = true
}

variable "pulse_creds" {
  type      = string
  sensitive = true
  ephemeral = true
}

data "spaceman_node" "build" {
  name = var.node_name
}

resource "spaceman_space" "payments" {
  slug    = "payments"
  name    = "Payments"
  goals   = "Keep the payment API green."
  node_id = data.spaceman_node.build.id
}

# ---- catalog ----

resource "spaceman_harness_profile" "careful" {
  slug     = "careful"
  name     = "Careful Claude Code"
  harness  = "claude-code"
  model    = "claude-sonnet-4-5"
  settings = { permission_mode = "default" }
}

resource "spaceman_prompt" "worker" {
  slug = "backend-worker"
  name = "Backend worker"
  kind = "role"
  body = <<-EOT
    You implement backend changes for the payments service.
    Keep each change small and covered by tests.
  EOT
}

resource "spaceman_skill" "release_notes" {
  slug = "release-notes"
  name = "Release notes"
  files = {
    "SKILL.md" = <<-EOT
      ---
      name: release-notes
      description: Write release notes from merged pull requests.
      ---

      Group changes by Added, Changed and Fixed.
    EOT
  }
}

resource "spaceman_role_template" "worker" {
  slug       = "backend-worker"
  name       = "Backend worker"
  kind       = "worker"
  profile_id = spaceman_harness_profile.careful.id
  prompt_id  = spaceman_prompt.worker.id
  skills     = [{ skill_id = spaceman_skill.release_notes.id }]
}

resource "spaceman_space_role" "implementor" {
  space_id    = spaceman_space.payments.id
  name        = "implementor-backend"
  template_id = spaceman_role_template.worker.id
}

# ---- credentials ----

resource "spaceman_account" "claude" {
  name              = "payments-claude"
  kind              = "anthropic_api_key"
  secret_wo         = var.anthropic_api_key
  secret_wo_version = var.anthropic_api_key_version
}

resource "spaceman_space_accounts" "payments" {
  space_id = spaceman_space.payments.id
  binding  = [{ account_id = spaceman_account.claude.id, default = true }]
}

# ---- MCP gateway ----

resource "spaceman_mcp_server" "tracker" {
  slug               = "tracker"
  name               = "Ticket tracker"
  url                = "https://tracker.example.com/mcp"
  headers_wo         = { Authorization = "Bearer ${var.tracker_token}" }
  headers_wo_version = 1
}

resource "spaceman_mcp_grant" "tracker" {
  server_id = spaceman_mcp_server.tracker.id
  space_id  = spaceman_space.payments.id
  role      = spaceman_space_role.implementor.name
  tools     = ["list_tickets", "get_ticket"]
}

resource "spaceman_approval_policy" "tracker_writes" {
  space_id  = spaceman_space.payments.id
  operation = "mcp.tracker.*"
  note      = "Tracker tools need a person's approval."
}

# ---- events ----

resource "spaceman_event_connection" "pulse" {
  name             = "pulse-prod"
  url              = "tls://pulse.example.com:4222"
  creds_wo         = var.pulse_creds
  creds_wo_version = 1
}

data "spaceman_event_template" "github_issues" {
  slug = "github-issue"
}

resource "spaceman_event_subscription" "issues" {
  space_id            = spaceman_space.payments.id
  name                = "payments issues"
  connection_id       = spaceman_event_connection.pulse.id
  template_version_id = data.spaceman_event_template.github_issues.latest_version_id
  follow              = "latest"
  vars                = { account_key = "acme", repository = "acme/payments" }
}

# ---- human actions ----

resource "spaceman_human_action_type" "design_review" {
  slug = "design-review"
  name = "Design review"
  request_schema = jsonencode({
    type       = "object"
    properties = { doc = { type = "string" } }
    required   = ["doc"]
  })
  outcomes = ["approved", "changes_requested"]
}

resource "spaceman_space_human_action_type" "design_review" {
  space_id = spaceman_space.payments.id
  type_id  = spaceman_human_action_type.design_review.id
}
