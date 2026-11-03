# Installation default for approvals.
resource "spaceman_human_action_policy" "approvals" {
  action_type            = "approval"
  remind_after_minutes   = 30
  remind_every_minutes   = 120
  max_reminders          = 5
  escalate_after_minutes = 240
  escalate_priority      = "urgent"
}

# A space override for questions: keep the answer for the asking agent's next run.
resource "spaceman_human_action_policy" "payments_questions" {
  space_id          = spaceman_space.payments.id
  action_type       = "question"
  requester_ended   = "next_launch"
  hold_minutes      = 120
  successor_role    = "implementor"
  rate_per_run_hour = 10
}
