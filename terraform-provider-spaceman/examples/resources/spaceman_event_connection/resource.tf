variable "pulse_creds" {
  type      = string
  sensitive = true
  ephemeral = true
}

resource "spaceman_event_connection" "pulse" {
  name = "pulse-prod"
  url  = "tls://pulse.example.com:4222"

  creds_wo         = var.pulse_creds
  creds_wo_version = 1
}
