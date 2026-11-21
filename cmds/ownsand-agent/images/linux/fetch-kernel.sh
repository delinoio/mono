#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Downloads the Linux guest kernel of the Firecracker runtime and checks its
# pinned SHA-256: the 6.1 guest kernel that the Firecracker project
# publishes with its CI artifacts (uncompressed vmlinux, x86_64). The
# project publishes no checksum for it; the hash below was recorded when the
# kernel was first downloaded. A kernel built and signed by the OwnSand
# release process replaces it before the first release.
#
# Usage: fetch-kernel.sh <output-vmlinux>
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <output-vmlinux>" >&2
  exit 2
fi
url="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155"
sha256="e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2"
if [ "$(uname -m)" != "x86_64" ]; then
  echo "only the x86_64 kernel is pinned" >&2
  exit 1
fi
tmp="$1.download"
curl -fsSL -o "$tmp" "$url"
echo "$sha256  $tmp" | sha256sum -c --quiet -
mv "$tmp" "$1"
echo "wrote $1"
