resource "spaceman_approval_policy" "tracker_writes" {
  space_id      = spaceman_space.payments.id
  operation     = "mcp.tracker.comment"
  uses          = 1
  valid_seconds = 900
  note          = "Comments on tickets need a person's approval."
}
