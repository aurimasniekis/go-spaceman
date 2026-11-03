resource "spaceman_prompt" "worker" {
  slug  = "backend-worker"
  name  = "Backend worker"
  kind  = "role"
  body  = file("${path.module}/prompts/backend-worker.md")
  notes = "Managed by Terraform"
}

# Pin a role to exactly this version instead of following the latest:
#   prompt_pin = spaceman_prompt.worker.latest_version_id
