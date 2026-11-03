variable "tracker_token" {
  type      = string
  sensitive = true
  ephemeral = true
}

resource "spaceman_mcp_server" "tracker" {
  slug  = "tracker"
  name  = "Ticket tracker"
  url   = "https://tracker.example.com/mcp"
  tools = ["list_tickets", "get_ticket", "comment"]

  headers_wo         = { Authorization = "Bearer ${var.tracker_token}" }
  headers_wo_version = 1
}
