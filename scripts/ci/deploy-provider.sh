#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Provider helpers of `.github/workflows/deploy.yml`. The exit code of
# `wrangler pages deploy` is not a deployment verdict: the CLI can exit 0
# before the deployment has a final status. These helpers read the deployment
# ID and wait for an explicit terminal status. Droplet server deployments need
# no helper: `droplet-deploy.sh` waits for the health check itself and its
# exit code is the verdict. `backup-database` creates an on-demand backup of a
# PlanetScale Postgres branch and waits until its state is `success`, then
# prints the backup ID.
#
# Usage:
#   deploy-provider.sh pages-id <wrangler-output-file>
#   deploy-provider.sh wait-pages <project> <deployment-id>
#   deploy-provider.sh backup-database <database> <branch> <backup-name>
#
# Environment:
#   CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID  for wait-pages.
#   PLANETSCALE_ORGANIZATION, PLANETSCALE_SERVICE_TOKEN  for backup-database.
#                         The service token needs the `write_backups` and
#                         `read_backups` accesses on the database.
#   PLANETSCALE_API       API base URL (default https://api.planetscale.com/v1).
#   DEPLOY_BACKUP_RETENTION_DAYS  days that PlanetScale keeps the backup
#                         (default 14).
#   DEPLOY_BACKUP_TIMEOUT seconds to wait for the backup (default 2400).
#   DEPLOY_POLL_INTERVAL  seconds between status reads (default 10).
#   DEPLOY_POLL_TIMEOUT   seconds before giving up (default 600).
#   DEPLOY_POLL_ERRORS    failed status reads in a row before giving up
#                         (default 5).
set -euo pipefail

# The other deploy scripts need Bash 4.4, so keep one minimum.
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

# Prints the last `pages-deploy` deployment ID of a Wrangler output file
# (`WRANGLER_OUTPUT_FILE_PATH`, newline-delimited JSON). Lines that are not
# valid JSON are skipped, and a missing, unreadable or non-file path reports
# the same "no deployment id" error instead of a raw jq failure.
pages_id() {
  local id=""
  if [[ -f "$1" && -r "$1" ]]; then
    # `|| true` keeps errexit/pipefail from ending the script before the
    # error branch below when jq still fails on the file.
    id="$(jq -R -r 'fromjson? | objects | select(.type == "pages-deploy") | .deployment_id | select(type == "string" and length > 0)' "$1" 2>/dev/null | tail -n 1)" || true
  fi
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

# Status read. `$1` is the project and `$2` the deployment ID.
pages_status() {
  local project="$1" id="$2"
  curl -fsS --retry 3 \
    -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
    "https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/pages/projects/$project/deployments/$id" \
    | jq -r '.result | "\(.environment) \(.latest_stage.name) \(.latest_stage.status)"'
}

# A preview deployment never updates production, for example when the
# project's production branch is not `main`; treat it as a failure.
pages_classify() {
  case "$1" in
    "production deploy success") echo success ;;
    production" "*" failure" | production" "*" canceled") echo failure ;;
    production" "*) echo pending ;;
    *) echo failure ;;
  esac
}

# A PlanetScale organization, database or branch name. They become API path
# segments, so only lowercase letters, digits and inner hyphens are accepted.
planetscale_name='^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'

planetscale_backups_url() {
  echo "${PLANETSCALE_API:-https://api.planetscale.com/v1}/organizations/$PLANETSCALE_ORGANIZATION/databases/$1/branches/$2/backups"
}

# Status read of a backup. `$1` is `<database>:<branch>` and `$2` the backup
# ID.
backup_status() {
  local database="${1%%:*}" branch="${1#*:}" id="$2"
  curl -fsS --retry 3 \
    -H "Authorization: Bearer $PLANETSCALE_SERVICE_TOKEN" \
    "$(planetscale_backups_url "$database" "$branch")/$id" \
    | jq -r '.state'
}

# `pending` and `running` are transient; `failed`, `canceled` and `ignored` are
# terminal failures, and so is any state that PlanetScale does not document.
backup_classify() {
  case "$1" in
    success) echo success ;;
    pending | running) echo pending ;;
    *) echo failure ;;
  esac
}

