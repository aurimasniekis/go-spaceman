resource "spaceman_dream" "nightly" {
  space_id       = spaceman_space.payments.id
  slug           = "nightly"
  name           = "Nightly consolidation"
  space_role     = spaceman_space_role.dreamer.name # a role whose template is of kind dreamer
  memory_roles   = ["implementor-backend"]
  interval_hours = 24
}
