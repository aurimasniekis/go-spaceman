# Security

SpaceMan runs software that writes and executes code on your machines, so its security model is explicit about what it
protects and what it does not.

- **Trusted teams, not hostile multi-tenancy.** Operators and node operators are trusted. See the
  [boundaries](#what-spaceman-does-not-protect) below and the [known limitations](/reference/limitations).
- **Least privilege by default.** Roles are granted per space; global grants are separate. Spaces you cannot read look
  like missing ones. API tokens are limited to their own grants and to their creator's.
- **Strong sign-in.** Argon2id passwords, mandatory TOTP for privileged accounts, recovery codes, throttling, step-up for
  sensitive actions, CSRF protection and same-origin checks for browser sessions.
- **Revocation is immediate and bounded.** Revoking a session, token or grant ends open terminals and live updates as
  soon as the change commits; expiry is caught within `auth.stream_revalidate` (at most 60 s). See
  [When access ends](/operators/access#when-access-ends) (ADR-0033).
- **Secrets stay out of the runner.** herdr-core stores a terminal's environment in plain text, so harness secrets reach
  the program through a launcher that reads and deletes a mode-0600 file instead of the terminal environment
  (ADR-0009). Run credentials are scoped to one run and revoked when it ends.
- **Fail closed when central is gone.** Nodes stop work after the lease and grace period, and a lease guard ends runs
  when the agent daemon dies.
- **Everything is audited** in a hash-chained log that can be verified from the UI or API.
- **A strict content security policy** for the UI; the API allows no active content at all. The UI policy allows
  WebAssembly compilation for the terminal renderer (ADR-0020).

## What SpaceMan does not protect

These boundaries are part of the security model (SM-SEC-001); the [threat model](/security/threat-model#security-boundaries-sm-sec-001)
explains each one.

- **Folder grouping is not isolation.** Spaces on one node share the agent's OS user; a space folder is not a sandbox.
- **Shell access can bypass MCP.** An agent with a shell can reach the network and other files directly, outside
  SpaceMan's tools, grants, approvals and audit.
- **Privileged nodes can read runtime secrets.** Root or the agent's OS user on a node can read the credentials of the
  runs there.
- **Transcripts and recordings are sensitive.** They contain whatever the agent saw and typed, secrets included; reading
  and deleting them are separate permissions.
- **Revocation cannot undo completed actions.** It stops what comes next; what already happened stays done.
- **Terminating a parent does not prove its descendants stopped.** Processes an agent started outside its terminal may
  survive the run.
- **Prompt injection is expected.** Events, repositories, memory, skills, KB content, messages and attachments may carry
  instructions; they are untrusted data and never grant anything, because every tool call is authorized centrally.
- **Shared support must not leak unrelated conversations.** Each requester gets its own KB support session; responses
  reach only their target.
- **Advisory approvals are not enforcement.** Only enforced approvals of operations SpaceMan performs gate anything.
- **Custom schemas cannot mint authority.** A custom "approved" field or outcome grants nothing.

The latest code review against these boundaries is
[`docs/SECURITY_REVIEW.md`](https://github.com/aurimasniekis/go-spaceman/blob/main/docs/SECURITY_REVIEW.md).

Report vulnerabilities as described in `SECURITY.md` in the repository.

- [Threat model](/security/threat-model)
- [Architectural decisions](/security/decisions)
