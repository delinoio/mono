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
# A second lock file `state/.proxy.lock` serializes the route change and the
# proxy reload of every service: a reload applies the whole shared Caddyfile,
# so two runs that interleave could restore a route that another run replaced.
#
# Steps: read a fresh deployment identifier, pull the image by digest, start a
# new container next to the running one under its own unique network alias (it
# does not join the alias of the running one), wait until `GET /healthz` returns a success status, write the
# reverse proxy route to the new alias and reload the proxy, and only then
# stop the old container. The proxy route selects the old container until the
# reload succeeds, so a container that is starting or unhealthy never receives
# a request. A failure before the proxy reload removes the new container, so
# the old one keeps serving. After the reload the replacement is the deployed
# image: the script records it in the state files first, and then stops and
# removes the old containers and the images that they ran (a different image
# than the replacement). A failed stop or removal makes the script exit
# non-zero and name the container, but the state files already name the
# replacement. A failed image removal is only logged. The exit code is the deployment verdict.
#
# The first Accounts API deploy with legacy images uses a separate drain
# invocation. It stops and removes all old containers before a compatible
# replacement is eligible to start. See the Accounts API cutover contract.
#
# A service that publishes UDP host ports cannot run next to its replacement,
# because only one container can publish a host port. For such a service the
# script creates the replacement, stops the running containers to free the
# ports, and only then starts the replacement and waits for its health check.
# The service does not answer between the stop and the route switch. When the
# replacement fails to start, fails its health check or Caddy rejects the
# route, the script removes it and restarts the containers stopped for UDP.
# An unresolved route switch keeps the replacement running and the prior
# containers stopped, so only the replacement can hold the UDP ports.
#
# On `SIGTERM`, `SIGINT` or `SIGHUP`, the script cleans up an HTTP-only
# replacement from launch through proxy-lock acquisition. For UDP, the same
# handler runs from the first stop through proxy-lock acquisition and also
# restarts the stopped containers. The handler holds the service lock and
# exits with 128 plus the signal number. Bash runs the trap after the
# foreground command returns. A Docker stop, `curl` or `flock` can wait
# up to its configured timeout. `SIGKILL` cannot be caught and can leave
# the UDP containers stopped. For both HTTP-only and UDP deployments,
# the script defers these signals during the route switch and image-state
# publication. When the reload succeeds, the script records the state and
# exits with the signal status before cleanup. When Caddy rejects the route,
# the script restores the route file, removes the replacement and restarts
# the containers it stopped for UDP. An unresolved switch retains the
# installed route and backup and exits without publishing state or cleanup.
#
# Every deployment reads a fresh identifier under the service lock, before it
# runs any `docker` command: a UUID that the kernel generates for each read
# (`/proc/sys/kernel/random/uuid`), without its hyphens (32 lowercase hex
# characters). The container name is `<service>-<UTC timestamp>-<last 12 digest
# characters>-<identifier>` and the network alias is the first 30 characters of
# the service name (without a trailing hyphen), a hyphen and the identifier, so
# the alias is a DNS label of at most 63 characters. The clock second and the
# digest do not make a name unique: a retry of the same image within the same
# second would otherwise get the name of the serving container, and so would
# a service with the same long name prefix on the shared network. When the
# identifier cannot be read or is not 32 hex characters, the script fails before
# it changes a container, the route or the state files. As a second guard, it
# fails the same way when a container with the new name already exists, so that
# the cleanup of a failed start, which removes a container by name, can only
# ever remove a container that this run created.
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
#   DROPLET_ID_SOURCE       file that yields a fresh UUID on every read (default
#                           /proc/sys/kernel/random/uuid). Tests replace it.
#   DROPLET_HEALTH_TIMEOUT  whole decimal seconds to wait for the health check,
#                           from 0 to 9223372036 (unset or empty: 120).
#                           Inspections, probes and retries share this deadline.
#   DROPLET_STOP_TIMEOUT    seconds between SIGTERM and SIGKILL. Accounts API
#                           defaults to 60 and SandRun API defaults to 80 for
#                           outgoing containers and failed replacements.
#                           Overrides for these services accept whole decimal
#                           seconds from the service default to 9223372036.
#                           Other services default to 30 for outgoing containers.
#   DROPLET_LOCK_TIMEOUT    seconds to wait for a running deployment of the
#                           same service to finish before the script gives up
#                           without a change (default 600).
#   DROPLET_PROXY_LOCK_TIMEOUT
#                           seconds to wait for another service to finish its
#                           route change and proxy reload before the script
#                           gives up without changing the route (default: the
#                           value of DROPLET_LOCK_TIMEOUT).
#   DROPLET_ACCOUNTS_CAP_CUTOVER
#                           set to drain for the one-time Accounts API drain.
#   DROPLET_ACCOUNTS_CAP_SAFE_MONTH
#                           UTC YYYY-MM approved after reviewing claim history
#                           following a completed drain. See the workflow contract.
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
  # Limit the capture before arithmetic and discard leading zeros so Bash
  # always reads the timeout as decimal.
  if [[ ! "$health_timeout" =~ ^0*([1-9][0-9]{0,9}|0)$ ]] ||
    (( BASH_REMATCH[1] > 9223372036 )); then
    fail "DROPLET_HEALTH_TIMEOUT must be a whole number of seconds from 0 to 9223372036."
  fi
  health_timeout="${BASH_REMATCH[1]}"
  local stop_timeout="${DROPLET_STOP_TIMEOUT:-30}"
  local replacement_stop_timeout=""
  local minimum_stop_timeout=""
  case "$service" in
    accounts-api) minimum_stop_timeout=60 ;;
    sandrun-api) minimum_stop_timeout=80 ;;
  esac
  if [[ -n "$minimum_stop_timeout" ]]; then
    stop_timeout="${DROPLET_STOP_TIMEOUT-$minimum_stop_timeout}"
    # Capture decimal digits without leading zeros for Bash and Docker. The
    # upper bound keeps Docker's conversion to a nanosecond duration in range.
    if [[ ! "$stop_timeout" =~ ^0*([1-9][0-9]{0,9})$ ]] ||
      (( BASH_REMATCH[1] < minimum_stop_timeout || BASH_REMATCH[1] > 9223372036 )); then
      fail "DROPLET_STOP_TIMEOUT for $service must be a whole number of seconds from $minimum_stop_timeout to 9223372036."
    fi
    stop_timeout="${BASH_REMATCH[1]}"
    replacement_stop_timeout="$stop_timeout"
  fi
  if ! command -v timeout > /dev/null || [[ ! -r /proc/uptime ]]; then
    fail "droplet-deploy.sh needs GNU coreutils timeout and /proc/uptime for the health-check deadline."
  fi
  if ! command -v python3 > /dev/null; then
    fail "droplet-deploy.sh needs python3 to read the active proxy configuration."
  fi
  local lock_timeout="${DROPLET_LOCK_TIMEOUT:-600}"
  local proxy_lock_timeout="${DROPLET_PROXY_LOCK_TIMEOUT:-$lock_timeout}"
  local id_source="${DROPLET_ID_SOURCE:-/proc/sys/kernel/random/uuid}"
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

  # The deployment identifier makes the container name and the network alias
  # unique. Read it before the first `docker` command, so that a failure
  # changes nothing. The redirect error of a missing file is silenced because
  # the message below names the file.
  local deployment_id=""
  # `read` returns non-zero for a last line without a newline but still sets
  # the variable, so a non-empty value counts as read.
  { [[ -r "$id_source" ]] && { IFS= read -r deployment_id < "$id_source" || [[ -n "$deployment_id" ]]; }; } 2> /dev/null ||
    fail "could not read the deployment identifier from $id_source; nothing was changed."
  deployment_id="${deployment_id//-/}"
  deployment_id="${deployment_id,,}"
  if [[ ! "$deployment_id" =~ ^[0-9a-f]{32}$ ]]; then
    fail "$id_source did not yield a UUID; nothing was changed."
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
  local new upstream
  new="$service-$(date -u +%Y%m%d%H%M%S)-${image: -12}-$deployment_id"
  upstream="$(upstream_alias "$service" "$deployment_id")"
  # The identifier makes a duplicate name practically impossible, but a failed
  # `docker run` or `docker create` is followed by a removal by name. Refuse
  # to start when the name is taken (a frozen identifier source, or a
  # container that was created by hand), so that the removal can only ever
  # name a container of this run. The caller holds the service lock, so no
  # other deployment of the service creates a container meanwhile.
  local taken
  taken="$(docker ps --all --quiet --filter "name=^/$new\$")" ||
    fail "could not check whether a container named $new exists; nothing was changed."
  if [[ -n "$taken" ]]; then
    fail "a container named $new already exists; nothing was changed."
  fi
  local sandrun_kind="" sandrun_status="" sandrun_cancel=""
  if [[ "$service" == sandrun-api ]]; then
    (( ${#publish[@]} == 0 )) || fail "SandRun rollback requires an HTTP-only deployment."
    sandrun_kind="$(sandrun_image_kind "$state.removal-images" "$image")" ||
      fail "SandRun target $image has no verified OwnSand removal capability; the route was not changed."
  fi
  local previous_serving="the previous container keeps serving"
  if [[ "$service" == accounts-api ]]; then
    accounts_cap_cutover "$state" "$image" "$stop_timeout" "${old[@]}"
    if (( ${#old[@]} == 0 )); then
      previous_serving="the service awaits a healthy compatible replacement"
    fi
  fi
  # Use the same inventory for the OwnSand preflight and UDP recovery.
  local -a running=()
  if (( ${#publish[@]} > 0 )) || [[ "$service" == ownsand-api ]]; then
    local running_ids running_id
    running_ids="$(docker ps --quiet --filter "label=io.delino.service=$service")" ||
      fail "could not list the running containers of $service; no replacement was started."
    while IFS= read -r running_id; do
      if [[ -n "$running_id" ]]; then
        running+=("$running_id")
      fi
    done <<< "$running_ids"
  fi
  if [[ "$service" == ownsand-api ]]; then
    ownsand_unlink_preflight "$state" "$image" "${running[@]}"
  fi
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
    # The name check above passed. Clean up this replacement if a signal
    # arrives during launch, the health check or the proxy lock wait.
    trap 'interrupted TERM "$new" "$replacement_stop_timeout"' TERM
    trap 'interrupted INT "$new" "$replacement_stop_timeout"' INT
    trap 'interrupted HUP "$new" "$replacement_stop_timeout"' HUP
    echo "Start $new as $upstream"
    # A failed `docker run` can leave a created container that did not start
    # (for example an OCI runtime or entrypoint error). `set -e` would end the
    # script before any cleanup, so the call is an `if` operand.
    if ! docker run --detach "${create[@]}" > /dev/null; then
      discard_replacement "$new" "$replacement_stop_timeout"
      fail "$new did not start; $previous_serving."
    fi
    if ! wait_healthy "$new" "$network" "$port" "$health_timeout"; then
      discard_replacement "$new" "$replacement_stop_timeout"
      fail "$new did not pass the health check; $previous_serving."
    fi
  else
    stopped=("${running[@]}")
    echo "Create $new as $upstream with the UDP ports $udp_ports"
    # No container is stopped yet, so the previous containers keep serving.
    if ! docker create "${create[@]}" > /dev/null; then
      discard_replacement "$new" "$replacement_stop_timeout"
      fail "$new was not created; the previous containers keep serving."
    fi
    # From the first stop until the route switch, a signal restores the
    # previous containers (see `interrupted`). Bash expands the quoted trap
    # text when the signal arrives, in the frame of the running command, so
    # `$new` and `stopped` are the values of this deployment.
    trap 'interrupted TERM "$new" "$replacement_stop_timeout" "${stopped[@]}"' TERM
    trap 'interrupted INT "$new" "$replacement_stop_timeout" "${stopped[@]}"' INT
    trap 'interrupted HUP "$new" "$replacement_stop_timeout" "${stopped[@]}"' HUP
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
      discard_replacement "$new" "$replacement_stop_timeout"
      restart_previous "${stopped[@]}"
      fail "$new did not start or did not pass the health check; the previous containers were started again."
    fi
  fi

  if [[ -n "$sandrun_kind" ]]; then
    local guard="$new" action=status
    local -a guard_args=()
    if [[ "$sandrun_kind" != guard ]]; then
      if ! guard="$(sandrun_rollback_driver "$sandrun_kind" "$new" "${old[@]}")"; then
        discard_replacement "$new" "$replacement_stop_timeout"
        fail "could not verify a running SandRun rollback guard, matching database settings and, for a pre-lease image, disabled legacy recovery; $previous_serving."
      fi
    fi
    if [[ "$sandrun_kind" == pre-lease ]]; then
      action=fence
      guard_args=("$deployment_id")
      # Arm cleanup before the database call: a lost reply can hide a commit.
      # Use a literal trap because errexit may unwind main before EXIT runs.
      printf -v sandrun_cancel 'sandrun_cancel_fence %q %q' "$guard" "$deployment_id"
      trap "$sandrun_cancel" EXIT
    fi
    if ! sandrun_status="$(sandrun_guard "$guard" "$action" "${guard_args[@]}")" ||
      [[ "$sandrun_kind" == pre-lease && "$sandrun_status" != ready ]] ||
      [[ "$sandrun_kind" == lease && "$sandrun_status" != open ]] ||
      [[ "$sandrun_kind" == guard && "$sandrun_status" != open && "$sandrun_status" != fenced ]]; then
      # These outcomes are reported only after the fence transaction aborts.
      if [[ "$sandrun_status" == tokens-remain || "$sandrun_status" == drain-incomplete ]]; then trap - EXIT; fi
      sandrun_guard_failure "$sandrun_status"
      discard_replacement "$new" "$replacement_stop_timeout"
      fail "the route of $service was not switched; $previous_serving."
    fi
  fi

  # Deployments and provisioning share the proxy lock because each switch
  # applies the whole Caddyfile. Take it after the service lock. Keep the
  # pre-switch traps active while waiting for it.
  local pending_signal="" route_result=1 proxy_lock_fd=""
  if acquire_proxy_lock "$root/state/.proxy.lock" "$proxy_lock_timeout" "$service" proxy_lock_fd; then
    trap 'pending_signal=TERM' TERM
    trap 'pending_signal=INT' INT
    trap 'pending_signal=HUP' HUP
    route_result=0
    # From the first possible route write, retain the fence unless Caddy's
    # result proves that the replacement did not take over.
    if [[ -n "$sandrun_cancel" ]]; then trap - EXIT; fi
    write_route "$site" "$service" "$upstream" "$port" "$hostname" "$proxy" "$protocol" "$deployment_id" ||
      route_result=$?
    if [[ -n "$sandrun_cancel" && "$route_result" == 1 ]]; then trap "$sandrun_cancel" EXIT; fi
    exec {proxy_lock_fd}>&-
  fi
  if (( route_result != 0 && route_result != 1 )); then
    fail "the route switch for $service is unresolved. Retained $new and the previous containers, the installed route and its backups. Image state and container cleanup were skipped. Retry the deployment or rollback after Caddy's admin API is available."
  fi
  if (( route_result == 1 )); then
    discard_replacement "$new" "$replacement_stop_timeout"
    if (( ${#stopped[@]} > 0 )); then
      restart_previous "${stopped[@]}"
      fail "the route of $service was not switched; the previous containers were started again."
    fi
    fail "the route of $service was not switched; $previous_serving."
  fi

  # The proxy routes to the replacement now, so it is the deployed image.
  # Record it before the cleanup: a failed cleanup must not leave the state
  # files naming the image that the route no longer serves.
  record_state "$state" "$image" || fail "the route of $service now serves $image, but the state files $state.current and $state.previous were not updated."
  trap - TERM INT HUP
  if [[ -n "$pending_signal" ]]; then
    local previous_status="any running previous containers keep running"
    if (( ${#publish[@]} > 0 )); then
      previous_status="the previous containers stay stopped"
    fi
    echo "::error::received SIG$pending_signal during the route switch or image-state publication. The route of $service serves $new, $previous_status, and cleanup was skipped. Deploy the same image again to finish the cleanup." >&2
    exit $((128 + $(signal_number "$pending_signal")))
  fi

  local -a failed=()
  # Image references of the removed old containers, and of the old containers
  # that are still there because their stop or removal failed.
  # An empty reference (an unreadable image) is never recorded; it is not a
  # valid array key.
  local -a superseded=()
  local -A in_use=() seen_image=()
  local id old_image
  for id in "${old[@]}"; do
    # Read the image before the removal. The reference is what the container
    # was created from (a digest reference for every deployment of this
    # script). A failed read only skips the image cleanup of this container.
    if ! old_image="$(docker inspect --format '{{.Config.Image}}' "$id")"; then
      echo "Could not read the image of the previous container $id; its image stays." >&2
      old_image=""
    fi
    echo "Stop the previous container $id"
    if ! docker stop --time "$stop_timeout" "$id" > /dev/null; then
      echo "::error::docker stop failed for the previous container $id." >&2
      failed+=("docker stop $id")
      if [[ -n "$old_image" ]]; then in_use[$old_image]=1; fi
      continue
    fi
    if ! docker rm "$id" > /dev/null; then
      echo "::error::docker rm failed for the previous container $id." >&2
      failed+=("docker rm $id")
      if [[ -n "$old_image" ]]; then in_use[$old_image]=1; fi
      continue
    fi
    if [[ -n "$old_image" && -z "${seen_image[$old_image]:-}" ]]; then
      seen_image[$old_image]=1
      superseded+=("$old_image")
    fi
  done
  # Removes the image of every removed old container, once per reference and
  # only when no old container that is still there uses it, and not when it is
  # the image of the replacement (a retry of the same image). The prune below
  # does not remove them: an image that was pulled by digest has no tag, and
  # the containerd image store (the default of a fresh Docker Engine 29) does
  # not count it as dangling. A failed removal is only logged: the replacement
  # already serves and the state files name it.
  for old_image in "${superseded[@]}"; do
    if [[ "$old_image" == "$image" || -n "${in_use[$old_image]:-}" ]]; then
      continue
    fi
    echo "Remove the previous image $old_image"
    if ! docker image rm "$old_image" > /dev/null; then
      echo "docker image rm failed for the previous image $old_image; it stays on the Droplet." >&2
    fi
  done
  # Removes images that no container uses any more and that are dangling. A
  # rollback pulls the previous image from the registry again.
  docker image prune --force > /dev/null || true
  if (( ${#failed[@]} > 0 )); then
    fail "the replacement $new already serves $service as $image, but the cleanup of the previous containers failed (${failed[*]}). Remove them by hand, or deploy the same image again to retry."
  fi
  if [[ "$sandrun_kind" == guard && "$sandrun_status" == fenced ]]; then
    if ! sandrun_guard "$new" resume > /dev/null; then
      fail "SandRun switched to $image, but could not reopen OwnSand teardown admission. Deploy the same image again."
    fi
  fi
  echo "$service runs $image behind https://$hostname"
}

fail() {
  echo "::error::$1" >&2
  exit 1
}

# Verify the pulled immutable reference (or an existing container's image
# ID). Builds before the label require a reviewed digest and capability.
sandrun_guard_image() {
  docker image inspect --format '{{index .Config.Labels "io.delino.sandrun.ownsand-removal"}}' "$1" 2>/dev/null
}

sandrun_image_kind() {
  local allowlist="$1" image="$2" capability
  capability="$(sandrun_guard_image "$image")" || return 1
  if [[ "$capability" == rollback-guard-v1 ]]; then
    echo guard
  elif [[ -z "$capability" && -f "$allowlist" ]]; then
    local pre_lease=0 lease=0
    if grep -Fxq -- "pre-lease $image" "$allowlist"; then pre_lease=1; fi
    if grep -Fxq -- "lease $image" "$allowlist"; then lease=1; fi
    case "$pre_lease:$lease" in
      1:0) echo pre-lease ;;
      0:1) echo lease ;;
      *) return 1 ;;
    esac
  else
    return 1
  fi
}

# Old binaries cannot run the guard. Use a running guard-capable container
# only after comparing its effective database settings with the target and
# every other running SandRun container. Keep settings inside the pipeline.
sandrun_rollback_driver() {
  local kind="$1" replacement="$2" id info status image_id capability driver=""
  shift 2
  local -a containers=("$replacement")
  for id in "$@"; do
    info="$(docker inspect --format '{{.State.Status}} {{.Image}}' "$id" 2>/dev/null)" || return 1
    read -r status image_id <<< "$info"
    case "$status" in
      exited|created|dead) continue ;;
      running) ;;
      *) return 1 ;;
    esac
    containers+=("$id")
    [[ "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || return 1
    capability="$(sandrun_guard_image "$image_id")" || return 1
    if [[ "$capability" == rollback-guard-v1 ]]; then driver="$id"; fi
  done
  [[ -n "$driver" ]] || return 1
  docker inspect --format '{{json .Config.Env}}' "${containers[@]}" 2>/dev/null |
    python3 -c '
import json
import sys

kind, count = sys.argv[1], int(sys.argv[2])
def database_settings(line):
    settings = {}
    legacy_key = "RECLAIM_LEGACY_OWNSAND_REMOVALS"
    for entry in json.loads(line):
        key, sep, value = entry.partition("=")
        if key in ("DATABASE_URL", legacy_key) or key.startswith("PG"):
            if not sep or key in settings:
                raise ValueError
            settings[key] = value
    if not settings.get("DATABASE_URL", "").strip():
        raise ValueError
    legacy = settings.pop(legacy_key, "").strip()
    if kind == "pre-lease" and legacy not in ("", "0", "f", "F", "false", "False", "FALSE"):
        raise ValueError
    return settings

try:
    configs = [database_settings(line) for line in sys.stdin]
    same_database = len(configs) == count and all(config == configs[0] for config in configs)
except (TypeError, ValueError, AttributeError):
    same_database = False
sys.exit(0 if same_database else 1)
' "$kind" "${#containers[@]}" ||
    return 1
  echo "$driver"
}

sandrun_guard() {
  local container="$1"
  shift
  timeout --signal=KILL 25s docker exec "$container" /usr/local/bin/sandrun-api ownsand-rollback "$@" 2>/dev/null
}

sandrun_cancel_fence() {
  if ! sandrun_guard "$1" cancel "$2" > /dev/null; then
    echo "::error::could not confirm that SandRun teardown admission was restored. Deploy a guard-capable image to recover." >&2
  fi
}

sandrun_guard_failure() {
  case "$1" in
    tokens-remain)
      echo "::error::SandRun rollback blocked by an unfinished OwnSand removal (live or expired). Retry removal on a lease-aware image." >&2 ;;
    drain-incomplete)
      echo "::error::SandRun rollback could not drain in-flight OwnSand removal claim writers." >&2 ;;
    fenced)
      echo "::error::SandRun teardown admission is fenced. Deploy a guard-capable image to reopen it." >&2 ;;
    *)
      echo "::error::SandRun could not verify OwnSand removal state in the target database." >&2 ;;
  esac
}

# An unlabelled build needs an exact registry digest reviewed by an operator.
# For a running container, bind that digest to the actual image ID before
# trusting it. A container's configured image reference alone is insufficient.
ownsand_unlink_image() {
  local allowlist="$1" reference="$2" image="$3" capability reviewed_id
  capability="$(docker image inspect --format '{{index .Config.Labels "io.delino.ownsand.pending-unlinks"}}' "$image")" ||
    return 1
  if [[ "$capability" == nullable-account-v1 ]]; then
    return 0
  fi
  [[ -z "$capability" && "$reference" =~ ^[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64}$ && -f "$allowlist" ]] &&
    grep -Fxq -- "$reference" "$allowlist" || return 1
  if [[ "$reference" != "$image" ]]; then
    reviewed_id="$(docker image inspect --format '{{.Id}}' "$reference")" || return 1
    [[ "$reviewed_id" == "$image" ]] || return 1
  fi
}

# Even a failed replacement can apply migration 0012 and write NULL account
# IDs. Check the target and every running fallback before creating a
# replacement or stopping a container. A readiness check cannot detect an
# older worker that repeatedly selects rows it cannot scan.
ownsand_unlink_preflight() {
  local state="$1" target="$2"
  shift 2
  local allowlist="$state.pending-unlink-compatible-images"
  local id details reference image_id
  ownsand_unlink_image "$allowlist" "$target" "$target" ||
    fail "OwnSand target $target has no verified nullable pending unlink support; no replacement was started."
  for id in "$@"; do
    details="$(docker inspect --format '{{.Config.Image}} {{.Image}}' "$id")" ||
      fail "could not inspect OwnSand fallback container $id; no replacement was started."
    read -r reference image_id <<< "$details"
    if [[ ! "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]] ||
      ! ownsand_unlink_image "$allowlist" "$reference" "$image_id"; then
      fail "OwnSand fallback container $id ($reference) has no verified nullable pending unlink support; no replacement was started."
    fi
  done
}

# Inspect the actual image ID of existing containers. The optional allowlist
# records exact registry digests for verified builds that predate the label.
accounts_cap_image() {
  local allowlist="$1" reference="$2" image_id="$3" capability
  capability="$(docker image inspect --format '{{index .Config.Labels "io.delino.accounts.cap-accounting"}}' "$image_id")" ||
    return 1
  [[ "$capability" == claim-time ]] ||
    { [[ -z "$capability" && "$reference" =~ ^[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64}$ && -f "$allowlist" ]] &&
      grep -Fxq -- "$reference" "$allowlist"; }
}

write_accounts_cap_state() {
  local path="$1" value="$2"
  if ! printf '%s\n' "$value" > "$path.new" || ! mv -fT "$path.new" "$path"; then
    fail "could not record the Accounts API cap cutover in $path."
  fi
}

# The caller holds the service lock from inventory through replacement startup.
# Drain is a separate invocation so an operator can review claims after all
# legacy processes exit. Record activation before a worker can make a claim,
# even if startup or the subsequent proxy switch fails.
accounts_cap_cutover() {
  local state="$1" image="$2" stop_timeout="$3"
  shift 3
  local path="$state.cap-accounting" allowlist="$state.cap-compatible-images"
  local mode="${DROPLET_ACCOUNTS_CAP_CUTOVER:-}" safe_month="${DROPLET_ACCOUNTS_CAP_SAFE_MONTH:-}"
  local month_pattern='[0-9]{4}-(0[1-9]|1[0-2])' status="" id reference image_id inventory month
  if [[ -e "$path" || -L "$path" ]]; then
    if [[ ! -f "$path" || -L "$path" ]] || ! status="$(cat "$path")" ||
      [[ "$status" != active && "$status" != draining && ! "$status" =~ ^drained\ $month_pattern$ ]]; then
      fail "could not verify the Accounts API cap cutover state in $path."
    fi
  fi
  if ! accounts_cap_image "$allowlist" "$image" "$image"; then
    fail "Accounts API image $image has no verified claim-time cap capability; deploy a compatible image."
  fi
  if [[ "$mode" == drain ]]; then
    [[ "$status" != active && -z "$safe_month" ]] ||
      fail "Accounts API drain is only for first activation. Review claim history after the drain."
    if [[ "$status" == 'drained '* && $# == 0 ]]; then
      echo "Accounts API containers are already drained (${status#drained } UTC)."
      exit 0
    fi
    write_accounts_cap_state "$path" draining
    for id in "$@"; do
      echo "Drain Accounts API container $id"
      if ! docker stop --time "$stop_timeout" "$id" > /dev/null || ! docker rm "$id" > /dev/null; then
        fail "could not confirm that Accounts API container $id stopped and was removed; no replacement was started."
      fi
    done
    if ! inventory="$(docker ps --all --quiet --filter 'label=io.delino.service=accounts-api')" || [[ -n "$inventory" ]]; then
      fail "could not confirm that all Accounts API containers were removed; no replacement was started."
    fi
    month="$(date -u +%Y-%m)"
    [[ "$month" =~ ^$month_pattern$ ]] || fail "could not read the UTC month for the Accounts API cap cutover."
    write_accounts_cap_state "$path" "drained $month"
    echo "Accounts API containers drained in $month UTC. Deploy after the next UTC month begins or after reviewing this month's claims."
    exit 0
  fi
  [[ -z "$mode" ]] || fail "DROPLET_ACCOUNTS_CAP_CUTOVER must be unset or drain."

  case "$status" in
    draining)
      fail "the Accounts API drain is incomplete; rerun the drain before deploying."
      ;;
    'drained '*)
      (( $# == 0 )) || fail "Accounts API containers appeared after the drain; rerun the drain before deploying."
      month="$(date -u +%Y-%m)"
      [[ "$month" =~ ^$month_pattern$ ]] || fail "could not read the UTC month for the Accounts API cap cutover."
      if [[ ! "$month" > "${status#drained }" && ( "$month" != "${status#drained }" || "$safe_month" != "$month" ) ]]; then
        fail "Accounts API was drained in ${status#drained } UTC; wait for a new UTC month or confirm the current month's legacy claim accounting."
      fi
      ;;
    *)
      [[ -n "$status" || $# != 0 ]] ||
        fail "Accounts API has no verified prior deployment; run the Accounts API drain before deploying."
      for id in "$@"; do
        if ! inventory="$(docker inspect --format '{{.Config.Image}} {{.Image}}' "$id")" ||
          ! read -r reference image_id <<< "$inventory" ||
          [[ ! "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]] ||
          ! accounts_cap_image "$allowlist" "$reference" "$image_id"; then
          fail "Accounts API container $id has no verified claim-time cap capability; follow the Accounts API cutover procedure before deploying."
        fi
      done
      ;;
  esac
  if [[ "$status" != active ]]; then
    write_accounts_cap_state "$path" active
  fi
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

# Prints the number of a signal name that the deploy script traps.
signal_number() {
  case "$1" in
    HUP) echo 1 ;;
    INT) echo 2 ;;
    *) echo 15 ;;
  esac
}

# Handles signals before the route switch. The arguments are the signal name,
# the replacement, its stop timeout and, for UDP, the containers that were
# running. The handler ignores further signals while it discards the
# replacement and restarts any previous containers passed by the caller.
# Starting a container that already runs is a no-op, so recovery also works
# partway through the UDP stop loop. The handler exits with 128 plus the
# signal number.
interrupted() {
  local name="$1" replacement="$2" stop_timeout="$3"
  shift 3
  trap '' TERM INT HUP
  echo "::error::received SIG$name before the route of the service switched; cleaning up the replacement $replacement." >&2
  discard_replacement "$replacement" "$stop_timeout"
  restart_previous "$@"
  exit $((128 + $(signal_number "$name")))
}

# Records the image that the route serves: `<state>.current` becomes the image
# and `<state>.previous` the image that `<state>.current` held. A retry with
# the same image keeps `<state>.previous`, so a rollback still finds the image
# that served before the first attempt. An empty or missing `<state>.current`
# (a write that an interrupted earlier attempt left behind) names no image, so
# it never replaces `<state>.previous`. `<state>.current` is published through
# a temporary file and a rename, so an interruption leaves either the old or
# the new file and never an empty one. The caller evaluates this function as an
# `if` or `||` operand, which disables `set -e` inside it, so every step checks
# its own status.
record_state() {
  local state="$1" image="$2" current=""
  mkdir -p "$(dirname "$state")" || return 1
  if [[ -f "$state.current" ]]; then
    current="$(< "$state.current")" || return 1
  fi
  if [[ -n "$current" && "$current" != "$image" ]]; then
    cp -f "$state.current" "$state.previous.new" || return 1
    mv -fT -- "$state.previous.new" "$state.previous" || return 1
  fi
  echo "$image" > "$state.current.new" || return 1
  mv -fT -- "$state.current.new" "$state.current" || return 1
}

# Prints the unique network alias of a replacement container. The alias is a
# DNS label, which has at most 63 characters, but a service name can use all of
# them. The alias keeps the first 30 characters of the service name (without a
# trailing hyphen) and appends a hyphen and the 32-character deployment
# identifier, so it is at most 63 characters long and differs in every
# deployment, also between services with the same name prefix.
upstream_alias() {
  local prefix="${1:0:30}" id="$2"
  while [[ "$prefix" == *- ]]; do
    prefix="${prefix%-}"
  done
  echo "$prefix-$id"
}

# Prints the log tail of a replacement that failed, then removes it. A failed
# log read or removal must not hide the deployment failure that the caller
# reports. Return zero explicitly on early returns because a trap can call
# this function after a negated command and inherit its nonzero status.
discard_replacement() {
  local replacement="$1" stop_timeout="$2"
  echo "Last log lines of $replacement:"
  docker logs --tail 200 "$replacement" 2>&1 || true
  if [[ -z "$stop_timeout" ]]; then
    docker rm --force "$replacement" > /dev/null || true
    return 0
  fi
  # Accounts and SandRun can start background workers before they serve
  # traffic. If graceful stop fails, leave the container for cleanup.
  if ! docker stop --time "$stop_timeout" "$replacement" > /dev/null; then
    echo "::error::docker stop failed for the replacement $replacement; it was not removed." >&2
    return 0
  fi
  if ! docker rm "$replacement" > /dev/null; then
    echo "::error::docker rm failed for the replacement $replacement." >&2
  fi
}

# Linux reports uptime in hundredths of a second, independent of wall-clock
# changes. Read the same clock for the deadline and every remaining budget.
health_clock() {
  local uptime
  IFS=' ' read -r uptime _ < /proc/uptime || return 1
  printf '%d\n' "$((10#${uptime%.*} * 100 + 10#${uptime#*.}))"
}

# Updates the remaining time in the caller's wait_healthy frame.
health_budget() {
  local now
  now="$(health_clock)" || return 1
  remaining=$((deadline - now))
  (( remaining > 0 ))
}

run_with_health_limit() {
  local duration
  printf -v duration '%d.%02ds' "$(($1 / 100))" "$(($1 % 100))"
  shift
  # Kill the command's process group at the deadline, even if it ignores TERM.
  LC_ALL=C timeout --signal=KILL "$duration" "$@"
}

# Waits for a 2xx response to GET /healthz before one shared deadline.
# The probe connects directly to the bridge address and bypasses host proxies.
# Inspections and retry pauses also use the remaining time. The probe retries
# redirects, other non-2xx responses and transport failures. A container that
# is no longer running fails immediately.
wait_healthy() {
  local container="$1" network="$2" port="$3" timeout="$4"
  local deadline remaining status address code probe_budget
  deadline="$(health_clock)" || return 1
  deadline=$((deadline + timeout * 100))
  while health_budget; do
    status="$(run_with_health_limit "$remaining" docker inspect --format '{{.State.Status}}' "$container")" ||
      status="unavailable"
    health_budget || break
    if [[ "$status" != running ]]; then
      echo "::error::$container is $status, not running." >&2
      return 1
    fi
    address="$(run_with_health_limit "$remaining" docker inspect --format "{{with index .NetworkSettings.Networks \"$network\"}}{{.IPAddress}}{{end}}" "$container")" ||
      address=""
    health_budget || break
    if [[ -n "$address" ]]; then
      probe_budget=$((remaining < 500 ? remaining : 500))
      if code="$(run_with_health_limit "$probe_budget" curl --silent --show-error --noproxy '*' --output /dev/null \
        --write-out '%{http_code}' --max-time 5 "http://$address:$port/healthz" 2> /dev/null)" &&
        health_budget && [[ "$code" =~ ^2[0-9]{2}$ ]]; then
        echo "$container passed the health check."
        return 0
      fi
    fi
    health_budget || break
    run_with_health_limit "$remaining" sleep 2 2> /dev/null || break
  done
  echo "::error::$container did not pass the health check within $timeout seconds." >&2
  return 1
}

# Takes the lock that serializes the route changes and proxy reloads of all
# services (see the call in `main`). `$1` is the lock file, `$2` the timeout in
# seconds, `$3` the service name for the message and `$4` the name of the
# variable that receives the file descriptor, which the caller closes to
# release the lock. The kernel also releases it when the script exits. The
# caller evaluates this function as an `if` condition, so every step checks
# its own status. Changes no route file.
acquire_proxy_lock() {
  local lock_file="$1" timeout="$2" service="$3" fd
  if ! { exec {fd}> "$lock_file"; } 2> /dev/null; then
    echo "::error::could not open the proxy lock file $lock_file." >&2
    return 1
  fi
  if ! flock --wait "$timeout" "$fd"; then
    echo "::error::another deployment held the proxy lock $lock_file for $timeout seconds; the route of $service was not changed. Run the deployment again when it has finished." >&2
    exec {fd}>&-
    return 1
  fi
  printf -v "$4" '%s' "$fd"
}

# Caddy's loopback API is available inside the pinned proxy container. Bound
# both the Docker command and the HTTP request. A timed-out write is ambiguous.
proxy_admin() {
  local proxy="$1" interactive="$2"
  shift 2
  local -a exec_args=()
  if [[ "$interactive" == yes ]]; then exec_args=(-i); fi
  timeout --signal=KILL 25s docker exec "${exec_args[@]}" "$proxy" curl --disable \
    --silent --show-error --noproxy '*' --max-time 20 "$@" http://127.0.0.1:2019/config/
}

# Match the exact site and its single upstream in the Caddyfile adapter's
# host -> subroute -> reverse_proxy structure. An unfamiliar shape cannot
# prove that this deployment is serving.
config_serves() {
  local config="$1" hostname="$2" dial="$3"
  python3 -c '
import json
import sys

hostname, dial = sys.argv[1:]
try:
    servers = json.load(sys.stdin)["apps"]["http"]["servers"]
    sites = [
        route
        for server in servers.values()
        for route in server.get("routes", [])
        if route.get("match") == [{"host": [hostname]}]
    ]
    [site] = sites
    [subroute] = site["handle"]
    [route] = subroute["routes"]
    [proxy] = route["handle"]
    [upstream] = proxy["upstreams"]
    serving = (
        subroute["handler"] == "subroute"
        and not route.get("match")
        and proxy["handler"] == "reverse_proxy"
        and upstream["dial"] == dial
    )
except (KeyError, TypeError, ValueError, AttributeError):
    serving = False
sys.exit(0 if serving else 1)
' "$hostname" "$dial" <<< "$config"
}

# Switches the route under the proxy lock. Status 0 confirms the replacement,
# 1 permits cleanup of a rejected replacement, and 2 retains it for recovery.
# The caller uses this function in a conditional, so check each operation.
write_route() {
  local site="$1" service="$2" upstream="$3" port="$4" hostname="$5" proxy="$6" protocol="$7" deployment_id="$8"
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
    backup="$site.previous.$deployment_id"
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
  local candidate http_status active etag etag_pattern='^"/config/ [0-9a-f]+"$'
  # Adapt while the proxy lock is held and pass the resulting JSON by value.
  # A delayed Docker exec must not read a later deployment's route files.
  if ! candidate="$(timeout --signal=KILL 25s docker exec "$proxy" caddy adapt --config /etc/caddy/Caddyfile --adapter caddyfile)" ||
    ! config_serves "$candidate" "$hostname" "$upstream:$port"; then
    echo "::error::the adapted Caddy configuration does not select $upstream:$port for $hostname." >&2
  # Caddy checks the tag under its config lock before applying the POST.
  # Each successful switch uses a unique alias, which changes the tag and
  # prevents a delayed load from bringing back a removed upstream.
  elif ! etag="$(proxy_admin "$proxy" no --fail --output /dev/null --write-out '%header{etag}')" ||
    [[ ! "$etag" =~ $etag_pattern ]]; then
    echo "::error::could not read the current Caddy configuration tag for $service." >&2
  else
    http_status="$(proxy_admin "$proxy" yes --header "If-Match: $etag" \
      --header 'Content-Type: application/json' \
      --header 'Caddy-Config-Source-File: /etc/caddy/Caddyfile' \
      --header 'Caddy-Config-Source-Adapter: caddyfile' \
      --data-binary @- --output /dev/stderr --write-out '%{http_code}' <<< "$candidate")" || http_status=""
    case "$http_status" in
      200)
        echo "Reloaded the reverse proxy with the route https://$hostname -> $upstream:$port"
        ;;
      400|412|500)
        # An explicit error response from the loopback API completes this load.
        ;;
      *)
        if ! active="$(proxy_admin "$proxy" no --fail)" ||
          ! config_serves "$active" "$hostname" "$upstream:$port"; then
          # The previous upstream could still be visible before the POST applies.
          # Preserve both deployments until a later conditional switch succeeds.
          return 2
        fi
        echo "Confirmed the active reverse proxy route https://$hostname -> $upstream:$port"
        http_status=200
        ;;
    esac
    if [[ "$http_status" == 200 ]]; then
      rm -f -- "$site".previous.* || echo "Could not remove prior route backups for $service." >&2
      return 0
    fi
  fi

  echo "::error::the reverse proxy rejected the route of $service; restoring the previous route." >&2
  if [[ -n "$backup" ]]; then
    if ! mv -f "$backup" "$site"; then
      echo "::error::could not restore the previous route from $backup to $site." >&2
      return 2
    fi
  elif ! rm -f "$site"; then
    echo "::error::could not remove the rejected route $site." >&2
    return 2
  fi
  return 1
}

main "$@" < /dev/null
