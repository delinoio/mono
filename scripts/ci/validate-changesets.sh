#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Validates the pending changesets in the CI `quality` job, so that an invalid
# one (an unknown package, or the ignored `delidev` mixed with other packages)
# cannot reach `main` and stop every deployment. `changeset version` rejects
# both. It rewrites files, so run this only in a throwaway checkout.
#
# Without a pending changeset there is nothing to validate; `changeset
# version` would exit 1 ("No unreleased changesets found"), and most pull
# requests need no changeset.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

shopt -s nullglob
pending=()
for file in .changeset/*.md; do
  [[ "$(basename "$file")" == README.md ]] || pending+=("$file")
done

if (( ${#pending[@]} == 0 )); then
  echo "No pending changesets to validate."
  exit 0
fi

echo "Validating ${#pending[@]} pending changeset(s): ${pending[*]}"
pnpm exec changeset version
