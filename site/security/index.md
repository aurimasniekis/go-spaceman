# Security

SpaceMan runs software that writes and executes code on your machines, so its security model is explicit about what it
protects and what it does not.

- **Trusted team, not hostile tenants.** Operators and node operators are trusted. A space folder is not a sandbox; see
  the [known limitations](/reference/limitations).
- **Least privilege by default.** Roles are granted per space; global grants are separate. Spaces you cannot read look
  like missing ones. API tokens are limited to their own grants and to their creator's.
- **Strong sign-in.** Argon2id passwords, mandatory TOTP for privileged accounts, recovery codes, throttling, step-up for
  sensitive actions, CSRF protection and same-origin checks for browser sessions.
- **Secrets stay out of the runner.** herdr-core stores a terminal's environment in plain text, so harness secrets reach
  the program through a launcher that reads and deletes a mode-0600 file instead of the terminal environment
  (ADR-0009). Run credentials are scoped to one run and revoked when it ends.
- **Fail closed when central is gone.** Nodes stop work after the lease and grace period, and a lease guard ends runs
  when the agent daemon dies.
- **Everything is audited** in a hash-chained log that can be verified from the UI or API.
- **A strict content security policy** for the UI; the API allows no active content at all. The UI policy allows
  WebAssembly compilation for the terminal renderer (ADR-0020).

Report vulnerabilities as described in `SECURITY.md` in the repository.

- [Threat model](/security/threat-model)
- [Architectural decisions](/security/decisions)
