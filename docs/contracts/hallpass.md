# Contract: go-hallpass (conventions and integration)

- **Revision:** `github.com/aurimasniekis/go-hallpass` **v0.7.0**, commit `bdd8329a33d0…` (local `../hallpass`), Apache-2.0. Inspected 2026-11-02.

## Patterns SpaceMan copies (source of truth for SM-AUTH-001)
- **Argon2id** (`internal/opauth/password.go`): param set v1 `m=64 MiB, t=3, p=2, keyLen=32, saltLen=16`, stored as a PHC string and upgraded on
  login. Passwords are NFKC-normalised, max 1024 bytes, min 12. Unknown accounts get a dummy hash. Hashing concurrency is limited by a semaphore (4).
- **Sessions:** `__Host-` cookie (non-`__Host-` on plain-http dev), HttpOnly, SameSite=Lax. A 256-bit token stored as `sha256(prefix|token)`.
  Idle 30 min, absolute 12 h, re-auth window 5 min. Rotated on MFA, re-auth and privilege change. A "limited" principal still owes MFA.
- **CSRF:** `base64url(HMAC(csrf_secret, prefix|displayID))`, sent in `X-CSRF-Token` and requiring `Origin == public_url` or
  `Sec-Fetch-Site: same-origin`. Bearer requests are exempt.
- **Throttle:** SQLite keys `acct:`, `ip:` (IPv6 grouped by /64) and `mfa:`. First 3 failures free, then 1 s doubling to 15 min, decaying after
  24 h. No permanent lockout.
- **TOTP:** RFC 6238, SHA-1, 6 digits, 30 s, ±1 step, replay blocked through the last accepted step. 160-bit secret, sealed.
- **Recovery codes:** 10 codes `xxxxxx-xxxxxx`, HMAC-stored.
- **API tokens:** `<prefix>_at_<43 base64url>`, HMAC-stored with a 4-char hint, expiry 1–366 days or never. Bearer only.
- **Keyring:** AES-256-GCM envelope with a 32-byte KEK file (0400, `O_EXCL` creation). Purpose keys are versioned
  (`active|decrypt_only|retired`). Seal format `0x01|u32 version|12-byte nonce|ct`, AAD bound to table/column/row. `MACAll` for lookups
  across rotations. KEK id = `hex(sha256("…-kek-id"||kek)[:8])`.
- **Store:** described in ADR-0002. **Audit:** hash-chained, appended in the same transaction. **HTTP:** RFC 9457 problems, `If-Match`
  version, `Idempotency-Key` (8–128 printable, 24 h, per principal, request hash, 409 on mismatch), cursor pagination.
- **Packaging:** distroless `static-debian12:nonroot` image built in three stages; `-tags embedui`; hardened systemd unit; release script
  with Syft SBOMs and SHA256SUMS; GitHub Actions pinned by SHA.

## Divergences (recorded as ADRs)
- **Bootstrap:** Hallpass uses a 30-minute one-time setup token. SpaceMan's brief requires a **non-expiring, full-scope root token**, shown
  once. SpaceMan does both: `spaceman admin bootstrap` prints the root token, and the root token (or the UI setup flow) creates the first admin user.
- **RBAC:** Hallpass uses a hand-written matrix. SpaceMan uses Casbin (ADR-0004).
- **API:** Hallpass has a hand-written OpenAPI spec. SpaceMan uses huma code-first (ADR-0003).

## Integration points
- **Hallpass Git roles:** nodes may clone through `git clone center:<remote>/<repo>.git`. That is node-provided Git auth (SM-REPO-005), and SpaceMan
  only records the remote URL.
- **Hallpass skill:** `integrations/skills/hallpass/SKILL.md` (+ `references/commands.md`). SpaceMan's skill catalog can import it as a
  versioned skill (SM-MCP-004).
- **Hallpass agent sessions:** scoped via `HALLPASS_SESSION` or `CLAUDE_CODE_SESSION_ID` env. SpaceMan sets `HALLPASS_SESSION=<run id>` for
  traceability. **This is not a security boundary** (Hallpass ADR-0061).
- Hallpass approvals stay in Hallpass. A SpaceMan human action may *link* to a Hallpass request, but it never approves it (SM-APPR-006).
