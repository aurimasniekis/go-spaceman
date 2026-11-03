resource "spaceman_mcp_grant" "tracker_read" {
  server_id = spaceman_mcp_server.tracker.id
  space_id  = spaceman_space.payments.id
  role      = "implementor-backend"
  tools     = ["list_tickets", "get_ticket"]
}
