#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Fails when a changeset that the deploy plan selected is no longer pending on
# `main`. A rerun of failed jobs reuses the outputs of the original `plan` job,
# so without this check an old run could deploy a revision that a later run
# already superseded and released.
#
# Environment:
#   CHANGESETS   JSON array of changeset ids from the plan job.
#   REPOSITORY   owner/name of the GitHub repository.
#   GH_TOKEN     token with contents: read.
set -euo pipefail

mapfile -t ids < <(jq -r '.[]' <<< "$CHANGESETS")
missing=()
for id in "${ids[@]}"; do
  if ! gh api --silent "repos/$REPOSITORY/contents/.changeset/$id.md?ref=main" 2> /dev/null; then
    missing+=("$id")
  fi
done

if (( ${#missing[@]} > 0 )); then
  echo "::error::The plan is stale: .changeset/{$(IFS=,; echo "${missing[*]}")}.md is no longer pending on main. Another run already released it. Start a new deployment instead of rerunning this one."
  exit 1
fi
echo "All ${#ids[@]} planned changesets are still pending on main."
