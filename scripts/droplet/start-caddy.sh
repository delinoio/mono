#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Brings the `caddy` reverse proxy container of the shared DigitalOcean
# Droplet to the running state with the pinned image. `provision.sh` runs it
# as root after it writes the Caddyfile; it can run again at any time:
#
#   start-caddy.sh <image> <provisioning-root> <network>
#
# Arguments:
#   image   Caddy image reference pinned by digest.
#   root    provisioning root; `<root>/caddy` is mounted as /etc/caddy.
#   network Docker network that the container joins.
#
# Create Caddy with the pinned image and the `local` log driver. Recreate
# an existing container if its image or log driver differs. Recreation
# preserves the certificates in the `caddy-data` volume and can interrupt
# open connections. Obtain the image before removing the old container.
# Keep the old container if its settings cannot be read or the pull fails.
# When both settings match, start a stopped container or reload a running
# one. A container reads the current Caddyfile when it starts.
# After the create and start paths the script waits until the new Caddy
# process answers its admin API, because a successful `docker run` or
# `docker start` only means that Docker accepted the start: Caddy can still
# fail to load the Caddyfile or an imported route file and exit. The probe
# runs `wget` inside the container against http://127.0.0.1:2019/config/ (the
# loopback admin endpoint in the container's network namespace, so no port is
# published) and discards the body. Each attempt first reads the container
# state; a container that is exited, dead or restarting fails the script at
# once. The state reads, probes and retry delays share one 30-second
# deadline. On timeout the failure message reports the last known state
# and points to the Caddy logs. The reload path does not wait.
# The running process already answered, and `caddy reload` validates the
# new configuration itself.
#
# Environment (optional, for tests and slow hosts):
#   START_CADDY_READY_TIMEOUT   whole decimal seconds to wait for the admin API; default 30.
#   START_CADDY_READY_INTERVAL  seconds between attempts; default 1.
#
# A failing `docker pull`, `docker run`, `docker start` or `docker exec` fails
# the script with the Docker diagnostic on standard error.
set -euo pipefail

if (( BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4) )); then
  echo "error: start-caddy.sh needs Bash 4.4 or newer, not $BASH_VERSION." >&2
  exit 2
