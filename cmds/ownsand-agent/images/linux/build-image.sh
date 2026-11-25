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
# The output directory gets `vmlinux`, `vmlinux.config`, and `rootfs.ext4`.
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

# Keep staging on the destination file system so publication uses renames.
# Docker Desktop already needs access to the output directory.
stage="$(mktemp -d "$out_dir/.ownsand-image-stage.XXXXXX")"
outputs=(vmlinux vmlinux.config rootfs.ext4)
publish_lock="$out_dir/.ownsand-image-publish.lock"
publishing=false

cleanup() {
  local status=$? name keep_stage=false
  trap '' INT TERM
  if "$publishing"; then
    for name in "${outputs[@]}"; do
      # Each staged file disappears only when its rename completes.
      if [ -e "$stage/out/$name" ]; then
        continue
      fi
      if [ -e "$stage/previous/$name" ] || [ -L "$stage/previous/$name" ]; then
        if ! mv -f -- "$stage/previous/$name" "$out_dir/" 9>&-; then
          echo "could not restore $out_dir/$name" >&2
          keep_stage=true
        fi
      elif ! rm -f -- "$out_dir/$name" 9>&-; then
        keep_stage=true
      fi
    done
  fi
  if "$keep_stage"; then
    echo "image rollback incomplete; recovery files are in $stage" >&2
  elif ! rm -rf -- "$stage" 9>&- && [ "$status" -eq 0 ]; then
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -- "$stage/guest" "$stage/out" "$stage/previous"
(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false -o "$stage/guest/ownsand-guest" ./cmds/ownsand-guest)

docker run --rm --platform "linux/$arch" \
  -v "$repo/cmds/ownsand-agent/images/linux:/scripts:ro" \
  -v "$stage/guest:/stage:ro" \
  -v "$stage/out:/out" \
  -e "SIZE_MIB=$size_mib" \
  "$image" bash -euo pipefail -c '
    apt-get update -qq >/dev/null
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends busybox-static e2fsprogs file >/dev/null
    mkdir -p /work/cmds/ownsand-agent/images/linux
    cp /scripts/build-rootfs.sh /work/cmds/ownsand-agent/images/linux/
    GUEST_BINARY=/stage/ownsand-guest BUSYBOX=/bin/busybox \
      /work/cmds/ownsand-agent/images/linux/build-rootfs.sh /out/rootfs.ext4 "$SIZE_MIB"
  '
"$here/build-kernel.sh" "$arch" "$stage/out/vmlinux"
for name in "${outputs[@]}"; do
  if [ ! -f "$stage/out/$name" ] || [ -L "$stage/out/$name" ]; then
    echo "build did not produce a regular file: $name" >&2
    exit 1
  fi
done

host_os="$(go env GOHOSTOS)"
host_arch="$(go env GOHOSTARCH)"
(cd "$repo" && CGO_ENABLED=0 GOOS="$host_os" GOARCH="$host_arch" \
  go build -mod=readonly -trimpath -buildvcs=false -o "$stage/publish-lock" "$here/publishlock/main.go")

if [ ! -L "$publish_lock" ] && [ -d "$publish_lock" ]; then
  echo "legacy image publication lock directory: $publish_lock" >&2
  echo "Confirm no older image builder is active before removing the empty directory with rmdir." >&2
  exit 1
fi
if [ -L "$publish_lock" ] || { [ -e "$publish_lock" ] && [ ! -f "$publish_lock" ]; }; then
  echo "image publication lock path is not a regular file: $publish_lock" >&2
  exit 1
fi

# The helper locks the same open file description that Bash retains on fd 9.
# Close it in publication children so an orphan cannot keep the lock held.
exec 9<>"$publish_lock"
"$stage/publish-lock" "$publish_lock" <&9 9>&-

for name in "${outputs[@]}"; do
  dest="$out_dir/$name"
  if [ -d "$dest" ]; then
    echo "output path is a directory: $dest" >&2
    exit 1
  fi
  if [ -L "$dest" ] || [ -f "$dest" ]; then
    cp -pP -- "$dest" "$stage/previous/$name" 9>&-
  elif [ -e "$dest" ]; then
    echo "output path is not a regular file or symbolic link: $dest" >&2
    exit 1
  fi
done

publishing=true
for name in "${outputs[@]}"; do
  # Let mv append the basename, so a directory at the final filename fails.
  mv -f -- "$stage/out/$name" "$out_dir/" 9>&-
done
publishing=false
echo "wrote $out_dir/vmlinux, $out_dir/vmlinux.config and $out_dir/rootfs.ext4"
