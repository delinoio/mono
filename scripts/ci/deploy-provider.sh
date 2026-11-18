#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Provider helpers of `.github/workflows/deploy.yml`. The exit codes of
# `railway up` and `wrangler pages deploy` are not deployment verdicts: both
# CLIs can exit 0 before the deployment has a final status. These helpers read
# the deployment ID and wait for an explicit terminal status.
#
# Usage:
#   deploy-provider.sh railway-id < railway-up-json-output
#   deploy-provider.sh pages-id <wrangler-output-file>
#   deploy-provider.sh wait-railway <service> <deployment-id>
#   deploy-provider.sh wait-pages <project> <deployment-id>
#
# Environment:
#   CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID  for wait-pages.
#   DEPLOY_POLL_INTERVAL  seconds between status reads (default 10).
#   DEPLOY_POLL_TIMEOUT   seconds before giving up (default 1800 for Railway,
#                         600 for Pages).
#   DEPLOY_POLL_ERRORS    failed status reads in a row before giving up
#                         (default 5).
set -euo pipefail

# `;;&` in the command dispatch needs Bash 4; the other deploy scripts need
# 4.4, so keep one minimum.
if (( BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4) )); then
  echo "::error::$0 needs Bash 4.4 or newer, not $BASH_VERSION." >&2
  exit 2
fi

interval="${DEPLOY_POLL_INTERVAL:-10}"
# GNU coreutils `timeout` (`gtimeout` when Homebrew installs it with a prefix).
timeout_command=""
for candidate in timeout gtimeout; do
  if command -v "$candidate" > /dev/null; then
    timeout_command="$candidate"
    break
  fi
done
max_errors="${DEPLOY_POLL_ERRORS:-5}"

# Prints the last `deploymentId` of the `railway up --detach --json` output on
# stdin. Lines that are not JSON objects are ignored.
# Errors go to stderr, because the workflow captures stdout as the ID.
railway_id() {
  local id
  id="$(jq -R -r 'fromjson? | objects | .deploymentId | select(type == "string" and length > 0)' | tail -n 1)"
  if [[ -z "$id" ]]; then
    echo "::error::railway up returned no deployment id." >&2
    return 1
  fi
  echo "$id"
}

# Prints the last `pages-deploy` deployment ID of a Wrangler output file
# (`WRANGLER_OUTPUT_FILE_PATH`, newline-delimited JSON).
pages_id() {
  local id
  id="$(jq -r 'select(.type == "pages-deploy") | .deployment_id | select(type == "string" and length > 0)' "$1" | tail -n 1)"
  if [[ -z "$id" ]]; then
    echo "::error::wrangler pages deploy returned no deployment id." >&2
    return 1
  fi
  echo "$id"
}

# Polls the status until `classify` reports success or failure. Each status
# read runs as `bash $0 <read-command> <name> <id>` under `timeout`, so a stalled
# request cannot outlive the deadline, and a result that arrives after the
# deadline is not accepted. Tolerates up to `max_errors` failed reads in a
# row; the deployment may still succeed.
# Arguments: label, timeout, read command, classify function, name, id.
poll() {
  local label="$1" timeout="$2" read_command="$3" classify="$4" name="$5" id="$6"
  local value="" errors=0 deadline=$((SECONDS + timeout)) verdict remaining
  while (( (remaining = deadline - SECONDS) > 0 )); do
    if ! value="$("$timeout_command" "$remaining" bash "$0" "$read_command" "$name" "$id")" || (( SECONDS >= deadline )); then
      (( SECONDS < deadline )) || break
      if (( ++errors >= max_errors )); then
        echo "::error::Could not read the status of $label $max_errors times in a row."
        return 1
      fi
      echo "::warning::Could not read the status of $label (failure $errors of $max_errors)."
      nap
      continue
    fi
    errors=0
    verdict="$("$classify" "$value")"
    case "$verdict" in
      success) echo "$label: $value"; return 0 ;;
      failure) echo "::error::$label ended with $value."; return 1 ;;
    esac
    echo "$label: ${value:-not listed yet}"
    nap
  done
  echo "::error::$label did not finish within $timeout seconds (last status: ${value:-unknown})."
  return 1
}

# Sleeps for the poll interval, but not past the deadline.
nap() {
  local left=$((deadline - SECONDS))
  (( left > 0 )) || return 0
  sleep "$(( interval < left ? interval : left ))"
}

# Status reads. `$1` is the service or project and `$2` the deployment ID.
railway_status() {
  local service="$1" id="$2"
  railway deployment list --service "$service" --limit 20 --json < /dev/null \
    | jq -r --arg id "$id" '.[] | select(.id == $id) | .status'
}

railway_classify() {
  case "$1" in
    SUCCESS) echo success ;;
    FAILED | CRASHED | REMOVED | REMOVING | SKIPPED) echo failure ;;
    *) echo pending ;;
  esac
}

pages_status() {
  local project="$1" id="$2"
  curl -fsS --retry 3 \
    -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
    "https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/pages/projects/$project/deployments/$id" \
    | jq -r '.result.latest_stage | "\(.name) \(.status)"'
}

pages_classify() {
  case "$1" in
    "deploy success") echo success ;;
    *" failure" | *" canceled") echo failure ;;
    *) echo pending ;;
  esac
}

command="${1:-}"
case "$command" in
  railway-id) railway_id ;;
  pages-id) pages_id "$2" ;;
  read-railway) railway_status "$2" "$3" ;;
  read-pages) pages_status "$2" "$3" ;;
  wait-railway | wait-pages)
    if [[ -z "$timeout_command" ]]; then
      echo "::error::$0 $command needs GNU coreutils timeout (or gtimeout)." >&2
      exit 2
    fi
    ;;&
  wait-railway)
    service="$2" id="$3"
    if ! poll "Railway deployment $id of $service" "${DEPLOY_POLL_TIMEOUT:-1800}" read-railway railway_classify "$service" "$id"; then
      # Best-effort diagnostics with a short limit, so an unresponsive API
      # cannot hold the deployment queue after the verdict.
      "$timeout_command" 30 railway logs "$id" --service "$service" --build --lines 200 < /dev/null || true
      "$timeout_command" 30 railway logs "$id" --service "$service" --deployment --lines 200 < /dev/null || true
      exit 1
    fi
    ;;
  wait-pages)
    project="$2" id="$3"
    poll "Pages deployment $id of $project" "${DEPLOY_POLL_TIMEOUT:-600}" read-pages pages_classify "$project" "$id"
    ;;
  *)
    echo "usage: $0 railway-id | pages-id <file> | wait-railway <service> <id> | wait-pages <project> <id>" >&2
    exit 2
    ;;
esac
