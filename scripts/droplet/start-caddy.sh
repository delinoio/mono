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
# The script handles the state of the `caddy` container in one of three ways,
# so the certificates in the `caddy-data` volume and the open connections stay
# whenever the image did not change:
#   - The container is missing, or it uses another image: obtain the pinned
#     image first (a `docker pull`, only when the image is not already
#     local), then remove the old container (if present) and create it with
#     the pinned image. A failing pull fails the script before the old
#     container is touched, so the proxy keeps serving; a local image needs no
#     registry connection.
#   - The container uses the pinned image and is not running (for example
#     after `docker stop`, which `--restart unless-stopped` does not undo):
#     start it. `docker exec` cannot reload a stopped container, and the
#     started container reads the Caddyfile that `provision.sh` just wrote.
#   - The container uses the pinned image and is running: reload the
#     configuration without recreating it.
# After the create and start paths the script waits until the new Caddy
# process answers its admin API, because a successful `docker run` or
# `docker start` only means that Docker accepted the start: Caddy can still
# fail to load the Caddyfile or an imported route file and exit. The probe
# runs `wget` inside the container against http://127.0.0.1:2019/config/ (the
# loopback admin endpoint in the container's network namespace, so no port is
# published) and discards the body. Each attempt first reads the container
# state; a container that is exited, dead or restarting fails the script at
# once, and so does a container that does not answer within 30 seconds. The
# failure message names the state and points to `docker logs caddy`. The
# reload path does not wait: the running process already answered and
# `caddy reload` validates the new configuration itself.
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
ready_timeout="${START_CADDY_READY_TIMEOUT:-30}"
ready_interval="${START_CADDY_READY_INTERVAL:-1}"
if [[ ! "$ready_timeout" =~ ^[0-9]+$ ]] || [[ ! "$ready_interval" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
  echo "error: START_CADDY_READY_TIMEOUT and START_CADDY_READY_INTERVAL must be numbers of seconds." >&2
  exit 2
fi

# Waits until the Caddy process of the `caddy` container answers its admin
# API. Returns non-zero with a diagnostic when the container is not running
# (exited, dead, restarting and so on) or does not answer before the deadline.
wait_ready() {
  local deadline=$((SECONDS + 10#$ready_timeout))
  local state attempts=0
  # `timeout` bounds one attempt when `docker exec` itself hangs; `wget -T`
  # bounds the HTTP request. Without `timeout` only the latter applies.
  local -a bound=()
  if command -v timeout > /dev/null 2>&1; then
    bound=(timeout 10)
  fi
  while :; do
    if ! state="$(docker inspect --format '{{.State.Status}}' caddy 2>&1)"; then
      echo "error: cannot read the state of the caddy container: $state" >&2
      echo "Run \`docker logs caddy\` and \`docker ps --all\` for details." >&2
      return 1
    fi
    case "$state" in
      running)
        attempts=$((attempts + 1))
        if "${bound[@]}" docker exec caddy wget -q -T 2 -O /dev/null http://127.0.0.1:2019/config/ > /dev/null 2>&1; then
          return 0
        fi
        ;;
      created) ;;
      *)
        echo "error: the caddy container is $state, not running; Caddy exited before it answered its admin API." >&2
        echo "Run \`docker logs caddy\` to see why Caddy stopped (for example an invalid Caddyfile or route file)." >&2
        return 1
        ;;
    esac
    if (( SECONDS >= deadline )); then
      echo "error: Caddy did not answer its admin API (http://127.0.0.1:2019/config/) within ${ready_timeout} seconds; the container state is $state after $attempts probe attempts." >&2
      echo "Run \`docker logs caddy\` to see why Caddy is not ready." >&2
      return 1
    fi
    sleep "$ready_interval"
  done
}

# A missing container makes `docker inspect` fail, which leaves both values
# empty and selects the create path below.
current_image=""
running=""
read -r current_image running < <(
  docker inspect --format '{{.Config.Image}} {{.State.Running}}' caddy 2> /dev/null || true
) || true

if [[ "$current_image" != "$caddy_image" ]]; then
  # Obtain the pinned image before the old container goes away. `docker run`
  # would pull it implicitly, but only after the removal, so an unreachable
  # registry would leave the Droplet without a proxy. When the pull fails,
  # `set -e` ends the script here with the pull diagnostic and the old
  # container keeps running.
  docker image inspect "$caddy_image" > /dev/null 2>&1 || docker pull "$caddy_image"
  docker rm --force caddy > /dev/null 2>&1 || true
  docker run --detach --name caddy \
    --network "$network" \
    --restart unless-stopped \
    --publish 80:80 --publish 443:443 --publish 443:443/udp \
    --volume "$root/caddy:/etc/caddy:ro" \
    --volume caddy-data:/data \
    --volume caddy-config:/config \
    "$caddy_image"
  wait_ready
elif [[ "$running" != "true" ]]; then
  docker start caddy
  wait_ready
else
  docker exec caddy caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
fi
