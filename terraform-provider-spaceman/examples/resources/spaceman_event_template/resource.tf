resource "spaceman_event_template" "deploy_failed" {
  slug = "deploy-failed"
  name = "Failed deploys"
  definition = jsonencode({
    subject   = "evt.deploy.{{vars.env}}.>"
    variables = [{ name = "env", required = true, example = "prod" }]
    filters   = [{ path = "data.status", op = "eq", value = "failed" }]
    title     = "Deploy to {{vars.env}} failed"
    body      = "{{data.message}}"
    urgency   = "high"
  })
}
