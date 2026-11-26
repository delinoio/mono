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
#   - creates the `deploy` user that the deploy workflow connects as, with
#     the given public key as its only SSH key,
#   - creates /srv/delino (server env files, reverse proxy routes and
#     deployment state) and acquires the proxy lock,
#   - makes Docker rotate container logs (`local` log driver) with
#     configure-docker-logs.sh, which keeps every other daemon setting,
#   - enables Docker and creates the `delino` network,
#   - brings the Caddy reverse proxy to the running state with
#     start-caddy.sh: it creates the container, starts a stopped one, or
#     reloads a running one, and recreates it only when the pinned image
#     changes. After a create or a start it waits until Caddy answers its
#     admin API, so a Caddy that exited on a bad configuration fails the
#     script. Caddy terminates TLS for every server host name with
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

if ! command -v flock > /dev/null; then
  echo "error: provision.sh needs flock (util-linux) for the proxy lock." >&2
  exit 2
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
install_docker=true
if command -v docker > /dev/null; then
  # LoadState distinguishes a missing unit from an installed, stopped daemon.
  if ! docker_load_state="$(systemctl show --property=LoadState --value docker.service)"; then
    echo "error: could not query the load state of docker.service." >&2
    exit 1
  fi
  case "$docker_load_state" in
    loaded) install_docker=false ;;
    not-found) ;;
    *)
      printf 'error: unexpected load state for docker.service: %q.\n' "$docker_load_state" >&2
      exit 1
      ;;
  esac
fi

# https://docs.docker.com/engine/install/ubuntu/
if [[ "$install_docker" == true ]]; then
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
install -d -m 0755 -o "$deploy_user" -g "$deploy_user" "$root" "$root/state"
# Server env files hold secrets.
install -d -m 0700 -o "$deploy_user" -g "$deploy_user" "$root/env"
proxy_lock="$root/state/.proxy.lock"
proxy_lock_timeout="${DROPLET_PROXY_LOCK_TIMEOUT:-${DROPLET_LOCK_TIMEOUT:-600}}"
log "Acquire the Caddy proxy lock"
# Create the lock as the deploy user so its first open works even when root
# provisions with a restrictive umask. Preserve the inode on every rerun.
if ! runuser -u "$deploy_user" -- touch -- "$proxy_lock" ||
  ! { exec {proxy_lock_fd}>> "$proxy_lock"; } 2> /dev/null; then
  echo "error: could not open the proxy lock file $proxy_lock." >&2
  exit 1
fi
if ! flock --wait "$proxy_lock_timeout" "$proxy_lock_fd"; then
  echo "error: could not acquire the proxy lock $proxy_lock (wait limit: $proxy_lock_timeout seconds)." >&2
  exit 1
fi

# Restarting Docker can interrupt a deployment or rollback and start Caddy
# while route files are being changed. Keep the lock from logging setup
# through the Caddy helper, including its readiness checks.
log "Rotate container logs"
# The `local` log driver keeps at most 100 MB per container by default.
# configure-docker-logs.sh sets only the `log-driver` key and keeps the other
# daemon settings. It also recovers from an interrupted run by verifying the
# active driver and restarting Docker if necessary. The helper is resolved
# from this script, so provisioning works from any current directory.
"$(dirname "${BASH_SOURCE[0]}")/configure-docker-logs.sh" /etc/docker/daemon.json
systemctl enable --now docker

install -d -m 0755 -o "$deploy_user" -g "$deploy_user" "$root/caddy" "$root/caddy/sites"
caddyfile="{
	email $acme_email
}

# One route file per server, written by scripts/ci/droplet-deploy.sh.
import sites/*.caddy"
printf '%s\n' "$caddyfile" > "$root/caddy/Caddyfile"
chown "$deploy_user:$deploy_user" "$root/caddy/Caddyfile"

log "Create the $network Docker network"
# Use the same system daemon as configure-docker-logs.sh.
unset DOCKER_CONTEXT
docker_command=(docker --host unix:///var/run/docker.sock)
if ! "${docker_command[@]}" network inspect "$network" > /dev/null 2>&1; then
  "${docker_command[@]}" network create "$network"
fi

log "Start the Caddy reverse proxy"
# start-caddy.sh creates a missing container or recreates one with another
# image or log driver. When the pinned image and the local log driver match,
# the helper starts a stopped container or reloads a running one.
# After a create or a start the helper waits until Caddy answers its local
# admin API and fails when Caddy exited or does not answer within 30 seconds;
# `set -e` then ends this script before the `Done` message.
# The helper is resolved from this script, so provisioning works from any
# current directory.
"$(dirname "${BASH_SOURCE[0]}")/start-caddy.sh" "$caddy_image" "$root" "$network"
exec {proxy_lock_fd}>&-

log "Done. Next: log in to the registry as $deploy_user, create the server env files and configure the Cloud Firewall."
