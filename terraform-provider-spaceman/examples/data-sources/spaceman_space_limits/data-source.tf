data "spaceman_space_limits" "payments" {
  space_id = spaceman_space.payments.id
}

output "payments_concurrency" {
  value = data.spaceman_space_limits.payments.values["max_concurrent_runs"]
}
