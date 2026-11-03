resource "spaceman_harness_profile" "careful" {
  slug     = "careful"
  name     = "Careful Claude Code"
  harness  = "claude-code"
  model    = "claude-sonnet-4-5"
  settings = { permission_mode = "default" }
}
