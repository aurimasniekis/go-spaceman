# terraform-provider-spaceman

The Terraform provider for [SpaceMan](../README.md): it manages a central's durable configuration through the
administrative API (`/api/v1`). It lives in this repository as a nested Go module
(`github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman`) built on the HashiCorp
[terraform-plugin-framework](https://developer.hashicorp.com/terraform/plugin/framework). Design decisions: ADR-0035 in
[`docs/DECISIONS.md`](../docs/DECISIONS.md).

Status: **phase 1** of work package M5-01. Not published to a registry.

## What it manages

| Resource | API |
|---|---|
| `spaceman_space` | `/spaces` (archive on destroy, purge opt-in; a `node_id` change relocates through `/spaces/{id}/relocation`) |
| `spaceman_space_role` | `/spaces/{id}/roles/{name}` |
| `spaceman_space_accounts` | `/spaces/{id}/accounts` (the whole binding set) |
| `spaceman_account` | `/accounts` (write-only `secret_wo`) |
| `spaceman_harness_profile` | `/harness-profiles` |
| `spaceman_role_template` | `/role-templates` |
| `spaceman_prompt` | `/prompts` (publishes immutable versions) |
| `spaceman_skill` | `/skills` (publishes immutable versions) |
| `spaceman_mcp_server` | `/mcp-servers` (write-only `headers_wo`) |
| `spaceman_mcp_grant` | `/mcp-servers/{id}/grants` |
| `spaceman_event_connection` | `/event-connections` (write-only `creds_wo`) |
| `spaceman_event_template` | `/event-templates`, `/event-template-versions/{id}/publish` |
| `spaceman_event_subscription` | `/spaces/{id}/subscriptions` |
| `spaceman_human_action_type` | `/human-types` (publishes immutable versions) |
| `spaceman_space_human_action_type` | `/spaces/{id}/human-types/{type}` |
| `spaceman_approval_policy` | `/approval-policies` |
| `spaceman_knowledge_base` | `/spaces/{id}/kbs` (disabled on destroy) |
| `spaceman_dream` | `/spaces/{id}/dreams` (disabled on destroy) |
| `spaceman_oidc_provider` | `/oidc/providers` (write-only `client_secret_wo`) |
| `spaceman_oidc_group_mapping` | `/oidc/providers/{id}/mappings` |

Data sources: `spaceman_node`, `spaceman_nodes`, `spaceman_space`, `spaceman_spaces`, `spaceman_event_template`.

Never managed: runtime state (runs, tasks, inbox items, messages, human-action requests and responses, approvals,
recordings), nodes (enrolled with a one-time token) and API tokens (their secret is shown once).

Not yet available (phase 2): limits and quotas, schedules and cross-space requests, which SpaceMan does not implement yet;
space repositories (no API yet); backup target and schedule (configuration file
only today); users and their role grants.

## Behaviour in brief

- **Ownership and drift (SM-API-005).** UI edits use the same API and show as drift for fields the configuration sets.
  Optional fields left out are not owned: their current value is kept and never shows as drift.
- **Optimistic concurrency.** Object versions are kept in private state and sent as `If-Match`; a change made after the
  plan makes the apply fail with a conflict instead of being overwritten.
- **Idempotent creates.** Creates send an `Idempotency-Key` where the API accepts one and retry safely on 502/503/504.
- **Write-only secrets.** Credentials are write-only arguments (Terraform 1.11+) with a `<name>_version` companion: bump it
  to rotate. They never reach a plan or the state.
- **Immutable versions.** Prompts, skills, event templates and human-action types publish one new version per content
  change and never re-publish an unchanged configuration.
- **No silent replacement.** Fields the API cannot change in place on objects it cannot delete (slugs, kinds, a prompt's
  name) are refused at plan time.
- **Relocation.** A change of a space's `node_id` is an in-place update that runs SpaceMan's safe relocation and waits for
  it (`relocation_dirty_state`, `relocation_stop_runs`, `relocation_recovery`, `relocation_retain_days`,
  `relocation_timeout`). Unpushed or uncommitted state is refused by default; the space stays paused on the new node.

The [registry documentation](docs/index.md) has the details of every resource.

## Using it locally

```sh
cd terraform-provider-spaceman
go build -o ~/.terraform.d/spaceman-dev/terraform-provider-spaceman .
cat > ~/.terraformrc-spaceman <<'EOF'
provider_installation {
  dev_overrides {
    "aurimasniekis/spaceman" = "/home/you/.terraform.d/spaceman-dev"
  }
  direct {}
}
EOF
export TF_CLI_CONFIG_FILE=~/.terraformrc-spaceman
export SPACEMAN_ENDPOINT=https://spaceman.example.com
export SPACEMAN_TOKEN=...   # an API token of an automation user
terraform -chdir=examples/complete plan
```

## Development

From the repository root:

| Target | What it does |
|---|---|
| `make tf-test` | `go vet` and unit tests (client, schema validity, conversions, plan modifiers, docs drift). Part of `make check`. |
| `make tf-lint` | golangci-lint with the repository's configuration. |
| `make tf-acc` | Acceptance tests (`TF_ACC=1`) against a real local central (see below). Needs Go and Terraform ≥ 1.11. |
| `make tf-docs` | Regenerates `docs/` from the schemas and `examples/`. |

The acceptance harness (`internal/provider/harness_test.go`) builds `spaceman` and `devpulse` from the parent module,
initializes a KEK and a central in a fresh temporary directory, bootstraps it (root token generated at run time, never
printed), starts `spaceman serve` on a random loopback port, registers two nodes and runs every `TestAcc*` test: create,
update, import, drift from direct API edits, write-only secret rotation, version publication without perpetual diffs, and
a `node_id` change planned in place and sent to SpaceMan as a relocation (refused there: the harness runs no node agents). It stops only its own processes and removes only its own directory (`SM_TF_KEEP=1` keeps
it for debugging). Nothing in the tests talks to a real model, Pulse deployment or identity provider.
