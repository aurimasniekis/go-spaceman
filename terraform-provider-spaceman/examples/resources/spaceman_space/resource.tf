data "spaceman_node" "build" {
  name = "build-01"
}

resource "spaceman_space" "payments" {
  slug                  = "payments"
  name                  = "Payments"
  description           = "The payments service and its infrastructure."
  goals                 = "Keep the payment API green; ship the refunds epic."
  standing_instructions = "Never push to main; open a pull request."
  node_id               = data.spaceman_node.build.id
  disconnect_policy     = "interrupt"
}
