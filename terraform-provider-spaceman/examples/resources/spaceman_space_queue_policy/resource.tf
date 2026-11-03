resource "spaceman_space_queue_policy" "payments" {
  space_id            = spaceman_space.payments.id
  max_top_level_tasks = 2
  incident_insertion  = "top"
  incident_preempts   = true
}
