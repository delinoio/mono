#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Makes the Docker daemon use the `local` log driver, which rotates container
# logs (at most 100 MB per container by default), and keeps every other
# setting of the daemon configuration. `provision.sh` runs it as root; it can
# run again at any time:
#
#   configure-docker-logs.sh [daemon-config-file]
#
# Argument:
#   daemon-config-file  Docker daemon configuration; default
#                       /etc/docker/daemon.json.
#
# The script handles the configuration file in one of these ways:
#   - The file is missing: create it with only the `log-driver` setting.
#   - The file is a JSON object whose top-level `log-driver` is already
#     `local`: leave it byte for byte as it is.
#   - The file is a JSON object with another `log-driver` value or none: set
#     the key and keep every other key (`data-root`, `registry-mirrors` and so
#     on). The file is rewritten as indented JSON.
#   - The file is not valid JSON, or valid JSON that is not an object: print
#     a diagnostic and exit non-zero before it writes a file or restarts
#     Docker.
# The new content goes to a temporary file in the same directory, which
# replaces the configuration with one rename, so a failed write leaves the
# original file intact. An existing file keeps its permission bits and owner.
# After a successful write, restart Docker and verify its active log driver.
# For an unchanged file, check the active driver first. Restart if the check
# fails or reports another driver, for example after an interrupted run.
#
# The JSON handling needs python3, which Ubuntu 24.04 images include; the
# script adds no package.
set -euo pipefail

if (( BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4) )); then
  echo "error: configure-docker-logs.sh needs Bash 4.4 or newer, not $BASH_VERSION." >&2
  exit 2
fi
if (( $# > 1 )); then
  echo "usage: configure-docker-logs.sh [daemon-config-file]" >&2
  exit 2
fi
if ! command -v python3 > /dev/null 2>&1; then
  echo "error: configure-docker-logs.sh needs python3 to edit the Docker daemon configuration." >&2
  exit 1
fi

config="${1:-/etc/docker/daemon.json}"

# Exit codes of the Python step: 0 = file already correct (nothing written),
# 10 = file written, anything else = failure with a diagnostic on standard
# error and the original file intact.
status=0
python3 - "$config" << 'PY' || status=$?
import json
import os
import stat
import sys
import tempfile

path = sys.argv[1]
# Write next to the real file when the configuration is a symbolic link, so
# the rename replaces the file and keeps the link.
path = os.path.realpath(path)
directory = os.path.dirname(path)


def fail(message):
    print(f"error: {message}", file=sys.stderr)
    sys.exit(1)


def reject_constant(literal):
    raise ValueError(f"unexpected literal {literal}")


try:
    with open(path, "rb") as handle:
        raw = handle.read()
    existing = os.stat(path)
except FileNotFoundError:
    raw = None
    existing = None
except OSError as error:
    fail(f"cannot read {path}: {error}")

if raw is None:
    config = {}
else:
    try:
        config = json.loads(raw.decode("utf-8"), parse_constant=reject_constant)
    except (UnicodeDecodeError, ValueError) as error:
        fail(f"{path} is not valid JSON ({error}); fix or remove it, then run again")
    if not isinstance(config, dict):
        fail(f"{path} must hold a JSON object, not {type(config).__name__}")

if config.get("log-driver") == "local":
    sys.exit(0)

config["log-driver"] = "local"
content = (json.dumps(config, indent=2, ensure_ascii=False) + "\n").encode("utf-8")

temporary = None
try:
    descriptor, temporary = tempfile.mkstemp(
        prefix=".daemon.json.", suffix=".tmp", dir=directory
    )
    with os.fdopen(descriptor, "wb") as handle:
        handle.write(content)
        handle.flush()
        mode = stat.S_IMODE(existing.st_mode) if existing else 0o644
        os.fchmod(handle.fileno(), mode)
        if existing:
            try:
                os.fchown(handle.fileno(), existing.st_uid, existing.st_gid)
            except PermissionError:
                # Only root can change the owner; the owner of a file that
                # root did not create is not worth a failed provisioning.
                pass
        os.fsync(handle.fileno())
    os.replace(temporary, path)
    temporary = None
except OSError as error:
    fail(f"cannot write {path}: {error}; the original file is unchanged")
finally:
    if temporary is not None:
        try:
            os.unlink(temporary)
        except OSError:
            pass
# The rename is done. Syncing the directory only makes it durable across a
# power loss, so a failure here must not report the written file as unchanged.
try:
    directory_descriptor = os.open(directory, os.O_RDONLY)
    try:
        os.fsync(directory_descriptor)
    finally:
        os.close(directory_descriptor)
except OSError:
    pass
sys.exit(10)
PY

active_log_driver() {
  # Query the system daemon even if the caller has selected another context.
  DOCKER_CONTEXT= docker --host unix:///var/run/docker.sock info --format '{{.LoggingDriver}}'
}

case "$status" in
  0)
    if driver="$(active_log_driver 2>/dev/null)" && [[ "$driver" == local ]]; then
      echo "Docker already uses the local log driver; $config is unchanged."
      exit 0
    fi
    echo "$config already sets the local log driver; restarting Docker to apply it."
    ;;
  10)
    echo "Set the local log driver in $config; restarting Docker."
    ;;
  *)
    echo "error: $config was not changed and Docker was not restarted." >&2
    exit 1
    ;;
esac

if ! systemctl restart docker; then
  echo "error: could not restart Docker to apply the local log driver from $config." >&2
  exit 1
fi
if ! driver="$(active_log_driver)"; then
  echo "error: could not read Docker's active log driver from the local socket after restart." >&2
  exit 1
fi
if [[ "$driver" != local ]]; then
  echo "error: Docker reports log driver ${driver@Q} after restart; expected local." >&2
  exit 1
fi
echo "Docker uses the local log driver."
