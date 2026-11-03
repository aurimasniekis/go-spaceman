data "spaceman_event_template" "github_issues" {
  slug = "github-issue"
}

resource "spaceman_event_subscription" "issues" {
  space_id            = spaceman_space.payments.id
  name                = "payments issues"
  connection_id       = spaceman_event_connection.pulse.id
  template_version_id = data.spaceman_event_template.github_issues.latest_version_id
  follow              = "latest"
  vars                = { account_key = "acme", repository = "acme/payments" }
}