fi
if (( $# != 3 )); then
  echo "usage: start-caddy.sh <image> <provisioning-root> <network>" >&2
  exit 2
fi

caddy_image="$1"
root="$2"
network="$3"
# The network and bind mounts belong to the Droplet's system daemon.
unset DOCKER_CONTEXT
docker_command=(docker --host unix:///var/run/docker.sock)
logs_command="DOCKER_CONTEXT= ${docker_command[*]} logs caddy"
ready_timeout="${START_CADDY_READY_TIMEOUT:-30}"
ready_interval="${START_CADDY_READY_INTERVAL:-1}"
if [[ ! "$ready_timeout" =~ ^[0-9]+$ ]] || [[ ! "$ready_interval" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
  echo "error: START_CADDY_READY_TIMEOUT and START_CADDY_READY_INTERVAL must be numbers of seconds." >&2
  exit 2
fi

# Linux exposes elapsed time in hundredths of a second, independent of
# wall-clock adjustments.
readiness_clock() {
  local uptime
  IFS=' ' read -r uptime _ < /proc/uptime
  printf '%d\n' "$((10#${uptime%.*} * 100 + 10#${uptime#*.}))"
}

readiness_budget() {
  remaining=$((deadline - $(readiness_clock)))
  (( remaining > 0 ))
}

run_with_readiness_limit() {
  local duration
  printf -v duration '%d.%02ds' "$(($1 / 100))" "$(($1 % 100))"
  shift
  # Kill the process group at the limit so a client that ignores TERM
  # cannot keep provisioning blocked past the deadline.
  LC_ALL=C timeout --signal=KILL "$duration" "$@"
}

# Waits until the Caddy process of the caddy container answers its admin API.
wait_ready() {
  local deadline=$(( $(readiness_clock) + 10#$ready_timeout * 100 ))
  local remaining probe_budget state last_state=unavailable attempts=0
  while readiness_budget; do
    if state="$(run_with_readiness_limit "$remaining" "${docker_command[@]}" inspect --format '{{.State.Status}}' caddy 2>&1)"; then
      last_state="$state"
    elif readiness_budget; then
      echo "error: cannot read the state of the caddy container: $state" >&2
      echo "Run \`$logs_command\` and \`DOCKER_CONTEXT= ${docker_command[*]} ps --all\` for details." >&2
      return 1
    else
      break
    fi

    readiness_budget || break
    case "$state" in
      running)
        attempts=$((attempts + 1))
        probe_budget=$((remaining < 1000 ? remaining : 1000))
        if { run_with_readiness_limit "$probe_budget" "${docker_command[@]}" exec caddy wget -q -T 2 -O /dev/null http://127.0.0.1:2019/config/; } > /dev/null 2>&1 &&
          readiness_budget; then
          return 0
        fi
        ;;
      created) ;;
      *)
        echo "error: the caddy container is $state, not running; Caddy exited before it answered its admin API." >&2
        echo "Run \`$logs_command\` to see why Caddy stopped (for example an invalid Caddyfile or route file)." >&2
        return 1
        ;;
    esac

    readiness_budget || break
    if ! { run_with_readiness_limit "$remaining" sleep "$ready_interval"; } 2> /dev/null; then
      break
    fi
  done

  echo "error: Caddy did not answer its admin API (http://127.0.0.1:2019/config/) within ${ready_timeout} seconds; the container state is $last_state after $attempts probe attempts." >&2
  echo "Run \`$logs_command\` to see why Caddy is not ready." >&2
  return 1
}

# List by exact name to distinguish an absent container from an inspect
# failure. Keep the ID so removal targets only the container we inspected.
if ! current_id="$("${docker_command[@]}" container ls --all --no-trunc --filter 'name=^/caddy$' --format '{{.ID}}')"; then
  echo "error: cannot look up the caddy container." >&2
  exit 1
fi
current_image=""
running=""
current_log_driver=""
if [[ -n "$current_id" ]]; then
  if ! settings="$("${docker_command[@]}" container inspect --format '{{.Config.Image}}|{{.State.Running}}|{{.HostConfig.LogConfig.Type}}' "$current_id")"; then
    echo "error: cannot inspect the settings of the caddy container." >&2
    exit 1
  fi
  # Preserve field boundaries and reject missing fields and Docker's
  # placeholders before a settings mismatch can trigger removal.
  settings_pattern='^([^[:space:]|<>]+)\|(true|false)\|([^[:space:]|<>]+)$'
  if [[ ! "$settings" =~ $settings_pattern ]]; then
    echo "error: cannot determine the image, running state and log driver of the caddy container." >&2
    exit 1
  fi
  current_image="${BASH_REMATCH[1]}"
  running="${BASH_REMATCH[2]}"
  current_log_driver="${BASH_REMATCH[3]}"
fi

if [[ "$current_image" != "$caddy_image" || "$current_log_driver" != "local" ]]; then
  # Obtain the pinned image before the old container goes away. `docker run`
  # would pull it implicitly, but only after the removal, so an unreachable
  # registry would leave the Droplet without a proxy. When the pull fails,
  # `set -e` ends the script here with the pull diagnostic and the old
  # container keeps running.
  "${docker_command[@]}" image inspect "$caddy_image" > /dev/null 2>&1 || "${docker_command[@]}" pull "$caddy_image"
  if [[ -n "$current_id" ]]; then
    "${docker_command[@]}" rm --force "$current_id" > /dev/null
  fi
  "${docker_command[@]}" run --detach --name caddy \
    --network "$network" \
    --restart unless-stopped \
    --log-driver local \
    --publish 80:80 --publish 443:443 --publish 443:443/udp \
    --volume "$root/caddy:/etc/caddy:ro" \
    --volume caddy-data:/data \
    --volume caddy-config:/config \
    "$caddy_image"
  wait_ready
elif [[ "$running" != "true" ]]; then
  "${docker_command[@]}" start caddy
  wait_ready
else
  "${docker_command[@]}" exec caddy caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
fi
