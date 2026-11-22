#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Replaces the container of one server on the shared DigitalOcean Droplet.
# `deploy-targets.sh servers` runs this script on the Droplet as the `deploy`
# user over SSH, with the script on standard input:
#
#   ssh <droplet> bash -s -- <service> <image> <port> <hostname> \
#     <upstream-protocol> <udp-ports> < droplet-deploy.sh
#
# A maintainer rolls a service back with the same command, the image of the
# previous deployment (`/srv/delino/state/<service>.previous`) and the other
# values from the `deploy` field of the server's package.json.
#
# A lock file `state/<service>.lock` (`flock`) serializes deployments and
# rollbacks of one service: a second run waits until the first one ends.
#
# Steps: pull the image by digest, start a new container next to the running
# one under its own unique network alias (it does not join the alias of the
# running one), wait until `GET /healthz` returns a success status, write the
# reverse proxy route to the new alias and reload the proxy, and only then
# stop the old container. The proxy route selects the old container until the
# reload succeeds, so a container that is starting or unhealthy never receives
# a request. A failure before the proxy reload removes the new container, so
# the old one keeps serving. After the reload the replacement is the deployed
# image: the script records it in the state files first, and then stops and
# removes the old containers. A failed stop or removal makes the script exit
# non-zero and name the container, but the state files already name the
# replacement. The exit code is the deployment verdict.
#
# A service that publishes UDP host ports cannot run next to its replacement,
# because only one container can publish a host port. For such a service the
# script creates the replacement, stops the running containers to free the
# ports, and only then starts the replacement and waits for its health check.
# The service does not answer between the stop and the route switch. When the
# replacement fails to start, fails its health check or the route switch
# fails, the script removes it and starts the stopped containers again; the
# route still names them.
#
# Arguments:
#   service   Droplet service name: container name prefix, label value and
#             env file name. A deployment by an earlier version of this
#             script also used it as the network alias of the reverse proxy
#             upstream; the first deployment with this version switches the
#             route away from that alias.
#   image     image reference pinned by digest (`<repository>@sha256:<hex>`).
#   port      port that the server listens on inside the container. The
#             script sets `ADDR=:<port>`.
#   hostname  public host name that the reverse proxy routes to the service.
#   upstream-protocol
#             protocol from the reverse proxy to the container: `http1`
#             (HTTP/1.1) or `h2c` (HTTP/2 without TLS, for bidirectional
#             streams).
#   udp-ports UDP ports that the Droplet publishes on the same host ports, as
#             a comma-separated list, or `none`.
#
# Environment (defaults match `scripts/droplet/provision.sh`):
#   DROPLET_ROOT            state directory (default /srv/delino).
#   DROPLET_NETWORK         Docker network of the proxy and the servers
#                           (default delino).
#   DROPLET_PROXY           reverse proxy container name (default caddy).
#   DROPLET_HEALTH_TIMEOUT  seconds to wait for the health check (default 120).
#   DROPLET_STOP_TIMEOUT    seconds between SIGTERM and SIGKILL when the old
#                           container stops (default 30).
#   DROPLET_LOCK_TIMEOUT    seconds to wait for a running deployment of the
#                           same service to finish before the script gives up
#                           without a change (default 600).
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
  if ! command -v flock > /dev/null; then
    echo "::error::droplet-deploy.sh needs flock (util-linux) for the deployment lock." >&2
    exit 2
  fi
  if (( $# != 6 )); then
    echo "usage: droplet-deploy.sh <service> <image> <port> <hostname> <upstream-protocol> <udp-ports>" >&2
    exit 2
  fi
  local service="$1" image="$2" port="$3" hostname="$4" protocol="$5" udp_ports="$6"
  # The planner already checks these values; check them again, because they
  # become file names, container names and proxy configuration here.
  # A DNS label has at most 63 characters, as in the planner's host check.
  local label='[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?'
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
  if [[ "$protocol" != http1 && "$protocol" != h2c ]]; then
    fail "the upstream protocol ${protocol@Q} is not http1 or h2c."
  fi
  # Each UDP port is published on the same host port: `--publish <p>:<p>/udp`.
  local -a publish=()
  if [[ "$udp_ports" != none ]]; then
    if [[ ! "$udp_ports" =~ ^[0-9]+(,[0-9]+)*$ ]]; then
      fail "the UDP ports ${udp_ports@Q} are not a comma-separated list of ports or none."
    fi
    local -A seen=()
    local udp_port
    local -a udp_list
    IFS=, read -r -a udp_list <<< "$udp_ports"
    for udp_port in "${udp_list[@]}"; do
      if [[ ! "$udp_port" =~ ^[1-9][0-9]{3,4}$ ]] || (( udp_port < 1024 || udp_port > 65535 )); then
        fail "the UDP port ${udp_port@Q} is not a number from 1024 to 65535."
      fi
      if [[ -n "${seen[$udp_port]:-}" ]]; then
        fail "the UDP port $udp_port is listed twice."
      fi
      seen[$udp_port]=1
      publish+=(--publish "$udp_port:$udp_port/udp")
    done
  fi

  local root="${DROPLET_ROOT:-/srv/delino}"
  local network="${DROPLET_NETWORK:-delino}"
  local proxy="${DROPLET_PROXY:-caddy}"
  local health_timeout="${DROPLET_HEALTH_TIMEOUT:-120}"
  local stop_timeout="${DROPLET_STOP_TIMEOUT:-30}"
  local lock_timeout="${DROPLET_LOCK_TIMEOUT:-600}"
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

  # One deployment of a service at a time. A deployment from the workflow and
  # a rollback that a maintainer runs by hand both run this script on the
  # Droplet, so they take the same lock. Without it, one run could stop and
  # remove the container that the route of the other run names, and both runs
  # would write the same route staging and backup files. The lock belongs to
  # the open file description of the file descriptor below and the kernel
  # releases it when the script exits, however it ends.
  mkdir -p "$root/state" || fail "could not create the state directory $root/state."
  local lock_fd
  { exec {lock_fd}> "$state.lock"; } 2> /dev/null || fail "could not open the lock file $state.lock."
  if ! flock --wait "$lock_timeout" "$lock_fd"; then
    fail "another deployment of $service held $state.lock for $lock_timeout seconds; nothing was changed. Run the deployment again when it has finished."
  fi

  echo "Pull $image"
  docker pull --quiet "$image"

  # Read the list into a variable first: a failure inside a process
  # substitution would not stop the script, and the deployment would then
  # leave the previous containers running next to the new one.
  local old_ids
  old_ids="$(docker ps --all --quiet --filter "label=io.delino.service=$service")" ||
    fail "could not list the existing containers of $service; no replacement was started."
  local -a old=()
  local old_id
  while IFS= read -r old_id; do
    if [[ -n "$old_id" ]]; then
      old+=("$old_id")
    fi
  done <<< "$old_ids"
  local stamp new upstream
  stamp="$(date -u +%Y%m%d%H%M%S)-${image: -12}"
  new="$service-$stamp"
  upstream="$(upstream_alias "$service" "$stamp")"
  # The replacement takes no `--network-alias "$service"`. The running
  # container keeps that alias (and the route still names it), and Docker DNS
  # would hand out the replacement address with the shared alias while the
  # replacement still starts or fails its health check. The route switches to
  # the unique alias only after the health check passed.
  local -a create=(--name "$new"
    --network "$network" --network-alias "$upstream"
    --label "io.delino.service=$service" --label "io.delino.image=$image"
    --restart unless-stopped
    --env-file "$env_file" --env "ADDR=:$port"
    "${publish[@]}"
    "$image")
  # Running containers that the script stops before the replacement starts
  # (only for a service with UDP ports), and starts again when the
  # replacement fails.
  local -a stopped=()
  if (( ${#publish[@]} == 0 )); then
    echo "Start $new as $upstream"
    # A failed `docker run` can leave a created container that did not start
    # (for example an OCI runtime or entrypoint error). `set -e` would end the
    # script before any cleanup, so the call is an `if` operand.
    if ! docker run --detach "${create[@]}" > /dev/null; then
      discard_replacement "$new"
      fail "$new did not start; the previous container keeps serving."
    fi
    if ! wait_healthy "$new" "$network" "$port" "$health_timeout"; then
      discard_replacement "$new"
      fail "$new did not pass the health check; the previous container keeps serving."
    fi
  else
    local running_ids
    running_ids="$(docker ps --quiet --filter "label=io.delino.service=$service")" ||
      fail "could not list the running containers of $service; no replacement was started."
    local running_id
    while IFS= read -r running_id; do
      if [[ -n "$running_id" ]]; then
        stopped+=("$running_id")
      fi
    done <<< "$running_ids"
    echo "Create $new as $upstream with the UDP ports $udp_ports"
    # No container is stopped yet, so the previous containers keep serving.
    if ! docker create "${create[@]}" > /dev/null; then
      discard_replacement "$new"
      fail "$new was not created; the previous containers keep serving."
    fi
    for running_id in "${stopped[@]}"; do
      echo "Stop the previous container $running_id to free the UDP ports"
      if ! docker stop --time "$stop_timeout" "$running_id" > /dev/null; then
        docker rm --force "$new" > /dev/null || true
        restart_previous "${stopped[@]}"
        fail "docker stop failed for the previous container $running_id; the replacement was removed and the previous containers were started again."
      fi
    done
    echo "Start $new"
    if ! docker start "$new" > /dev/null || ! wait_healthy "$new" "$network" "$port" "$health_timeout"; then
      discard_replacement "$new"
      restart_previous "${stopped[@]}"
      fail "$new did not start or did not pass the health check; the previous containers were started again."
    fi
  fi

  if ! write_route "$site" "$service" "$upstream" "$port" "$hostname" "$proxy" "$protocol"; then
    discard_replacement "$new"
    if (( ${#stopped[@]} > 0 )); then
      restart_previous "${stopped[@]}"
      fail "the route of $service was not switched; the previous containers were started again."
    fi
    fail "the route of $service was not switched; the previous container keeps serving."
  fi

  # The proxy routes to the replacement now, so it is the deployed image.
  # Record it before the cleanup: a failed cleanup must not leave the state
  # files naming the image that the route no longer serves.
  record_state "$state" "$image" || fail "the route of $service now serves $image, but the state files $state.current and $state.previous were not updated."

  local -a failed=()
  local id
  for id in "${old[@]}"; do
    echo "Stop the previous container $id"
    if ! docker stop --time "$stop_timeout" "$id" > /dev/null; then
      echo "::error::docker stop failed for the previous container $id." >&2
      failed+=("docker stop $id")
      continue
    fi
    if ! docker rm "$id" > /dev/null; then
      echo "::error::docker rm failed for the previous container $id." >&2
      failed+=("docker rm $id")
    fi
  done
  # Removes images that no container uses any more. A rollback pulls the
  # previous image from the registry again.
  docker image prune --force > /dev/null || true
  if (( ${#failed[@]} > 0 )); then
    fail "the replacement $new already serves $service as $image, but the cleanup of the previous containers failed (${failed[*]}). Remove them by hand, or deploy the same image again to retry."
  fi
  echo "$service runs $image behind https://$hostname"
}

fail() {
  echo "::error::$1" >&2
  exit 1
}

# Starts the previous containers again after a failed replacement of a
# service with UDP ports. The replacement is already removed, so the host
# ports are free. A container that does not start is named, and the caller
# still reports the deployment failure.
restart_previous() {
  local id
  for id in "$@"; do
    echo "Start the previous container $id again"
    if ! docker start "$id" > /dev/null; then
      echo "::error::docker start failed for the previous container $id; $id does not serve until it is started by hand." >&2
    fi
  done
}

# Records the image that the route serves: `<state>.current` becomes the image
# and `<state>.previous` the image that `<state>.current` held. A retry with
# the same image keeps `<state>.previous`, so a rollback still finds the image
# that served before the first attempt. The caller evaluates this function as
# an `if` or `||` operand, which disables `set -e` inside it, so every step
# checks its own status.
record_state() {
  local state="$1" image="$2"
  mkdir -p "$(dirname "$state")" || return 1
  if [[ -f "$state.current" ]] && [[ "$(< "$state.current")" != "$image" ]]; then
    mv -f "$state.current" "$state.previous" || return 1
  fi
  echo "$image" > "$state.current"
}

# Prints the unique network alias of a replacement container. The alias is a
# DNS label, which has at most 63 characters, but a service name can use all of
# them. The alias keeps the first 35 characters of the service name (without a
# trailing hyphen) and appends a hyphen and the 27-character stamp (a 14-digit
# timestamp, a hyphen and 12 digest characters), so it is at most 63
# characters long and differs in every deployment.
upstream_alias() {
  local prefix="${1:0:35}" stamp="$2"
  while [[ "$prefix" == *- ]]; do
    prefix="${prefix%-}"
  done
  echo "$prefix-$stamp"
}

# Prints the log tail of a replacement that failed, then removes it. A failed
# log read or removal must not hide the deployment failure that the caller
# reports.
discard_replacement() {
  echo "Last log lines of $1:"
  docker logs --tail 200 "$1" 2>&1 || true
  docker rm --force "$1" > /dev/null || true
}

# Waits until the container answers `GET /healthz` with a 2xx status. Redirects
# are not followed, so a 3xx answer is not healthy. A transport failure (no
# status) is retried until the deadline. Fails at once when the container stops.
wait_healthy() {
  local container="$1" network="$2" port="$3" timeout="$4"
  local deadline=$((SECONDS + timeout)) status address code
  while (( SECONDS < deadline )); do
    status="$(docker inspect --format '{{.State.Status}}' "$container")"
    if [[ "$status" != running ]]; then
      echo "::error::$container is $status, not running." >&2
      return 1
    fi
    address="$(docker inspect --format "{{with index .NetworkSettings.Networks \"$network\"}}{{.IPAddress}}{{end}}" "$container")"
    if [[ -n "$address" ]]; then
      code="$(curl --silent --show-error --output /dev/null \
        --write-out '%{http_code}' --max-time 5 \
        "http://$address:$port/healthz" 2> /dev/null)" || code=""
      if [[ "$code" =~ ^2[0-9]{2}$ ]]; then
        echo "$container passed the health check."
        return 0
      fi
    fi
    sleep 2
  done
  echo "::error::$container did not pass the health check within $timeout seconds." >&2
  return 1
}

# Writes the reverse proxy route of the service to the unique alias of its
# replacement container and reloads the proxy. The caller evaluates this function as an `if` condition, which
# disables `set -e` inside it, so every step checks its own status and names
# the failed operation. The previous route stays in place until the new route
# is complete (the new route goes to a temporary file that `mv` renames over
# the route file), and the proxy reloads only after that. Restores the
# previous route when the proxy rejects the new one.
write_route() {
  local site="$1" service="$2" upstream="$3" port="$4" hostname="$5" proxy="$6" protocol="$7"
  local route
  # `lb_try_duration` retries a failed connection for 10 seconds, so a request
  # that reaches the old container while it stops is sent to the new one.
  # `versions h2c` proxies HTTP/2 without TLS to the container, so that
  # bidirectional streams stay HTTP/2 from end to end. The route sets no
  # stream timeout, and a reload keeps the streams of the previous
  # configuration open. The server-wide 1-minute `read_body_idle` and
  # `write_idle` defaults of Caddy still apply (see the Droplet section of
  # docs/repository-workflow-contract.md).
  local -a transport=()
  if [[ "$protocol" == h2c ]]; then
    transport=("		transport http {" "			versions h2c" "		}")
  fi
  route="$(printf '%s\n' \
    "# Written by scripts/ci/droplet-deploy.sh for $service. Do not edit." \
    "$hostname {" \
    "	reverse_proxy $upstream:$port {" \
    "		lb_try_duration 10s" \
    "${transport[@]}" \
    "	}" \
    "}")"
  # The proxy imports `sites/*.caddy`; the suffixes below keep the backup and
  # the temporary file out of that glob.
  local backup="" pending="$site.new"
  if ! mkdir -p "$(dirname "$site")"; then
    echo "::error::could not create the directory of the route file $site." >&2
    return 1
  fi
  if [[ -f "$site" ]]; then
    backup="$site.previous"
    if ! cp "$site" "$backup"; then
      echo "::error::could not back up the route file $site to $backup." >&2
      return 1
    fi
  fi
  if ! printf '%s\n' "$route" > "$pending"; then
    echo "::error::could not write the new route to $pending." >&2
    rm -f "$pending"
    return 1
  fi
  if ! mv -f "$pending" "$site"; then
    echo "::error::could not install the new route as $site." >&2
    rm -f "$pending"
    return 1
  fi
  if docker exec "$proxy" caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile; then
    echo "Reloaded the reverse proxy with the route https://$hostname -> $upstream:$port"
    return 0
  fi
  echo "::error::the reverse proxy rejected the route of $service; restoring the previous route." >&2
  if [[ -n "$backup" ]]; then
    if ! mv -f "$backup" "$site"; then
      echo "::error::could not restore the previous route from $backup to $site." >&2
    fi
  elif ! rm -f "$site"; then
    echo "::error::could not remove the rejected route $site." >&2
  fi
  return 1
}

main "$@" < /dev/null
