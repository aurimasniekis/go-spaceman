resource "spaceman_skill" "release_notes" {
  slug = "release-notes"
  name = "Release notes"
  files = {
    "SKILL.md" = <<-EOT
      ---
      name: release-notes
      description: Write release notes from merged pull requests.
      ---

      Group changes by Added, Changed and Fixed.
    EOT
  }
}
