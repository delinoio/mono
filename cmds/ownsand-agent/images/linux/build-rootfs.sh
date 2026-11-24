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
# Keep staging beside the output so publication stays on one file system.
stage=""
trap 'rm -rf -- "$work"; if [ -n "$stage" ]; then rm -rf -- "$stage"; fi' EXIT
rootfs="$work/rootfs"

busybox="${BUSYBOX:-$(command -v busybox || true)}"
if [ -z "$busybox" ]; then
  echo "busybox is required" >&2
  exit 1
fi
# -L follows a symlink, because file describes the link and not its target
# otherwise, and a dangling link prints no linkage so it is still rejected.
# file prints "static-pie linked" for a static position-independent executable.
if ! file -L -- "$busybox" | grep -Eq 'statically linked|static-pie linked'; then
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
# Capture the applet list in an assignment: a failing command substitution in
# the word list of a for loop does not stop a set -e script, so a busybox
# that cannot list its applets would otherwise produce an image without them.
if ! applets="$("$busybox" --list)"; then
  echo "busybox --list failed: $busybox" >&2
  exit 1
fi
if [ -z "$applets" ]; then
  echo "busybox --list printed no applets: $busybox" >&2
  exit 1
fi
while IFS= read -r applet; do
  case "$applet" in
    busybox | init | '') ;;
    *) ln -sf busybox "$rootfs/bin/$applet" ;;
  esac
done <<< "$applets"
if [ ! -L "$rootfs/bin/sh" ]; then
  echo "busybox does not provide the sh applet: $busybox" >&2
  exit 1
fi
printf 'root:x:0:0:root:/root:/bin/sh\n' > "$rootfs/etc/passwd"
printf 'root:x:0:\n' > "$rootfs/etc/group"
printf 'sandbox\n' > "$rootfs/etc/hostname"
: > "$rootfs/etc/resolv.conf"

# Build the image beside the output and publish it with one rename, so an
# allocation or formatting failure leaves an earlier image byte for byte
# unchanged and a first build that fails leaves nothing at the output path.
if [ -d "$out" ]; then
  echo "output path is a directory: $out" >&2
  exit 1
fi
out_name="${out##*/}"
if [ -z "$out_name" ]; then
  echo "output path must name a file: $out" >&2
  exit 1
fi
out_dir=./
case "$out" in
  /*) out_dir="${out%/*}/" ;;
  */*) out_dir="./${out%/*}/" ;;
esac
stage="$(mktemp -d "${out_dir}.${out_name}.XXXXXX")"
staged="$stage/$out_name"
: > "$staged"
truncate -s "${size_mib}M" "$staged"
"$mkfs" -q -F -L ownsand-root -d "$rootfs" -E root_owner=0:0 "$staged"
# Let mv append the basename, so a directory at the final filename fails.
if ! mv -f -- "$staged" "$out_dir"; then
  echo "could not publish rootfs image: $out" >&2
  exit 1
fi
echo "wrote $out"
