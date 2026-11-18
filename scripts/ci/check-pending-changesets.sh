#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Fails when the plan of this deploy run is no longer current. A rerun of
# failed jobs reuses the outputs of the original `plan` job, so without this
# check an old run could overwrite the deployment of a newer run or release
# changesets that changed after planning. The plan is stale when:
#
# - a newer run of the deploy workflow started, even one that failed or was
#   cancelled, because it may already have deployed a newer revision (a newer
#   run that still waits in the concurrency queue does not count);
# - a planned changeset is no longer on `main` (another run released it); or
# - a planned changeset changed on `main` after planning (different blob).
#
# Only HTTP 404 means "not on main". Other API errors are retried and then
# reported as errors, not as a stale plan.
#
# Environment:
#   CHANGESET_BLOBS     JSON object that maps each planned changeset id to the
#                       Git blob SHA of `.changeset/<id>.md` at the plan.
#   REPOSITORY          owner/name of the GitHub repository.
#   WORKFLOW            file name of the deploy workflow, e.g. deploy.yml.
#   GITHUB_RUN_NUMBER   run number of this run (kept by reruns).
#   GH_TOKEN            token with contents: read and actions: read.
#   CHECK_RETRY_DELAY   optional seconds between retries (default 5).
set -euo pipefail

retry_delay="${CHECK_RETRY_DELAY:-5}"
api_output=""

# Runs `gh api` with retries. Returns 0 on success, 4 on HTTP 404 and 1 on
# any other failure after three attempts. The output or error is left in
# `api_output`.
api() {
  local attempt
  for attempt in 1 2 3; do
    if api_output="$(gh api "$@" 2>&1)"; then
      return 0
    fi
    if [[ "$api_output" == *"(HTTP 404)"* ]]; then
      return 4
    fi
    if (( attempt < 3 )); then
      echo "::warning::gh api $1 failed (attempt $attempt of 3): $api_output"
      sleep "$retry_delay"
    fi
  done
  return 1
}

stale() {
  echo "::error::The plan is stale: $1 Start a new deployment instead of rerunning this one."
  exit 1
}

if [[ ! "$GITHUB_RUN_NUMBER" =~ ^[0-9]+$ ]]; then
  echo "::error::GITHUB_RUN_NUMBER is not a number: $GITHUB_RUN_NUMBER"
  exit 1
fi
status=0
api "repos/$REPOSITORY/actions/workflows/$WORKFLOW/runs?per_page=50" \
  --jq "[.workflow_runs[] | select(.run_number > $GITHUB_RUN_NUMBER and (.status == \"in_progress\" or .status == \"completed\")) | .run_number] | max // 0" \
  || status=$?
if (( status != 0 )); then
  echo "::error::Could not list the runs of $WORKFLOW: $api_output"
  exit 1
fi
if (( api_output > 0 )); then
  stale "run #$api_output of $WORKFLOW started after this run #$GITHUB_RUN_NUMBER."
fi

mapfile -t ids < <(jq -r 'keys[]' <<< "$CHANGESET_BLOBS")
for id in "${ids[@]}"; do
  expected="$(jq -r --arg id "$id" '.[$id]' <<< "$CHANGESET_BLOBS")"
  status=0
  api "repos/$REPOSITORY/contents/.changeset/$id.md?ref=main" --jq '.sha' || status=$?
  case "$status" in
    0) ;;
    4) stale ".changeset/$id.md is no longer on main; another run already released it." ;;
    *)
      echo "::error::Could not read .changeset/$id.md on main: $api_output"
      exit 1
      ;;
  esac
  if [[ "$api_output" != "$expected" ]]; then
    stale ".changeset/$id.md changed on main after planning."
  fi
done
echo "The plan is current: no newer run, and all ${#ids[@]} planned changesets are unchanged on main."
