#!/usr/bin/env bash
set -euo pipefail

expected_sha="${1:?expected commit SHA required}"
repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"
actual_sha="$(git rev-parse HEAD)"
if [[ "$actual_sha" != "$expected_sha" ]]; then
  printf 'HEAD is %s, expected %s\n' "$actual_sha" "$expected_sha" >&2
  exit 1
fi
printf 'Validating %s\n' "$actual_sha"
sw_vers
go version

mkdir -p "$repo_root/.t3-tmp"
export TMPDIR="$repo_root/.t3-tmp"
scratch="$(mktemp -d "$TMPDIR/download-aliases-apfs.XXXXXX")"
mountpoint="$scratch/volume"
image="$scratch/staging.sparseimage"
mkdir "$mountpoint"
mounted=0
cleanup() {
  if (( mounted == 1 )); then
    hdiutil detach "$mountpoint" || return 1
  fi
  rm -rf "$scratch"
}
trap cleanup EXIT

# APFS is the case-insensitive variant of the disk image format.
hdiutil create -size 512m -type SPARSE -fs APFS -volname SandRunDownloadAliases "$image"
hdiutil attach -nobrowse -mountpoint "$mountpoint" "$image"
mounted=1
diskutil info "$mountpoint"
mkdir "$mountpoint/ProbeName"
[[ -d "$mountpoint/probename" ]]
rmdir "$mountpoint/ProbeName"

# Use the mounted filesystem for the test's destination and staging paths.
# macOS may use Darwin's temporary directory for build output, so compile
# separately and set TMPDIR to the volume when running the test binary.
go test -c -o "$scratch/sandrun.test" ./go/sandrun
log="$TMPDIR/macos-download-directory-aliases.log"
TMPDIR="$mountpoint" "$scratch/sandrun.test" -test.v -test.count=1 \
  -test.run '^TestDownload(DirectoryCaseCollision|ArchiveRepeatedDirectoryName)$' | tee "$log"
grep -F 'the staging volume aliases "Reports" and "reports": true' "$log"
for test_case in explicit implicit; do
  for existing in false true; do
    grep -F -- "--- PASS: TestDownloadDirectoryCaseCollision/$test_case/existing=$existing " "$log"
  done
done
grep -F -- '--- PASS: TestDownloadArchiveRepeatedDirectoryName ' "$log"
