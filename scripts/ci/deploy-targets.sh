#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Deploy loops of `.github/workflows/deploy.yml`. Each deployable is uploaded
# in plan order, and the stale-plan check runs right before each upload, so a
# changeset that is withdrawn or released elsewhere while an earlier target
# deploys or this target builds stops the upload.
#
# Usage:
#   deploy-targets.sh servers        # SERVERS: plan servers JSON
#   deploy-targets.sh static-sites   # SITES: plan static sites JSON
#
# Environment:
#   SHA                  deployed revision.
#   RUNNER_TEMP          directory with `servers/<name>.tar.gz` build contexts
#                        and room for scratch files.
#   GITHUB_STEP_SUMMARY  optional summary file.
#   DEPLOY_GUARD         stale-plan check command
#                        (default scripts/ci/check-pending-changesets.sh).
#   DEPLOY_PROVIDER      provider helper (default scripts/ci/deploy-provider.sh).
#   Plus the environment of the stale-plan check and the provider helper.
set -euo pipefail

if (( BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4) )); then
  echo "::error::$0 needs Bash 4.4 or newer, not $BASH_VERSION." >&2
  exit 2
fi

here="$(cd "$(dirname "$0")" && pwd)"
guard="${DEPLOY_GUARD:-$here/check-pending-changesets.sh}"
provider="${DEPLOY_PROVIDER:-$here/deploy-provider.sh}"
summary="${GITHUB_STEP_SUMMARY:-/dev/null}"

# Requires a nonempty JSON array of objects that have every given field, so
# a missing or malformed plan output cannot pass as "nothing to deploy".
require_targets() {
  local label="$1" json="$2"
  shift 2
  if ! jq -e --args 'type == "array" and length > 0 and all(.[]; type == "object" and (. as $o | $ARGS.positional | all(. as $k | $o[$k] | type == "string" and length > 0)))' "$@" <<< "$json" > /dev/null 2>&1; then
    echo "::error::$label is not a nonempty list of deployables: ${json:-<empty>}" >&2
    exit 1
  fi
}

check_plan() {
  "$guard" < /dev/null
}

deploy_servers() {
  require_targets SERVERS "${SERVERS:-}" name version service
  echo "## Servers" >> "$summary"
  local name version service context id
  while IFS=$'\t' read -r -u 3 name version service; do
    context="$RUNNER_TEMP/context-$name"
    mkdir -p "$context"
    tar -xzf "$RUNNER_TEMP/servers/$name.tar.gz" -C "$context"
    echo "::group::Deploy $name@$version to Railway service $service"
    check_plan
    # The exit code of `railway up` is not a verdict; wait for an explicit
    # terminal status of the deployment.
    id="$(railway up "$context" --path-as-root --no-gitignore --detach --json \
      --service "$service" --message "$name@$version ($SHA)" < /dev/null \
      | "$provider" railway-id)"
    "$provider" wait-railway "$service" "$id"
    echo "::endgroup::"
    echo "- $name@$version: Railway service \`$service\`, deployment \`$id\`" >> "$summary"
  done 3< <(jq -r '.[] | [.name, .version, .service] | @tsv' <<< "$SERVERS")
}

deploy_static_sites() {
  require_targets SITES "${SITES:-}" name version project output
  echo "## Static sites" >> "$summary"
  local name version project output result id
  while IFS=$'\t' read -r -u 3 name version project output; do
    echo "::group::Build and deploy $name@$version to Cloudflare Pages project $project"
    pnpm exec turbo run build --filter="$name" < /dev/null
    check_plan
    result="$RUNNER_TEMP/wrangler-$name.ndjson"
    WRANGLER_OUTPUT_FILE_PATH="$result" pnpm exec wrangler pages deploy "$output" \
      --project-name "$project" --branch main --commit-hash "$SHA" \
      --commit-message "$name@$version" < /dev/null
    # Wrangler exits 0 also when it cannot determine the final status; wait
    # until the deploy stage of the deployment succeeds.
    id="$("$provider" pages-id "$result")"
    DEPLOY_POLL_INTERVAL="${DEPLOY_POLL_INTERVAL:-5}" "$provider" wait-pages "$project" "$id"
    echo "::endgroup::"
    echo "- $name@$version: Cloudflare Pages project \`$project\`, deployment \`$id\`" >> "$summary"
  done 3< <(jq -r '.[] | [.name, .version, .project, .output] | @tsv' <<< "$SITES")
}

case "${1:-}" in
  servers) deploy_servers ;;
  static-sites) deploy_static_sites ;;
  *)
    echo "usage: $0 servers | static-sites" >&2
    exit 2
    ;;
esac
