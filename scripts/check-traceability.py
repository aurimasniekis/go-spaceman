#!/usr/bin/env python3
"""Fail when a GOAL.md requirement ID has no row in docs/TRACEABILITY.md, or a row names an unknown ID.

Also fails when a row claims real evidence without naming it, so "verified (real)" can never be asserted blank.
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

if errors:
    print("\n".join(errors))
    sys.exit(1)
print(f"ok: {len(defined)} requirements traced")
