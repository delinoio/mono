#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Writes THIRD-PARTY-LICENSES.txt for a server container image: the license
# files of every third-party Go module that the production command package
# links when it is built for linux/amd64 with CGO disabled, exactly like the
# `build-servers` job of `.github/workflows/deploy.yml` builds it.
#
# Usage: scripts/ci/server-notices.sh <service> <package> <output-file>
#   service      the server name, used in the file header.
#   package      the Go package of the production command, relative to the
#                current directory, for example ./servers/accounts-api/cmd/accounts-api.
#   output-file  the path of the bundle. It is written only after discovery
#                succeeds and every linked third-party module has a license file.
#
# The module and license discovery follows `write_notices` in
# `scripts/ci/release-binaries.sh`. Keep the license file expression below in
# sync with it. It is a separate script because that function is tied to the
# binary release plan (per-platform CGO settings) and also writes GO-LICENSE.
set -euo pipefail

if (( $# != 3 )); then
  echo "usage: $0 <service> <package> <output-file>" >&2
  exit 2
fi
service="$1"
package="$2"
out="$3"

# Build settings of the server images (the Droplet runs linux/amd64
# containers on a distroless static base). They decide which modules link.
export GOOS=linux GOARCH=amd64 CGO_ENABLED=0

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
modules="$work/modules"
sorted_modules="$work/sorted-modules"
license_paths="$work/license-paths"
sorted_license_paths="$work/sorted-license-paths"
bundle="$work/THIRD-PARTY-LICENSES.txt"

if ! go list -mod=readonly -deps -buildvcs=false \
  -f '{{with .Module}}{{if not .Main}}{{.Path}}{{"\t"}}{{.Version}}{{"\t"}}{{.Dir}}{{end}}{{end}}' \
  "$package" < /dev/null > "$modules"; then
  echo "::error::Cannot list the Go modules that $service links (go list $package failed)." >&2
  exit 1
fi

if ! sort -u "$modules" > "$sorted_modules"; then
  echo "::error::Cannot sort the Go modules that $service links." >&2
  exit 1
fi

{
  echo "Third-party licenses of $service"
  echo
  echo "This file holds the license files of every Go module that the $service executable links."
} > "$bundle"

count=0
while IFS=$'\t' read -r -u 3 module version module_dir; do
  [[ -n "$module" ]] || continue
  if [[ -z "$module_dir" || ! -d "$module_dir" ]]; then
    echo "::error::Module $module $version that $service links has no source directory; download the modules before you generate the notices." >&2
    exit 1
  fi
  if ! find "$module_dir" -maxdepth 2 -type f \( -ipath "$module_dir/licen[cs]e*" -o -ipath "$module_dir/copying*" -o -ipath "$module_dir/notice*" -o -ipath "$module_dir/patents*" -o -ipath "$module_dir/licenses/*" \) -print0 > "$license_paths"; then
    echo "::error::Cannot find license files for $module $version that $service links." >&2
    exit 1
  fi
  if ! sort -z "$license_paths" > "$sorted_license_paths"; then
    echo "::error::Cannot sort license paths for $module $version that $service links." >&2
    exit 1
  fi
  found=0
  while IFS= read -r -d '' file; do
    found=1
    {
      echo
      echo "================================================================"
      echo "$module $version: ${file#"$module_dir"/}"
      echo "================================================================"
      echo
      cat -- "$file"
    } >> "$bundle"
  done < "$sorted_license_paths"
  if (( found == 0 )); then
    echo "::error::Module $module $version that $service links has no license file in $module_dir." >&2
    exit 1
  fi
  count=$((count + 1))
done 3< "$sorted_modules"

if (( count == 0 )); then
  # Every server links at least one third-party module; an empty list means
  # that the discovery did not see the dependencies.
  echo "::error::The Go package $package of $service links no third-party module; the dependency discovery is incomplete." >&2
  exit 1
fi

mkdir -p -- "$(dirname -- "$out")"
cp -- "$bundle" "$out"
echo "Wrote $out with the licenses of $count third-party modules of $service." >&2
