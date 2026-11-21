#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prepares the shared DigitalOcean Droplet that runs every server under
# `servers/`. Run it once as root on a new Ubuntu 24.04 LTS Droplet, and
# again after a change to this script; every step can run more than once.
#
#   provision.sh <deploy-public-key-file> <acme-email>
#
# The script:
#   - installs Docker Engine from the Docker apt repository,
#   - makes Docker rotate container logs (`local` log driver),
#   - creates the `deploy` user that the deploy workflow connects as, with
#     the given public key as its only SSH key,
#   - creates /srv/delino (server env files, reverse proxy routes, deployment
#     state) and the `delino` Docker network,
#   - brings the Caddy reverse proxy to the running state with
#     start-caddy.sh: it creates the container, starts a stopped one, or
#     reloads a running one, and recreates it only when the pinned image
#     changes. Caddy terminates TLS for every server host name with
#     automatic Let's Encrypt certificates.
#
# It does not configure the DigitalOcean Cloud Firewall, the registry login
# or the server env files; docs/repository-workflow-contract.md lists those
# steps.
set -euo pipefail

if (( BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4) )); then
  echo "error: provision.sh needs Bash 4.4 or newer, not $BASH_VERSION." >&2
  exit 2
fi
if (( $# != 2 )); then
  echo "usage: provision.sh <deploy-public-key-file> <acme-email>" >&2
  exit 2
fi
if (( EUID != 0 )); then
  echo "error: run provision.sh as root." >&2
  exit 1
fi

public_key_file="$1"
acme_email="$2"
root="/srv/delino"
network="delino"
deploy_user="deploy"
# Caddy release, pinned by the digest of its multi-platform image index.
# Update both values together.
caddy_image="caddy:2.11.9@sha256:4851e54a46b206517c574821bb5cc67f170d2156041e9fbc4c9193b45e84a75b"

if [[ ! "$acme_email" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]]; then
  echo "error: ${acme_email@Q} is not an email address." >&2
  exit 1
fi
public_key="$(< "$public_key_file")"
if [[ ! "$public_key" =~ ^(ssh-ed25519|ecdsa-sha2-nistp256|ssh-rsa)\ [A-Za-z0-9+/=]+(\ [^[:cntrl:]]*)?$ ]]; then
  echo "error: $public_key_file does not hold one SSH public key." >&2
  exit 1
fi

log() {
  echo "==> $*"
}

log "Install Docker Engine"
# https://docs.docker.com/engine/install/ubuntu/
if ! command -v docker > /dev/null; then
  apt-get update
  apt-get install -y ca-certificates curl
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  # shellcheck source=/dev/null
  codename="$(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")"
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $codename stable" \
    > /etc/apt/sources.list.d/docker.list
  apt-get update
  apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin
fi

log "Rotate container logs"
# The `local` log driver keeps at most 100 MB per container by default.
daemon_config='{
  "log-driver": "local"
}'
if [[ ! -f /etc/docker/daemon.json ]] || [[ "$(< /etc/docker/daemon.json)" != "$daemon_config" ]]; then
  printf '%s\n' "$daemon_config" > /etc/docker/daemon.json
  systemctl restart docker
fi
systemctl enable --now docker

log "Create the $deploy_user user"
if ! id "$deploy_user" > /dev/null 2>&1; then
  useradd --create-home --shell /bin/bash "$deploy_user"
fi
# Membership in the docker group gives root-equivalent access to the host.
# The deploy workflow needs it to replace containers; protect the key.
usermod --append --groups docker "$deploy_user"
home="$(getent passwd "$deploy_user" | cut -d: -f6)"
install -d -m 0700 -o "$deploy_user" -g "$deploy_user" "$home/.ssh"
# `restrict` turns off forwarding and terminal allocation; the workflow only
# runs commands.
printf 'restrict %s\n' "$public_key" > "$home/.ssh/authorized_keys"
chown "$deploy_user:$deploy_user" "$home/.ssh/authorized_keys"
chmod 0600 "$home/.ssh/authorized_keys"

log "Create $root"
install -d -m 0755 -o "$deploy_user" -g "$deploy_user" "$root" "$root/caddy" "$root/caddy/sites" "$root/state"
# Server env files hold secrets.
install -d -m 0700 -o "$deploy_user" -g "$deploy_user" "$root/env"
caddyfile="{
	email $acme_email
}

# One route file per server, written by scripts/ci/droplet-deploy.sh.
import sites/*.caddy"
printf '%s\n' "$caddyfile" > "$root/caddy/Caddyfile"
chown "$deploy_user:$deploy_user" "$root/caddy/Caddyfile"

log "Create the $network Docker network"
if ! docker network inspect "$network" > /dev/null 2>&1; then
  docker network create "$network"
fi

log "Start the Caddy reverse proxy"
# start-caddy.sh creates a missing container, recreates one that runs another
# image, starts a stopped one that has the pinned image and reloads a running
# one that has it. The container is recreated only when its image changed, so
# the certificates in the `caddy-data` volume and the open connections stay.
# The helper is resolved from this script, so provisioning works from any
# current directory.
"$(dirname "${BASH_SOURCE[0]}")/start-caddy.sh" "$caddy_image" "$root" "$network"

log "Done. Next: log in to the registry as $deploy_user, create the server env files and configure the Cloud Firewall."
