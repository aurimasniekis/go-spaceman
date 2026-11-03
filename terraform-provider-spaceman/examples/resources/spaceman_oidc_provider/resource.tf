variable "porch_client_secret" {
  type      = string
  sensitive = true
  ephemeral = true
}

resource "spaceman_oidc_provider" "porch" {
  slug         = "porch"
  display_name = "Porch"
  issuer       = "https://porch.example.com"
  client_id    = "spaceman"
  groups_claim = "porch.groups"
  provisioning = "jit"
  jit          = { groups = ["spaceman-users"] }
  enabled      = true

  client_secret_wo         = var.porch_client_secret
  client_secret_wo_version = 1
}
