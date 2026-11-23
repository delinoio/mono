#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Fails when the plan of this deploy run is no longer current. A rerun of
# failed jobs reuses the outputs of the original `plan` job, so without this
# check an old run could overwrite the deployment of a newer run or release
# changesets that changed after planning. The plan is stale when:
#
# - a newer run of the deploy workflow on `main` started, even one that failed or was
#   cancelled, because it may already have deployed a newer revision. A newer
#   run whose first attempt still waits in the concurrency queue does not
#   count, and neither does a cancelled or queued run in which no job of any
#   attempt started (a later dispatch replaces a pending run of the group only
#   when the queue of 100 pending runs, `queue: max`, is full).
#   A queued rerun counts when an earlier attempt started a job;
# - `main` no longer contains the planned revision (rewound or rewritten);
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
#   SHA                 planned revision; `main` must still contain it.
#   GH_TOKEN            token with contents: read and actions: read.
#   CHECK_RETRY_DELAY   optional seconds between retries (default 5).
#   RUNS_ONLY           `true` stops after the newer-run check. The release
#                       recovery (`release-recovery.sh`) uses it after it
#                       proved that the planned release already landed: the
#                       planned changesets are then gone from `main` and
#                       the version commit changed the manifests, so the
#                       other checks would always report a stale plan.
set -euo pipefail

# Empty arrays under `set -u` and `mapfile` need Bash 4.4 or newer.
if (( BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4) )); then
  echo "::error::$0 needs Bash 4.4 or newer, not $BASH_VERSION."
  exit 2
fi

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

# A missing or malformed map would skip every changeset check below.
if ! jq -es 'length == 1 and (.[0] | type == "object" and length > 0 and all(.[]; type == "string" and test("^[0-9a-f]{40}$")))' \
  <<< "${CHANGESET_BLOBS:-}" > /dev/null 2>&1; then
  echo "::error::CHANGESET_BLOBS is not a nonempty map of changeset blob SHAs: ${CHANGESET_BLOBS:-<empty>}"
  exit 1
fi
if [[ ! "${SHA:-}" =~ ^[0-9a-f]{40}$ ]]; then
  echo "::error::SHA is not a full commit SHA: ${SHA:-}"
  exit 1
fi
if [[ ! "${GITHUB_RUN_NUMBER:-}" =~ ^[0-9]+$ ]]; then
  echo "::error::GITHUB_RUN_NUMBER is not a number: ${GITHUB_RUN_NUMBER:-}"
  exit 1
fi
# Collect the started runs on `main` that are newer than this run. The API
# lists runs newest first, so read pages until a page reaches this run or
# runs out; a page of queue-cancelled runs must not hide a newer started run.
newer_runs=""
# Each newer run gets a kind (no empty TSV fields, because a tab is an IFS
# whitespace character and empty fields would collapse):
# - started: in progress, or completed without being cancelled;
# - probe: cancelled, or a rerun (attempt > 1) that waits in the queue; an
#   earlier attempt may have deployed, so its jobs decide;
# - queued: a first attempt that waits in the queue; ignored.
kind_filter='if .status == "in_progress" or (.status == "completed" and .conclusion != "cancelled") then "started"
  elif .status == "completed" or (.run_attempt // 1) > 1 then "probe"
  else "queued" end'
for (( page = 1; ; page++ )); do
  status=0
  api "repos/$REPOSITORY/actions/workflows/$WORKFLOW/runs?branch=main&per_page=100&page=$page" \
    --jq ".workflow_runs[] | [.id, .run_number, ($kind_filter)] | @tsv" \
    || status=$?
  if (( status != 0 )); then
    echo "::error::Could not list the runs of $WORKFLOW: $api_output"
    exit 1
  fi
  [[ -n "$api_output" ]] || break
  reached=false
  while IFS=$'\t' read -r run_id run_number kind; do
    if (( run_number <= GITHUB_RUN_NUMBER )); then
      reached=true
    elif [[ "$kind" != "queued" ]]; then
      newer_runs+="$run_id"$'\t'"$run_number"$'\t'"$kind"$'\n'
    fi
  done <<< "$api_output"
  if [[ "$reached" == true ]]; then
    break
  fi
done
while IFS=$'\t' read -r run_id run_number kind; do
  [[ -n "$run_id" ]] || continue
  if [[ "$kind" == "probe" ]]; then
    # `filter=all` covers every attempt: an earlier attempt of a rerun may
    # have deployed even when the latest attempt was cancelled or still
    # waits in the queue. Read every page until a started job appears.
    started=0
    for (( job_page = 1; ; job_page++ )); do
      status=0
      api "repos/$REPOSITORY/actions/runs/$run_id/jobs?filter=all&per_page=100&page=$job_page" \
        --jq '"\([.jobs[] | select(.started_at != null)] | length) \(.jobs | length)"' || status=$?
      if (( status != 0 )); then
        echo "::error::Could not list the jobs of run #$run_number: $api_output"
        exit 1
      fi
      read -r page_started page_jobs <<< "$api_output"
      if (( page_started > 0 )); then
        started=1
        break
      fi
      (( page_jobs > 0 )) || break
    done
    if (( started == 0 )); then
      echo "Ignoring run #$run_number: no job of any attempt started."
      continue
    fi
  fi
  stale "run #$run_number of $WORKFLOW started after this run #$GITHUB_RUN_NUMBER."
done <<< "$newer_runs"

if [[ "${RUNS_ONLY:-}" == true ]]; then
  echo "No newer run of $WORKFLOW started after this run #$GITHUB_RUN_NUMBER."
  exit 0
fi

# The planned revision must still be on `main`. A `main` that was rewound or
# rewritten after planning can keep a changeset with the same blob, but the
# code to deploy is no longer on `main`.
# The same comparison lists the files that changed on `main` since the plan,
# with both paths of a rename (a manifest renamed away also counts).
# A changed package manifest, lockfile, workspace file or Changesets config
# makes the release job refuse to version and tag, so stop before uploading.
status=0
api "repos/$REPOSITORY/compare/$SHA...main?per_page=100" \
  --jq '{status, total: ((.files // []) | length), manifests: [(.files // [])[] | .filename, (.previous_filename // empty) | select(test("(^|/)package\\.json$") or . == "pnpm-lock.yaml" or . == "pnpm-workspace.yaml" or . == ".changeset/config.json")]}' \
  || status=$?
case "$status" in
  0) ;;
  4) stale "the planned revision $SHA is not in the repository any more." ;;
  *)
    echo "::error::Could not compare the planned revision with main: $api_output"
    exit 1
    ;;
