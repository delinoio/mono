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
# - A download goes to a private staging directory in the cache and is
#   verified before an atomic rename publishes it. Both paths are on one
#   file system, so no build sees a partial archive. A failed download or
#   checksum mismatch leaves the published path untouched.
# - A directory at the archive path stays untouched, even if it appears
#   during the download. The helper reports the conflict and exits nonzero.
#   It removes its staging directory on exit, including on INT and TERM.
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

archive_name="linux-$version.tar.xz"
archive="$cache/$archive_name"

# Prints nothing; the exit status says whether the file has the pinned checksum.
verify() {
  echo "$sha256  $1" | sha256sum -c --quiet - >/dev/null 2>&1
}

if [ -d "$archive" ]; then
  echo "the kernel source path $archive is a directory; remove it and run the build again" >&2
  exit 1
fi
if [ -f "$archive" ] && verify "$archive"; then
  echo "using the cached kernel source $archive" >&2
  exit 0
fi
if [ -e "$archive" ]; then
  echo "the cached kernel source $archive does not match its checksum; downloading it again" >&2
fi

mkdir -p "$cache"
stage="$(mktemp -d "$archive.download.XXXXXX")"
trap 'rm -rf -- "$stage"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
tmp="$stage/$archive_name"

curl -fsSL -o "$tmp" "$url"
if ! verify "$tmp"; then
  echo "the downloaded kernel source from $url does not match SHA-256 $sha256" >&2
  exit 1
fi
chmod 0644 "$tmp"
# Let mv append the archive basename to the cache directory. The rename then
# fails if the final archive path has become a directory.
if ! mv -f -- "$tmp" "$cache/"; then
  if [ -d "$archive" ]; then
    echo "the kernel source path $archive became a directory; remove it and run the build again" >&2
  else
    echo "could not publish the kernel source $archive" >&2
  fi
  exit 1
fi
echo "cached the kernel source $archive" >&2
