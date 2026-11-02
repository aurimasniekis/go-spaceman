# Security policy

SpaceMan holds harness credentials, launches processes on operator machines, and mediates human approvals. Please report security
issues privately to **aurimas@niekis.lt**, not in public issues. Include affected versions, reproduction steps, and the impact you observed.

## Supported versions

Pre-1.0: only the latest released minor version receives fixes.

## Security model in one paragraph

SpaceMan is built for **trusted teams**, not hostile multi-tenancy. Space folders are not sandboxes. An agent with shell access can go around
MCP mediation. Privileged node operators can read runtime secrets. Transcripts and recordings are sensitive. Events, repositories, memory,
skills, KB content, messages and attachments may contain prompt injection. Advisory approvals are not technical enforcement. Custom
human-action schemas cannot grant authority. Revoking access cannot undo actions that already completed. The full model is in
[`docs/THREAT_MODEL.md`](docs/THREAT_MODEL.md), and the explicit limits are in [`docs/LIMITATIONS.md`](docs/LIMITATIONS.md).
