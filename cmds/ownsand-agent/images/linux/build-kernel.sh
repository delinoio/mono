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

version="6.18.64"
# From https://cdn.kernel.org/pub/linux/kernel/v6.x/sha256sums.asc.
sha256="020d45510c539972e6f14c5c71566878d3b7ef56fb082b3e09134f2ebaade562"
image="debian:trixie-20261104"

here="$(cd "$(dirname "$0")" && pwd)"
out_dir="$(cd "$(dirname "$out")" && pwd)"
out_name="$(basename "$out")"
cache="${OWNSAND_KERNEL_CACHE:-$HOME/.cache/ownsand-kernel}"
mkdir -p "$cache"

docker run --rm \
  -v "$here:/src:ro" \
  -v "$out_dir:/out" \
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
    tarball="/cache/linux-$VERSION.tar.xz"
    if [ ! -f "$tarball" ]; then
      curl -fsSL -o "$tarball.download" "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$VERSION.tar.xz"
      mv "$tarball.download" "$tarball"
    fi
    echo "$SHA256  $tarball" | sha256sum -c --quiet -
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
    cp "$built" "/out/$OUT_NAME"
    cp .config "/out/$OUT_NAME.config"
  '
echo "wrote $out ($(shasum -a 256 "$out" | cut -d" " -f1))"
