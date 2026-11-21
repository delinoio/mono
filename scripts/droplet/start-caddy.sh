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
#   - The container is missing, or it uses another image: remove it (if
#     present) and create it with the pinned image.
#   - The container uses the pinned image and is not running (for example
#     after `docker stop`, which `--restart unless-stopped` does not undo):
#     start it. `docker exec` cannot reload a stopped container, and the
#     started container reads the Caddyfile that `provision.sh` just wrote.
#   - The container uses the pinned image and is running: reload the
#     configuration without recreating it.
# A failing `docker run`, `docker start` or `docker exec` fails the script
# with the Docker diagnostic on standard error.
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
