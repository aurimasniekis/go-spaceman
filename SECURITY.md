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

## Telegram

The optional Telegram bot and Mini App (ADR-0083) let a linked person act as themselves from a phone. A link is a
capped principal: its grants are the intersection of its own cap (operator by default) and the person's current grants,
so it is never wider than the person and loses a grant when they do. A person who still has to enroll two-factor
authentication gets a **limited** link. Step-up, re-authentication, MFA and personal operations are refused to
Telegram by classification, and a test fails on any operation nobody classified. Confirmations are single-use and bound to
the link, the card and its version. Enforced approvals are decided in the browser unless a space opts in. The Mini App
and the webhook have their own routes and hosts: the Mini App host answers `404` and never reaches the web app or the
API whenever no running bot serves it (a host known from `telegram.miniapp_host` or the bot's stored host is never routed to the web app), and long-lived streams carry a heartbeat. The
bot token is write-only and sealed. Content goes to Telegram's cloud only for spaces that opt in and at the level they
choose. Everything is built against a fake server: **it has not been verified against a real bot**. Details:
[`docs/THREAT_MODEL.md`](docs/THREAT_MODEL.md#telegram-bot-and-mini-app-adr-0083-sm-tg-001014) and
[`docs/LIMITATIONS.md`](docs/LIMITATIONS.md).

