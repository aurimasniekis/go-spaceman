resource "spaceman_oidc_group_mapping" "admins" {
  provider_id = spaceman_oidc_provider.porch.id
  group_value = "spaceman-admins"
  role        = "admin"
  domain      = "*"
}
