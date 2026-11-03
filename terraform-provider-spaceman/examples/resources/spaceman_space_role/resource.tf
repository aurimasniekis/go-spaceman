resource "spaceman_space_role" "implementor" {
  space_id     = spaceman_space.payments.id
  name         = "implementor-backend"
  template_id  = spaceman_role_template.worker.id
  model        = "claude-sonnet-4-5"
  instructions = "Work in small, reviewed steps."
  skills = [
    { skill_id = spaceman_skill.release_notes.id },
  ]
}
