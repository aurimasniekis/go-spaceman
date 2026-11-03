resource "spaceman_human_action_type" "design_review" {
  slug = "design-review"
  name = "Design review"
  request_schema = jsonencode({
    type       = "object"
    properties = { doc = { type = "string", title = "Design document" } }
    required   = ["doc"]
  })
  response_schema  = jsonencode({ type = "object", properties = { notes = { type = "string" } } })
  outcomes         = ["approved", "changes_requested"]
  default_priority = "high"
}
