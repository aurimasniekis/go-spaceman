# A user created by OIDC just-in-time provisioning.
data "spaceman_user" "grace" {
  email = "grace@example.com"
}

resource "spaceman_user_grants" "grace" {
  user_id = data.spaceman_user.grace.id
  grant   = [{ role = "operator", domain = spaceman_space.payments.id }]
}
