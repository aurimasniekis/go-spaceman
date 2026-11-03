resource "spaceman_space_limits" "payments" {
  space_id = spaceman_space.payments.id

  # Only these limits are Terraform's; overrides of other limits set in the UI are kept.
  max_concurrent_runs    = 4
  max_tool_calls_per_run = 2000
  task_deadline_minutes  = 240
  budget_tokens_24h      = 20000000
  budget_warn_percent    = 75
  on_lower               = "drain"
}
