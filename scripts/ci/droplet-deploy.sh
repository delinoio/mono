#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Replaces the container of one server on the shared DigitalOcean Droplet.
# `deploy-targets.sh servers` runs this script on the Droplet as the `deploy`
# user over SSH, with the script on standard input:
#
#   ssh <droplet> bash -s -- <service> <image> <port> <hostname> < droplet-deploy.sh
#
# A maintainer rolls a service back with the same command and the image of
# the previous deployment (`/srv/delino/state/<service>.previous`).
#
# Steps: pull the image by digest, start a new container next to the running
# one under the same network alias, wait until `GET /healthz` returns a
# success status, write the reverse proxy route and reload the proxy, and only
# then stop the old container. A failure before the old container stops
# removes the new container, so the old one keeps serving. The exit code is
# the deployment verdict.
#
# Arguments:
#   service   Droplet service name: container name prefix, network alias of
#             the reverse proxy upstream and env file name.
#   image     image reference pinned by digest (`<repository>@sha256:<hex>`).
#   port      port that the server listens on inside the container. The
#             script sets `ADDR=:<port>`.
#   hostname  public host name that the reverse proxy routes to the service.
#
# Environment (defaults match `scripts/droplet/provision.sh`):
#   DROPLET_ROOT            state directory (default /srv/delino).
#   DROPLET_NETWORK         Docker network of the proxy and the servers
#                           (default delino).
#   DROPLET_PROXY           reverse proxy container name (default caddy).
#   DROPLET_HEALTH_TIMEOUT  seconds to wait for the health check (default 120).
#   DROPLET_STOP_TIMEOUT    seconds between SIGTERM and SIGKILL when the old
#                           container stops (default 30).
set -euo pipefail

