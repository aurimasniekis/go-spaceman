# Payments accepts questions and knowledge requests from Platform ...
resource "spaceman_xspace_peer" "payments_from_platform" {
  space_id = spaceman_space.payments.id
  peer_id  = spaceman_space.platform.id
  inbound = {
    discover        = true
    questions       = true
    knowledge       = true
    knowledge_bases = ["docs"]
    disclosure      = "summary"
    max_per_hour    = 10
  }
}

# ... and Platform's control may send them (both sides must agree).
resource "spaceman_xspace_peer" "platform_to_payments" {
  space_id = spaceman_space.platform.id
  peer_id  = spaceman_space.payments.id
  outbound = {
    questions = true
    knowledge = true
  }
}
