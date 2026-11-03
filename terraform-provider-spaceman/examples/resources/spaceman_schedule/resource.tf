# An inbox item every weekday morning in Vilnius.
resource "spaceman_schedule" "triage" {
  space_id = spaceman_space.payments.id
  name     = "morning-triage"
  kind     = "cron"
  cron     = "0 9 * * 1-5"
  timezone = "Europe/Vilnius"
  target   = "inbox"
  title    = "Triage open pull requests"
  body     = "Look at the pull requests opened since yesterday and plan the day."
  priority = "high"
}

# A ready task every six hours, counted from a fixed anchor.
resource "spaceman_schedule" "dependency_audit" {
  space_id         = spaceman_space.payments.id
  name             = "dependency-audit"
  kind             = "interval"
  interval_seconds = 21600
  anchor           = "2026-01-01T00:00:00Z"
  missed           = "run_once"
  target           = "task"
  title            = "Audit dependencies"
  assignee_role    = "implementor"
}