backup_database() {
  local database="$1" branch="$2" name="$3" variable
  for variable in PLANETSCALE_ORGANIZATION PLANETSCALE_SERVICE_TOKEN; do
    if [[ -z "${!variable:-}" ]]; then
      echo "::error::backup-database needs $variable." >&2
      return 2
    fi
  done
  local -A names=([organization]="$PLANETSCALE_ORGANIZATION" [database]="$database" [branch]="$branch")
  local label
  for label in organization database branch; do
    if [[ ! "${names[$label]}" =~ $planetscale_name ]]; then
      echo "::error::The PlanetScale $label ${names[$label]@Q} is not valid." >&2
      return 2
    fi
  done
  # The backup name is longer than one path segment name: it carries the
  # service, the revision and the run.
  if [[ ! "$name" =~ ^[a-z0-9][a-z0-9-]{0,126}[a-z0-9]$ ]]; then
    echo "::error::The PlanetScale backup name ${name@Q} is not valid." >&2
    return 2
  fi
  local retention="${DEPLOY_BACKUP_RETENTION_DAYS:-14}"
  if [[ ! "$retention" =~ ^[1-9][0-9]{0,2}$ ]]; then
    echo "::error::DEPLOY_BACKUP_RETENTION_DAYS ${retention@Q} is not a number of days from 1 to 999." >&2
    return 2
  fi
  local body response id
  body="$(jq -cn --arg name "$name" --argjson days "$retention" \
    '{name: $name, retention_unit: "day", retention_value: $days, emergency: false}')"
  # No retry: a repeated request could create a second backup. A failed
  # request fails the deployment before any server deploys.
  if ! response="$(curl -fsS -X POST \
    -H "Authorization: Bearer $PLANETSCALE_SERVICE_TOKEN" \
    -H "Content-Type: application/json" \
    --data "$body" \
    "$(planetscale_backups_url "$database" "$branch")")"; then
    echo "::error::PlanetScale did not create the backup $name of $database/$branch." >&2
    return 1
  fi
  id="$(jq -r '.id | select(type == "string")' <<< "$response" 2> /dev/null)" || id=""
  if [[ ! "$id" =~ ^[A-Za-z0-9_-]+$ ]]; then
    echo "::error::PlanetScale returned no backup ID for $name of $database/$branch." >&2
    return 1
  fi
  echo "Created the PlanetScale backup $name ($id) of $database/$branch." >&2
  # Status lines go to standard error: standard output carries only the ID.
  poll "PlanetScale backup $name of $database/$branch" "${DEPLOY_BACKUP_TIMEOUT:-2400}" \
    read-backup backup_classify "$database:$branch" "$id" >&2
  echo "$id"
}

# Prints the usage line and exits with status 2. The `*)` branch of the command
# dispatch and the argument-count check below share it.
usage() {
  echo "usage: $0 pages-id <file> | wait-pages <project> <id> | backup-database <database> <branch> <name>" >&2
  exit 2
}

command="${1:-}"
# `set -u` would stop a missing positional argument with a Bash diagnostic and
# exit status 1, so count the arguments of each subcommand before dispatch.
# `read-pages` and `read-backup` are the internal status reads of `poll` and
# stay out of the usage line.
case "$command" in
  pages-id) required_arguments=1 ;;
  read-pages | wait-pages | read-backup) required_arguments=2 ;;
  backup-database) required_arguments=3 ;;
  *) required_arguments=0 ;;
esac
if (( $# <= required_arguments )); then
  usage
fi

case "$command" in
  pages-id) pages_id "$2" ;;
  read-pages) pages_status "$2" "$3" ;;
  read-backup) backup_status "$2" "$3" ;;
  backup-database)
    if [[ -z "$timeout_command" ]]; then
      echo "::error::$0 $command needs GNU coreutils timeout (or gtimeout)." >&2
      exit 2
    fi
    backup_database "$2" "$3" "$4"
    ;;
  wait-pages)
    if [[ -z "$timeout_command" ]]; then
      echo "::error::$0 $command needs GNU coreutils timeout (or gtimeout)." >&2
      exit 2
    fi
    project="$2" id="$3"
    poll "Pages deployment $id of $project" "${DEPLOY_POLL_TIMEOUT:-600}" read-pages pages_classify "$project" "$id"
    ;;
  *) usage ;;
esac
