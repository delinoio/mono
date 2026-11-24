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
# Each fetch stages its download in a unique directory next to <output-vmlinux>.
# It verifies the download before publishing it with an atomic rename on the
# same file system. A failed download or checksum mismatch leaves an existing
# kernel untouched. A directory at the output path stays untouched even if it
# appears during the fetch. The script removes its staging directory on exit.
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

out="$1"
reject_directory() {
  if [ -d "$out" ]; then
    echo "kernel output path is a directory: $out" >&2
    exit 1
  fi
}
reject_directory

out_name="${out##*/}"
if [ -z "$out_name" ]; then
  echo "kernel output path must name a file: $out" >&2
  exit 1
fi
out_dir=./
case "$out" in
  /*) out_dir="${out%/*}/" ;;
  */*) out_dir="./${out%/*}/" ;;
esac

stage="$(mktemp -d "${out_dir}${out_name}.download.XXXXXX")"
trap 'rm -rf -- "$stage"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
tmp="$stage/$out_name"
curl -fsSL -o "$tmp" "$url"
echo "$sha256  $tmp" | sha256sum -c --quiet -
chmod 0644 "$tmp"

# Let mv append the basename to the parent directory. The rename then fails
# if the final filename has become a directory.
if ! mv -f -- "$tmp" "$out_dir"; then
  reject_directory
  echo "could not publish the kernel at $out" >&2
  exit 1
fi
echo "wrote $out"
