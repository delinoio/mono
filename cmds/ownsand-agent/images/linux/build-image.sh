#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Builds the development Linux base image of one guest architecture with
# Docker: the guest kernel (build-kernel.sh) and the root file system
# (build-rootfs.sh, run in a container of the guest architecture with a
# static BusyBox and e2fsprogs). It needs Go and Docker and runs on Linux and
# macOS hosts; a container of the other architecture needs emulation (Docker
# Desktop provides it).
#
# The output directory gets `vmlinux` and `rootfs.ext4`, the layout of
# images/linux in the agent state directory.
#
# Usage: build-image.sh <amd64|arm64> <output-dir> [rootfs-size-mib]
set -euo pipefail

if [ "$#" -lt 2 ]; then
  echo "usage: $0 <amd64|arm64> <output-dir> [rootfs-size-mib]" >&2
  exit 2
fi
arch="$1"
out_dir="$2"
size_mib="${3:-256}"
case "$arch" in
  amd64 | arm64) ;;
  *)
    echo "unknown architecture: $arch" >&2
    exit 2
    ;;
esac
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../../../.." && pwd)"
image="debian:trixie-20261104"
mkdir -p "$out_dir"
out_dir="$(cd "$out_dir" && pwd)"

# The guest helper goes to a unique staging directory that the root file system
# container mounts read-only, so a file that already exists at
# "$out_dir/ownsand-guest" stays untouched on success and on failure. The
# directory is inside the output directory, which Docker already has to share
# with the container (Docker Desktop on macOS shares only some host paths), and
# the trap removes it on success, on a failed step and on a signal.
stage="$(mktemp -d "$out_dir/.ownsand-guest-stage.XXXXXX")"
trap 'rm -rf "$stage"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false -o "$stage/ownsand-guest" ./cmds/ownsand-guest)

docker run --rm --platform "linux/$arch" \
  -v "$repo/cmds/ownsand-agent/images/linux:/scripts:ro" \
  -v "$stage:/stage:ro" \
  -v "$out_dir:/out" \
  -e "SIZE_MIB=$size_mib" \
  "$image" bash -euo pipefail -c '
    apt-get update -qq >/dev/null
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends busybox-static e2fsprogs file >/dev/null
    mkdir -p /work/cmds/ownsand-agent/images/linux
    cp /scripts/build-rootfs.sh /work/cmds/ownsand-agent/images/linux/
    GUEST_BINARY=/stage/ownsand-guest BUSYBOX=/bin/busybox \
      /work/cmds/ownsand-agent/images/linux/build-rootfs.sh /out/rootfs.ext4 "$SIZE_MIB"
  '
rm -rf "$stage"

"$here/build-kernel.sh" "$arch" "$out_dir/vmlinux"
echo "wrote $out_dir/vmlinux and $out_dir/rootfs.ext4"