esac
compare="$api_output"
compare_status="$(jq -r '.status' <<< "$compare")"
case "$compare_status" in
  identical | ahead) ;;
  *) stale "main no longer contains the planned revision $SHA (compare status: $compare_status)." ;;
esac
manifests="$(jq -r '.manifests | join(", ")' <<< "$compare")"
if [[ -n "$manifests" ]]; then
  stale "main changed package manifests since the plan ($manifests); the release would not version or tag this deployment."
fi
# The compare API lists at most 300 changed files; a list that may be
# truncated cannot prove that no manifest changed.
if (( $(jq '.total' <<< "$compare") >= 300 )); then
  stale "main changed too many files since the plan to check its package manifests."
fi

# Read the `.changeset` directory of `main` once and compare every planned
# file locally: one request per check, whatever the number of changesets.
status=0
api "repos/$REPOSITORY/contents/.changeset?ref=main&per_page=1000" \
  --jq '[.[] | {key: .name, value: .sha}] | from_entries' || status=$?
case "$status" in
  0) current="$api_output" ;;
  4) current='{}' ;;
  *)
    echo "::error::Could not list .changeset on main: $api_output"
    exit 1
    ;;
esac
# The contents API lists at most 1,000 entries of any type (the map keeps
# subdirectories too, so its size is the listing size); a planned file that
# is missing from a full listing may only be beyond the limit.
listing_full=false
if (( $(jq 'length' <<< "$current") >= 1000 )); then
  listing_full=true
fi
mapfile -t ids < <(jq -r 'keys[]' <<< "$CHANGESET_BLOBS")
for id in "${ids[@]}"; do
  expected="$(jq -r --arg id "$id" '.[$id]' <<< "$CHANGESET_BLOBS")"
  actual="$(jq -r --arg file "$id.md" '.[$file] // ""' <<< "$current")"
  if [[ -z "$actual" && "$listing_full" == true ]]; then
    status=0
    api "repos/$REPOSITORY/contents/.changeset/$id.md?ref=main" --jq '.sha' || status=$?
    case "$status" in
      0) actual="$api_output" ;;
      4) ;;
      *)
        echo "::error::Could not read .changeset/$id.md on main: $api_output"
        exit 1
        ;;
    esac
  fi
  if [[ -z "$actual" ]]; then
    stale ".changeset/$id.md is no longer on main; another run already released it."
  fi
  if [[ "$actual" != "$expected" ]]; then
    stale ".changeset/$id.md changed on main after planning."
  fi
done
echo "The plan is current: no newer run, and all ${#ids[@]} planned changesets are unchanged on main."
