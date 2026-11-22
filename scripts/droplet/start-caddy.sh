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
elif [[ "$running" != "true" ]]; then
  docker start caddy
else
  docker exec caddy caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
fi
