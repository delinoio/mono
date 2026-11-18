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

interval="${DEPLOY_POLL_INTERVAL:-10}"
max_errors="${DEPLOY_POLL_ERRORS:-5}"

# Prints the last `deploymentId` of the `railway up --detach --json` output on
# stdin. Lines that are not JSON objects are ignored.
railway_id() {
  local id
  id="$(jq -R -r 'fromjson? | objects | select(.deploymentId != null) | .deploymentId' | tail -n 1)"
  if [[ -z "$id" ]]; then
    echo "::error::railway up returned no deployment id."
    return 1
  fi
  echo "$id"
}

# Prints the last `pages-deploy` deployment ID of a Wrangler output file
# (`WRANGLER_OUTPUT_FILE_PATH`, newline-delimited JSON).
pages_id() {
  local id
  id="$(jq -r 'select(.type == "pages-deploy") | .deployment_id' "$1" | tail -n 1)"
  if [[ -z "$id" ]]; then
    echo "::error::wrangler pages deploy returned no deployment id."
    return 1
  fi
  echo "$id"
}

# Polls `read_status` until `classify` reports success or failure. Tolerates
# up to `max_errors` failed reads in a row; the deployment may still succeed.
# Arguments: label, timeout, read function, classify function.
poll() {
  local label="$1" timeout="$2" read_status="$3" classify="$4"
  local value="" errors=0 deadline=$((SECONDS + timeout)) verdict
  while (( SECONDS < deadline )); do
    if ! value="$("$read_status")"; then
      if (( ++errors >= max_errors )); then
        echo "::error::Could not read the status of $label $max_errors times in a row."
        return 1
      fi
      echo "::warning::Could not read the status of $label (failure $errors of $max_errors)."
      sleep "$interval"
      continue
    fi
    errors=0
    verdict="$("$classify" "$value")"
    case "$verdict" in
      success) echo "$label: $value"; return 0 ;;
      failure) echo "::error::$label ended with $value."; return 1 ;;
    esac
    echo "$label: ${value:-not listed yet}"
    sleep "$interval"
  done
  echo "::error::$label did not finish within $timeout seconds (last status: ${value:-unknown})."
  return 1
}

railway_status() {
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
  wait-railway)
    service="$2" id="$3"
    if ! poll "Railway deployment $id of $service" "${DEPLOY_POLL_TIMEOUT:-1800}" railway_status railway_classify; then
      railway logs "$id" --service "$service" --build --lines 200 < /dev/null || true
      railway logs "$id" --service "$service" --deployment --lines 200 < /dev/null || true
      exit 1
    fi
    ;;
  wait-pages)
    project="$2" id="$3"
    poll "Pages deployment $id of $project" "${DEPLOY_POLL_TIMEOUT:-600}" pages_status pages_classify
    ;;
  *)
    echo "usage: $0 railway-id | pages-id <file> | wait-railway <service> <id> | wait-pages <project> <id>" >&2
    exit 2
    ;;
esac