# The whole script is one function that the last line calls with standard
# input from /dev/null. `bash -s` reads the script from standard input while
# it runs, so a command that read standard input would consume the rest of
# the script.
main() {
  if (( BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4) )); then
    echo "::error::droplet-deploy.sh needs Bash 4.4 or newer, not $BASH_VERSION." >&2
    exit 2
  fi
  if (( $# != 4 )); then
    echo "usage: droplet-deploy.sh <service> <image> <port> <hostname>" >&2
    exit 2
  fi
  local service="$1" image="$2" port="$3" hostname="$4"
  # The planner already checks these values; check them again, because they
  # become file names, container names and proxy configuration here.
  local label='[a-z0-9](-*[a-z0-9])*'
  if [[ ! "$service" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]]; then
    fail "the service name ${service@Q} is not valid."
  fi
  if [[ ! "$image" =~ ^[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64}$ ]]; then
    fail "the image ${image@Q} is not a reference pinned by digest."
  fi
  if [[ ! "$port" =~ ^[1-9][0-9]{3,4}$ ]] || (( port < 1024 || port > 65535 )); then
    fail "the port ${port@Q} is not a number from 1024 to 65535."
  fi
  if [[ ! "$hostname" =~ ^$label(\.$label)+$ ]] || (( ${#hostname} > 253 )); then
    fail "the host name ${hostname@Q} is not valid."
  fi

  local root="${DROPLET_ROOT:-/srv/delino}"
  local network="${DROPLET_NETWORK:-delino}"
  local proxy="${DROPLET_PROXY:-caddy}"
  local health_timeout="${DROPLET_HEALTH_TIMEOUT:-120}"
  local stop_timeout="${DROPLET_STOP_TIMEOUT:-30}"
  local env_file="$root/env/$service.env"
  local site="$root/caddy/sites/$service.caddy"
  local state="$root/state/$service"

  if [[ ! -f "$env_file" ]]; then
    fail "$env_file does not exist. Create it with the service configuration before the first deployment."
  fi
  # The script owns the listen address, so that the proxy route and the
  # process agree on the port.
  if grep -Eq '^[[:space:]]*ADDR=' "$env_file"; then
    fail "$env_file sets ADDR. Remove it; the deployment sets ADDR=:$port."
  fi

  echo "Pull $image"
  docker pull --quiet "$image"

  # Read the old containers into a variable first: a process substitution
  # would discard the exit status of docker ps, and a failed listing would
  # look like a first deployment and leave the old containers running.
  local -a old=()
  local listing
  if ! listing="$(docker ps --all --quiet --filter "label=io.delino.service=$service")"; then
    fail "could not list the containers of $service; no container was started."
  fi
  if [[ -n "$listing" ]]; then
    mapfile -t old <<< "$listing"
  fi
  local new
  new="$service-$(date -u +%Y%m%d%H%M%S)-${image: -12}"
  echo "Start $new"
  docker run --detach --name "$new" \
    --network "$network" --network-alias "$service" \
    --label "io.delino.service=$service" --label "io.delino.image=$image" \
    --restart unless-stopped \
    --env-file "$env_file" --env "ADDR=:$port" \
    "$image" > /dev/null

  if ! wait_healthy "$new" "$network" "$port" "$health_timeout"; then
    discard_replacement "$new"
    fail "$new did not pass the health check; the previous container keeps serving."
  fi

  if ! write_route "$site" "$service" "$port" "$hostname" "$proxy"; then
    discard_replacement "$new"
    fail "the reverse proxy did not accept the route of $service; the previous container keeps serving."
  fi

  local id
  for id in "${old[@]}"; do
    echo "Stop the previous container $id"
    docker stop --time "$stop_timeout" "$id" > /dev/null
    docker rm "$id" > /dev/null
  done

  mkdir -p "$root/state"
  if [[ -f "$state.current" ]] && [[ "$(< "$state.current")" != "$image" ]]; then
    mv "$state.current" "$state.previous"
  fi
  echo "$image" > "$state.current"
  # Removes images that no container uses any more. A rollback pulls the
  # previous image from the registry again.
  docker image prune --force > /dev/null || true
  echo "$service runs $image behind https://$hostname"
}

fail() {
  echo "::error::$1" >&2
  exit 1
}

# Prints the log tail of a replacement that failed, then removes it. A failed
# log read or removal must not hide the deployment failure the caller reports.
discard_replacement() {
  echo "Last log lines of $1:"
  docker logs --tail 200 "$1" 2>&1 || true
  docker rm --force "$1" > /dev/null || true
}

# Waits until the container answers `GET /healthz` with a success status.
# Fails at once when the container stops.
wait_healthy() {
  local container="$1" network="$2" port="$3" timeout="$4"
  local deadline=$((SECONDS + timeout)) status address
  while (( SECONDS < deadline )); do
    status="$(docker inspect --format '{{.State.Status}}' "$container")"
    if [[ "$status" != running ]]; then
      echo "::error::$container is $status, not running." >&2
      return 1
    fi
    address="$(docker inspect --format "{{with index .NetworkSettings.Networks \"$network\"}}{{.IPAddress}}{{end}}" "$container")"
    if [[ -n "$address" ]] && curl --fail --silent --show-error --output /dev/null \
      --max-time 5 "http://$address:$port/healthz" 2> /dev/null; then
      echo "$container passed the health check."
      return 0
    fi
    sleep 2
  done
  echo "::error::$container did not pass the health check within $timeout seconds." >&2
  return 1
}

# Writes the reverse proxy route of the service and reloads the proxy when the
# route changed. Restores the previous route when the proxy rejects it.
write_route() {
  local site="$1" service="$2" port="$3" hostname="$4" proxy="$5"
  local route
  # `lb_try_duration` retries a failed connection for 10 seconds, so a request
  # that reaches the old container while it stops is sent to the new one.
  route="$(printf '%s\n' \
    "# Written by scripts/ci/droplet-deploy.sh for $service. Do not edit." \
    "$hostname {" \
    "	reverse_proxy $service:$port {" \
    "		lb_try_duration 10s" \
    "	}" \
    "}")"
  if [[ -f "$site" ]] && [[ "$(< "$site")" == "$route" ]]; then
    return 0
  fi
  local backup=""
  if [[ -f "$site" ]]; then
    backup="$site.previous"
    cp "$site" "$backup"
  fi
  mkdir -p "$(dirname "$site")"
  printf '%s\n' "$route" > "$site"
  if docker exec "$proxy" caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile; then
    echo "Reloaded the reverse proxy with the route https://$hostname -> $service:$port"
    return 0
  fi
  if [[ -n "$backup" ]]; then
    mv "$backup" "$site"
  else
    rm -f "$site"
  fi
  return 1
}

main "$@" < /dev/null
