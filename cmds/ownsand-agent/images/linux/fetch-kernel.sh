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
# The download goes to a unique temporary file next to <output-vmlinux> and is
# verified before it is published, so fetches that run at the same time (two
# builds, a retry beside a slow run) never share a staging file. The rename
# that publishes it is atomic because both paths are on one file system. A
# download that fails or does not match the checksum leaves an existing
# <output-vmlinux> untouched, and the temporary file is removed whatever way
# the script ends.
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
tmp="$(mktemp "$1.download.XXXXXX")"
# Remove the temporary file whatever way the script ends, also on a signal.
trap 'rm -f "$tmp"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
curl -fsSL -o "$tmp" "$url"
echo "$sha256  $tmp" | sha256sum -c --quiet -
# mktemp creates the file with mode 0600; a published kernel is readable.
chmod 0644 "$tmp"
mv -f "$tmp" "$1"
echo "wrote $1"
