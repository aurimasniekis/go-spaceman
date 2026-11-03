resource "spaceman_knowledge_base" "docs" {
  space_id     = spaceman_space.payments.id
  slug         = "docs"
  name         = "Payments docs"
  repo         = "kb/payments-docs"
  space_role   = spaceman_space_role.kb.name # a role whose template is of kind kb
  validator    = ["python3", "tools/kb.py", "check"]
  write_policy = "review"
}
