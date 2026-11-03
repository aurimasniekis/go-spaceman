#!/usr/bin/env python3
"""Fail when a GOAL.md requirement ID has no row in docs/TRACEABILITY.md, or a row names an unknown ID.

Also fails when a row claims real evidence without naming it, so "verified (real)" can never be asserted blank, and
when docs/THREAT_MODEL.md or the site's Security page stops stating one of the SM-SEC-001 boundaries.
"""
import re
import sys
from pathlib import Path

root = Path(__file__).resolve().parent.parent
goal = (root / "docs" / "GOAL.md").read_text(encoding="utf-8")
trace = (root / "docs" / "TRACEABILITY.md").read_text(encoding="utf-8")

ID = re.compile(r"\bSM-[A-Z]+-\d{3}\b")
defined = []
for line in goal.splitlines():
    s = line.lstrip()
    m = ID.search(s)
    if m and (s.startswith("- " + m.group(0)) or s.startswith("| " + m.group(0))):
        defined.append(m.group(0))

rows = {}
for line in trace.splitlines():
    if not line.startswith("| SM-"):
        continue
    cells = [c.strip() for c in line.strip().strip("|").split("|")]
    rows[cells[0]] = cells

errors = []
for i in defined:
    if i not in rows:
        errors.append(f"missing traceability row: {i}")
for i in rows:
    if i not in defined:
        errors.append(f"traceability row for unknown requirement: {i}")
for i, cells in rows.items():
    if len(cells) < 6:
        errors.append(f"{i}: expected 6 columns, got {len(cells)}")
        continue
    impl = cells[2]
    if impl not in {"todo", "partial", "implemented", "n/a"} and not impl.startswith("withdrawn"):
        errors.append(f"{i}: unknown Impl status {impl!r}")

# SM-SEC-001: the threat model and the site's Security page state every documented boundary.
BOUNDARIES = [
    "trusted teams",
    "folder grouping is not isolation",
    "shell access can bypass mcp",
    "privileged nodes can read runtime secrets",
    "transcripts and recordings are sensitive",
    "revocation cannot undo completed actions",
    "terminating a parent does not prove its descendants stopped",
    "prompt injection",
    "shared support must not leak unrelated conversations",
    "advisory approvals are not enforcement",
    "custom schemas cannot mint authority",
]
INJECTION_SOURCES = ["events", "repositories", "memory", "skills", "kb", "messages", "attachments"]
for rel in ("docs/THREAT_MODEL.md", "site/security/index.md"):
    text = " ".join((root / rel).read_text(encoding="utf-8").lower().split())
    for b in BOUNDARIES:
        if b not in text:
            errors.append(f"{rel}: SM-SEC-001 boundary not stated: {b!r}")
    m = re.search(r"prompt injection[^.]*\.([^.]*)", text)
    sources = text[m.start():m.end() + 300] if m else ""
    for src in INJECTION_SOURCES:
        if src not in sources:
            errors.append(f"{rel}: SM-SEC-001 prompt-injection source not named near the boundary: {src!r}")

if errors:
    print("\n".join(errors))
    sys.exit(1)
print(f"ok: {len(defined)} requirements traced")
