#!/usr/bin/env bash
# Does a change touch what the documentation site is built from? (ADR-0062: the one place this path rule lives.)
#
#   scripts/site-affected.sh [<base>]
#
# Compares the working tree (committed, staged, unstaged and untracked files) with <base>. Without <base> it uses the
# merge base of HEAD with $SM_SITE_BASE, or else the nearest one with a known main branch (main or <remote>/main).
# Exits 0 when the site may change (the axe audit must run), 1 when it can't, and 0 as well when it can't tell (no
# base found, a git error): an unknown change is treated as site-affecting. CI passes HEAD^1 on a pull request's merge
# commit.
#
# Site-affecting: the site itself, docs/ (included into site pages), the UI and its audit spec, the OpenAPI document
# (the API reference), CHANGELOG.md (the changelog page), the Makefile targets, this script and the CI workflow.
set -uo pipefail

pattern='^(site/|docs/|web/|api/|CHANGELOG\.md$|Makefile$|scripts/site-affected\.sh$|\.github/workflows/ci\.yaml$)'

affected() {
  echo "site-affected: $1: the accessibility audit runs."
  exit 0
}

base="${1:-}"
if [[ -z "$base" && -n "${SM_SITE_BASE:-}" ]]; then
  base="$(git merge-base HEAD "$SM_SITE_BASE" 2>/dev/null)" || affected "no merge base with $SM_SITE_BASE"
fi
if [[ -z "$base" ]]; then
  # The nearest of the main branches this clone knows (local and every remote's): a stale remote isn't the base.
  best=""
  while read -r ref; do
    mb="$(git merge-base HEAD "$ref" 2>/dev/null)" || continue
    n="$(git rev-list --count "$mb..HEAD")" || continue
    if [[ -z "$best" || "$n" -lt "$best" ]]; then
      best="$n"
      base="$mb"
    fi
  done < <(git for-each-ref --format='%(refname)' refs/heads/main 'refs/remotes/*/main')
  [[ -n "$base" ]] || affected "no main branch to compare with (set SM_SITE_BASE)"
fi

if ! changed="$(git diff --name-only "$base" -- && git ls-files --others --exclude-standard)"; then
  affected "could not list the changes against $base"
fi

if hits="$(grep -E "$pattern" <<<"$changed")"; then
  affected "$(wc -l <<<"$hits") site-affecting path(s) changed, first $(head -n 1 <<<"$hits")"
fi
echo "site-affected: no site, docs, web or API change since ${base:0:12}: the accessibility audit is skipped."
exit 1
