#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Validates the pending changesets and the deployable manifests in the CI
# `quality` job, so that a change that would stop every deployment fails in
# its own pull request:
# - `deploy-plan.mjs --check` applies the deploy planner's rules that need no
#   release: changeset file names and the `deploy` manifest fields.
# - With pending changesets, the deploy planner checks the proposed releases
#   before `changeset version` exercises versioning. Versioning rewrites files,
#   so run this only in a throwaway checkout.
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

# Changesets CLI 3.0.3 resolves its configured base branch even when reading
# all pending changesets. A detached CI checkout may have only origin/main.
# Create the missing local branch at HEAD. Passing --since would limit the
# plan to changed files and miss changesets already committed on the base.
base_branch="$(node --print 'require("./.changeset/config.json").baseBranch ?? "main"')"
if ! git show-ref --verify --quiet "refs/heads/$base_branch"; then
  git branch -- "$base_branch" HEAD
fi

status_file="$(mktemp -t changeset-status.XXXXXXXX)"
trap 'rm -f -- "$status_file"' EXIT
pnpm exec changeset status --output "$status_file"
env -u GITHUB_OUTPUT -u GITHUB_STEP_SUMMARY node "$here/deploy-plan.mjs" "$status_file"
pnpm exec changeset version
