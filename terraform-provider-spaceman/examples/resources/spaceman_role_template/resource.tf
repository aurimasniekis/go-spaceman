resource "spaceman_role_template" "worker" {
  slug       = "backend-worker"
  name       = "Backend worker"
  kind       = "worker"
  profile_id = spaceman_harness_profile.careful.id
  prompt_id  = spaceman_prompt.worker.id # follows the latest published version
  skills     = [{ skill_id = spaceman_skill.release_notes.id }]
}
