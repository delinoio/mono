#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Builds the OwnSand Linux guest kernel in a Docker container: the pinned
# Linux long-term release with the architecture's defconfig and the
# kernel.config fragment next to this script. The output is the
# uncompressed kernel that the runtimes boot directly: the ELF vmlinux on
# amd64 (Hyper-V and Firecracker) and the arm64 Image (Virtualization.framework
# on Apple silicon). It needs Docker and runs on Linux and macOS hosts of
# either architecture; a cross compiler is used when the target differs from
# the container architecture.
#
# This is a development kernel: a kernel built and signed by the OwnSand
# release process replaces it before the first release.
#
# Usage: build-kernel.sh <amd64|arm64> <output-kernel>
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <amd64|arm64> <output-kernel>" >&2
  exit 2
fi
arch="$1"
out="$2"
case "$arch" in
  amd64 | arm64) ;;
  *)
    echo "unknown architecture: $arch" >&2
    exit 2
    ;;
esac

validate_destination() {
  if [ -d "$1" ]; then
    echo "kernel output path is a directory: $1" >&2
    return 1
  fi
  if [ -e "$1" ] && [ ! -f "$1" ] && [ ! -L "$1" ]; then
    echo "kernel output path is not a regular file or symbolic link: $1" >&2
    return 1
  fi
}
validate_destination "$out"
validate_destination "$out.config"

version="6.18.64"
# From https://cdn.kernel.org/pub/linux/kernel/v6.x/sha256sums.asc.
sha256="020d45510c539972e6f14c5c71566878d3b7ef56fb082b3e09134f2ebaade562"
image="debian:trixie-20261104"

here="$(cd "$(dirname "$0")" && pwd)"
out_dir="$(cd "$(dirname "$out")" && pwd -P)"
out_name="$(basename "$out")"
cache="${OWNSAND_KERNEL_CACHE:-$HOME/.cache/ownsand-kernel}"
mkdir -p "$cache"
# Docker reads a -v source without a slash as the name of a named volume, so
# a bare name such as kernel-cache would hide the host archives. The absolute
# path always names the host directory. pwd -P prints the physical path with
# symbolic links resolved (for example /private/var for /var on macOS), so the
# mount source does not depend on the logical path that the caller passed. A
# failure here stops the script (set -e) before docker runs.
cache="$(cd "$cache" && pwd -P)"

# Keep the stage on the destination filesystem so each final move is a rename.
stage="$(mktemp -d "$out_dir/.ownsand-kernel-stage.XXXXXX")"
outputs=("$out_name" "$out_name.config")
publish_lock="$out_dir/.ownsand-kernel-publish.lock"
locked=false
publishing=false

