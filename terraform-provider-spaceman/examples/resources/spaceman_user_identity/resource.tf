# Ada signs in through Porch; the subject is her Porch user ID.
resource "spaceman_user_identity" "ada_porch" {
  user_id     = spaceman_user.ada.id
  provider_id = spaceman_oidc_provider.porch.id
  subject     = "u_01j0000000000000000000000"
}
