#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Builds the minimal Linux base image root file system of the Firecracker
# runtime: an ext4 image with a static BusyBox userland and the guest helper
# as /sbin/init. It needs Go, a statically linked busybox and mkfs.ext4
# (e2fsprogs 1.43 or later for -d); it does not need root.
#
# Usage: build-rootfs.sh <output.ext4> [size-mib]
set -euo pipefail

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <output.ext4> [size-mib]" >&2
  exit 2
fi
out="$1"
size_mib="${2:-256}"
repo="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
work="$(mktemp -d "${TMPDIR:-/tmp}/ownsand-rootfs.XXXXXX")"
trap 'rm -rf "$work"' EXIT
rootfs="$work/rootfs"

busybox="$(command -v busybox || true)"
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
(cd "$repo" && CGO_ENABLED=0 go build -trimpath -o "$rootfs/sbin/ownsand-guest" ./cmds/ownsand-guest)
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
