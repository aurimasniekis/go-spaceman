resource "spaceman_user_grants" "ada" {
  user_id = spaceman_user.ada.id
  grant = [
    { role = "viewer", domain = "*" },
    { role = "operator", domain = spaceman_space.payments.id },
  ]
}
