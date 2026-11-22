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
#   deploy-targets.sh build-static-sites  # SITES; run without deploy secrets
#   deploy-targets.sh static-sites   # SITES: plan static sites JSON
#
# Environment:
#   SHA                  deployed revision.
#   RUNNER_TEMP          directory with the `servers/<name>.tar.gz` images
#                        that `build-servers` saved as `<name>:<SHA>`, and
#                        room for scratch files.
#   DROPLET_REGISTRY     container registry repository prefix for `servers`,
#                        for example `registry.digitalocean.com/<registry>`.
#                        The job must be logged in to the registry.
#   DROPLET_SSH_CONFIG   SSH client configuration file for `servers` that
#                        defines the host alias `droplet`.
#   DROPLET_DEPLOY       remote deploy script that runs on the Droplet
#                        (default scripts/ci/droplet-deploy.sh).
#   GITHUB_STEP_SUMMARY  optional summary file.
#   DEPLOY_GUARD         stale-plan check command
#                        (default scripts/ci/check-pending-changesets.sh).
#   DEPLOY_PROVIDER      provider helper (default scripts/ci/deploy-provider.sh).
#   SITES_ROOT           directory that holds the site outputs for
#                        `static-sites` (default: the working directory). The
#                        workflow unpacks the build artifact outside the
#                        checkout and points this at it.
#   Plus the environment of the stale-plan check and the provider helper.
set -euo pipefail

if (( BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4) )); then
  echo "::error::$0 needs Bash 4.4 or newer, not $BASH_VERSION." >&2
  exit 2
fi

here="$(cd "$(dirname "$0")" && pwd)"
guard="${DEPLOY_GUARD:-$here/check-pending-changesets.sh}"
provider="${DEPLOY_PROVIDER:-$here/deploy-provider.sh}"
remote_deploy="${DROPLET_DEPLOY:-$here/droplet-deploy.sh}"
summary="${GITHUB_STEP_SUMMARY:-/dev/null}"

# Requires a nonempty JSON array of objects that have every given field, so
# a missing or malformed plan output cannot pass as "nothing to deploy".
require_targets() {
  local label="$1" json="$2"
  shift 2
  if ! jq -es --args 'length == 1 and (.[0] | type == "array" and length > 0 and all(.[]; type == "object" and (. as $o | $ARGS.positional | all(. as $k | $o[$k] | type == "string" and length > 0))))' "$@" <<< "$json" > /dev/null 2>&1; then
    echo "::error::$label is not a nonempty list of deployables: ${json:-<empty>}" >&2
    exit 1
  fi
}

check_plan() {
  "$guard" < /dev/null
}

# Pushes each server image to the registry and replaces its container on the
# Droplet. `droplet-deploy.sh` waits for the health check on the Droplet, so
# its exit code is the verdict.
deploy_servers() {
  require_targets SERVERS "${SERVERS:-}" name version service hostname port upstream udpPorts
  if [[ ! "${DROPLET_REGISTRY:-}" =~ ^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+$ ]]; then
    echo "::error::DROPLET_REGISTRY is not a registry repository prefix: ${DROPLET_REGISTRY:-<empty>}" >&2
    exit 1
  fi
  if [[ ! -f "${DROPLET_SSH_CONFIG:-}" ]]; then
    echo "::error::DROPLET_SSH_CONFIG is not an SSH client configuration file: ${DROPLET_SSH_CONFIG:-<empty>}" >&2
    exit 1
  fi
  echo "## Servers" >> "$summary"
  local name version service hostname port upstream udp_ports ref digest image
  while IFS=$'\t' read -r -u 3 name version service hostname port upstream udp_ports; do
    echo "::group::Deploy $name@$version to Droplet service $service"
    docker load --quiet --input "$RUNNER_TEMP/servers/$name.tar.gz" < /dev/null
    ref="$DROPLET_REGISTRY/$service:$SHA"
    docker tag "$name:$SHA" "$ref" < /dev/null
    docker push --quiet "$ref" < /dev/null
    # Deploy by digest, so the Droplet runs exactly the pushed image even
    # when the tag moves.
    digest="$(docker buildx imagetools inspect "$ref" --format '{{json .Manifest}}' < /dev/null \
      | jq -r '.digest')"
    if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
      echo "::error::The registry returned no digest for $ref: ${digest:-<empty>}" >&2
      exit 1
    fi
    image="$DROPLET_REGISTRY/$service@$digest"
    check_plan
    ssh -F "$DROPLET_SSH_CONFIG" droplet bash -s -- \
      "$service" "$image" "$port" "$hostname" "$upstream" "$udp_ports" < "$remote_deploy"
    echo "::endgroup::"
    echo "- $name@$version: Droplet service \`$service\` at \`https://$hostname\`, image \`$image\`" >> "$summary"
  done 3< <(jq -r '.[] | [.name, .version, .service, .hostname, .port, .upstream, .udpPorts] | @tsv' <<< "$SERVERS")
}

# Builds every site in plan order. The workflow runs this in a step without
# deploy secrets, because the build runs third-party plugins.
build_static_sites() {
  require_targets SITES "${SITES:-}" name version project output
  local name version project output
  while IFS=$'\t' read -r -u 3 name version project output; do
    echo "::group::Build $name@$version"
    pnpm exec turbo run build --filter="$name" < /dev/null
    if [[ ! -d "$output" ]]; then
      echo "::error::The build of $name produced no $output directory."
      exit 1
    fi
    echo "::endgroup::"
  done 3< <(jq -r '.[] | [.name, .version, .project, .output] | @tsv' <<< "$SITES")
}

deploy_static_sites() {
  require_targets SITES "${SITES:-}" name version project output
  echo "## Static sites" >> "$summary"
  local name version project output result id
  local sites_root="${SITES_ROOT:-.}"
  while IFS=$'\t' read -r -u 3 name version project output; do
    echo "::group::Deploy $name@$version to Cloudflare Pages project $project"
    output="$sites_root/$output"
    if [[ ! -d "$output" ]]; then
      echo "::error::$output of $name was not built; run build-static-sites first."
      exit 1
    fi
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
  build-static-sites) build_static_sites ;;
  static-sites) deploy_static_sites ;;
  *)
    echo "usage: $0 servers | build-static-sites | static-sites" >&2
    exit 2
    ;;
esac
