resource "spaceman_space_human_action_type" "review" {
  space_id = spaceman_space.payments.id
  type_id  = spaceman_human_action_type.design_review.id
  label    = "Payments design review"
}
