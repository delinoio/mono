#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Validates the pending changesets and the deployable manifests in the CI
# `quality` job, so that a change that would stop every deployment fails in
# its own pull request:
# - `deploy-plan.mjs --check` applies the deploy planner's rules that need no
#   release: changeset file names and the `deploy` manifest fields;
# - `changeset version` rejects a changeset for an unknown package or one that
#   mixes the ignored `delidev` with other packages. It rewrites files, so run
#   this only in a throwaway checkout.
#
# Without a pending changeset there is nothing to validate; `changeset
# version` would exit 1 ("No unreleased changesets found"), and most pull
# requests need no changeset.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(git rev-parse --show-toplevel)"
cd "$root"

node "$here/deploy-plan.mjs" --check "$root"

# The same files that @changesets/read treats as changesets: Markdown files
# that are not hidden, not README.md (any case) and not an agent guidance
# file (AGENTS.md, CLAUDE.md, GEMINI.md).
# (The glob skips hidden files.)
shopt -s nullglob
pending=()
for file in .changeset/*.md .changeset/pre/*.md; do
  name="$(basename "$file")"
  if [[ "$(tr '[:upper:]' '[:lower:]' <<< "$name")" == readme.md || "$name" == AGENTS.md || "$name" == CLAUDE.md || "$name" == GEMINI.md ]]; then
    continue
  fi
  pending+=("$file")
done

if (( ${#pending[@]} == 0 )); then
  echo "No pending changesets to validate."
  exit 0
fi

echo "Validating ${#pending[@]} pending changeset(s): ${pending[*]}"
pnpm exec changeset version
