variable "anthropic_api_key" {
  type      = string
  sensitive = true
  ephemeral = true # never stored in a plan or state
}

resource "spaceman_account" "claude" {
  name        = "claude-team"
  kind        = "anthropic_api_key"
  description = "Team key for Claude Code runs"
  metadata    = { owner = "platform" }

  secret_wo         = var.anthropic_api_key
  secret_wo_version = 1 # bump to rotate
}
