#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Builds the minimal Linux base image root file system: an ext4 image with a
# static BusyBox userland and the guest helper as /sbin/init. It needs Go, a
# statically linked busybox and mkfs.ext4 (e2fsprogs 1.43 or later for -d);
# it does not need root. build-image.sh runs it in a container for another
# architecture or on a host without these tools.
#
# Environment: GOARCH selects the guest architecture of the guest helper
# (default: the Go default); GUEST_BINARY names a prebuilt linux guest
# helper instead of building it; BUSYBOX names the busybox binary (default:
# busybox on PATH). The busybox architecture must match GOARCH.
#
# Usage: build-rootfs.sh <output.ext4> [size-mib]
set -euo pipefail

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <output.ext4> [size-mib]" >&2
  exit 2
fi
out="$1"
size_mib="${2:-256}"
repo="$(cd "$(dirname "$0")/../../../.." && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/ownsand-rootfs.XXXXXX")"
trap 'rm -rf "$work"' EXIT
rootfs="$work/rootfs"

busybox="${BUSYBOX:-$(command -v busybox || true)}"
if [ -z "$busybox" ]; then
  echo "busybox is required" >&2
  exit 1
fi
if ! file "$busybox" | grep -q 'statically linked'; then
  echo "busybox must be statically linked: $busybox" >&2
  exit 1
fi
mkfs="$(command -v mkfs.ext4 || echo /usr/sbin/mkfs.ext4)"

mkdir -p "$rootfs"/{bin,sbin,etc,proc,sys,dev,tmp,run,root,usr/bin,usr/sbin,var/lib/ownsand-guest}
if [ -n "${GUEST_BINARY:-}" ]; then
  cp "$GUEST_BINARY" "$rootfs/sbin/ownsand-guest"
  chmod 0755 "$rootfs/sbin/ownsand-guest"
else
  (cd "$repo" && CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -o "$rootfs/sbin/ownsand-guest" ./cmds/ownsand-guest)
fi
ln -s ownsand-guest "$rootfs/sbin/init"
cp "$busybox" "$rootfs/bin/busybox"
for applet in $("$busybox" --list); do
  case "$applet" in
    busybox | init) ;;
    *) ln -sf busybox "$rootfs/bin/$applet" ;;
  esac
done
printf 'root:x:0:0:root:/root:/bin/sh\n' > "$rootfs/etc/passwd"
printf 'root:x:0:\n' > "$rootfs/etc/group"
printf 'sandbox\n' > "$rootfs/etc/hostname"
: > "$rootfs/etc/resolv.conf"

rm -f "$out"
truncate -s "${size_mib}M" "$out"
"$mkfs" -q -F -L ownsand-root -d "$rootfs" -E root_owner=0:0 "$out"
echo "wrote $out"