cleanup() {
  local status=$? name keep_stage=false
  trap '' HUP INT TERM
  if "$publishing"; then
    for name in "${outputs[@]}"; do
      # The staged file is absent only after its final rename succeeds.
      if [ -f "$stage/out/$name" ]; then
        continue
      fi
      if [ -e "$stage/previous/$name" ] || [ -L "$stage/previous/$name" ]; then
        if mv -f -- "$stage/previous/$name" "$out_dir/"; then
          continue
        fi
      elif rm -f -- "$out_dir/$name"; then
        continue
      fi
      echo "could not restore $out_dir/$name" >&2
      keep_stage=true
    done
  fi
  if "$keep_stage"; then
    echo "kernel rollback incomplete; recovery files are in $stage" >&2
  elif ! rm -rf -- "$stage"; then
    echo "could not remove kernel staging directory: $stage" >&2
  fi
  if "$locked" && ! rmdir -- "$publish_lock"; then
    echo "could not remove kernel publication lock: $publish_lock" >&2
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -- "$stage/out" "$stage/previous"

docker run --rm \
  -v "$here:/src:ro" \
  -v "$stage/out:/out" \
  -v "$cache:/cache" \
  -e "VERSION=$version" \
  -e "SHA256=$sha256" \
  -e "TARGET=$arch" \
  -e "OUT_NAME=$out_name" \
  "$image" bash -euo pipefail -c '
    host="$(dpkg --print-architecture)"
    packages="bc bison flex libelf-dev libssl-dev make gcc libc6-dev xz-utils curl ca-certificates python3 cpio kmod"
    if [ "$TARGET" != "$host" ]; then
      case "$TARGET" in
        amd64) packages="$packages gcc-x86-64-linux-gnu"; cross=x86_64-linux-gnu- ;;
        arm64) packages="$packages gcc-aarch64-linux-gnu"; cross=aarch64-linux-gnu- ;;
      esac
    else
      cross=
    fi
    apt-get update -qq >/dev/null
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends $packages >/dev/null
    # Fills the shared cache with a verified archive: a unique temporary file
    # that is checked before an atomic rename, so concurrent builds never see
    # a partial archive and a bad cached archive is replaced.
    bash /src/fetch-kernel-source.sh "$VERSION" "$SHA256" \
      "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$VERSION.tar.xz" /cache
    tarball="/cache/linux-$VERSION.tar.xz"
    mkdir -p /build
    tar -xJf "$tarball" -C /build
    cd "/build/linux-$VERSION"
    # Fixed build metadata, so that a rebuild gives the same kernel.
    export KBUILD_BUILD_TIMESTAMP="@0" KBUILD_BUILD_USER=ownsand KBUILD_BUILD_HOST=ownsand
    case "$TARGET" in
      amd64) karch=x86_64; target=vmlinux; built=vmlinux ;;
      arm64) karch=arm64; target=Image; built=arch/arm64/boot/Image ;;
    esac
    make -s ARCH="$karch" CROSS_COMPILE="$cross" defconfig
    scripts/kconfig/merge_config.sh -m .config /src/kernel.config >/dev/null
    make -s ARCH="$karch" CROSS_COMPILE="$cross" olddefconfig
    missing=0
    while IFS= read -r line; do
      case "$line" in
        CONFIG_*=y)
          if ! grep -qx "$line" .config; then
            echo "the merged configuration does not set $line" >&2
            missing=1
          fi
          ;;
      esac
    done < /src/kernel.config
    [ "$missing" = 0 ]
    make -s ARCH="$karch" CROSS_COMPILE="$cross" -j"$(nproc)" "$target"
    cp -T -- "$built" "/out/$OUT_NAME"
    cp -T -- .config "/out/$OUT_NAME.config"
  ' || {
  status=$?
  echo "kernel build failed for $out" >&2
  exit "$status"
}

for name in "${outputs[@]}"; do
  if [ ! -f "$stage/out/$name" ] || [ -L "$stage/out/$name" ]; then
    echo "kernel build did not produce a regular file for $out_dir/$name" >&2
    exit 1
  fi
done

if command -v shasum >/dev/null 2>&1; then
  checksum_tool=(shasum -a 256)
elif command -v sha256sum >/dev/null 2>&1; then
  checksum_tool=(sha256sum)
else
  echo "could not compute SHA-256 for $out: install shasum or sha256sum" >&2
  exit 1
fi

# Hash stdin so checksum output stays the same even for escaped filenames.
if ! checksum="$("${checksum_tool[@]}" < "$stage/out/$out_name")" ||
  ! [[ "${checksum%% *}" =~ ^[0-9a-f]{64}$ ]]; then
  echo "could not compute SHA-256 for $out with ${checksum_tool[0]}" >&2
  exit 1
fi

# A directory lock works on Linux and macOS without requiring a host lock tool.
if ! mkdir -- "$publish_lock"; then
  echo "could not acquire kernel publication lock: $publish_lock" >&2
  exit 1
fi
locked=true

for name in "${outputs[@]}"; do
  dest="$out_dir/$name"
  validate_destination "$dest"
  if [ -L "$dest" ] || [ -f "$dest" ]; then
    cp -pP -- "$dest" "$stage/previous/$name"
  fi
done

publishing=true
for name in "${outputs[@]}"; do
  # Give mv the parent directory so a directory at the final path fails.
  if ! mv -f -- "$stage/out/$name" "$out_dir/"; then
    echo "could not publish kernel output: $out_dir/$name" >&2
    exit 1
  fi
done
publishing=false
echo "wrote $out (${checksum%% *})"
