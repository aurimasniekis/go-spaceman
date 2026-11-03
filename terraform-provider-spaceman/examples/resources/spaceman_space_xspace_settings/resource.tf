resource "spaceman_space_xspace_settings" "payments" {
  space_id              = spaceman_space.payments.id
  profile               = "Owns the payment API and the refunds flow. Ask about charge states and refunds."
  max_depth             = 2
  max_outgoing_per_hour = 30
}
