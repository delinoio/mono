#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Fills the Linux kernel source cache of build-kernel.sh with the pinned
# source archive: linux-<version>.tar.xz in <cache-dir>, whose SHA-256 is
# <sha256>. Builds that share one cache directory (a cold cache, two builds at
# once) can run this at the same time:
#
# - A cached archive that matches the checksum is reused and nothing is
#   downloaded. A cached archive that does not match (a truncated or corrupt
#   file that an earlier build published) counts as a cache miss and is
#   replaced.
# - A download goes to a unique temporary file in the cache directory and is
#   verified before it is published. The rename that publishes it is atomic
#   because both paths are on one file system, so no build ever sees a
#   partial archive, and a download that fails or does not match the checksum
#   leaves the published path (and any valid archive there) untouched.
#
# The helper needs no lock: concurrent fills each publish a verified archive
# with identical content. A lock would not help either, because flock is
# unreliable on the Docker volumes of macOS hosts.
#
# Usage: fetch-kernel-source.sh <version> <sha256> <url> <cache-dir>
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <version> <sha256> <url> <cache-dir>" >&2
  exit 2
fi
version="$1"
sha256="$2"
url="$3"
cache="$4"

archive="$cache/linux-$version.tar.xz"

# Prints nothing; the exit status says whether the file has the pinned checksum.
verify() {
  echo "$sha256  $1" | sha256sum -c --quiet - >/dev/null 2>&1
}

if [ -f "$archive" ] && verify "$archive"; then
  echo "using the cached kernel source $archive" >&2
  exit 0
fi
if [ -e "$archive" ]; then
  echo "the cached kernel source $archive does not match its checksum; downloading it again" >&2
fi

mkdir -p "$cache"
tmp="$(mktemp "$archive.download.XXXXXX")"
# Remove the temporary file whatever way the helper ends, also on a signal.
trap 'rm -f "$tmp"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

curl -fsSL -o "$tmp" "$url"
if ! verify "$tmp"; then
  echo "the downloaded kernel source from $url does not match SHA-256 $sha256" >&2
  exit 1
fi
# mktemp creates the file with mode 0600; a published archive is readable.
chmod 0644 "$tmp"
mv -f "$tmp" "$archive"
echo "cached the kernel source $archive" >&2
