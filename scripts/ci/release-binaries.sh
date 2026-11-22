#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Binary release steps of `.github/workflows/deploy.yml` for the
# `github-release` deployables of the plan (`BINARIES`). Each subcommand is
# one workflow step, so that each secret reaches only the step that needs it:
#
#   build <cross|cgo> <out-dir>   build jobs, no secrets. `cross` builds every
#                                 platform without cgo, writes the license
#                                 notices and builds the release manifest
#                                 tool for the publish job into
#                                 <out-dir>/tools/release-manifest; `cgo`
#                                 builds the cgo platforms on a native runner.
#                                 Darwin executables go to
#                                 <out-dir>/darwin/<name>/, all other files to
#                                 <out-dir>/release/<name>/.
#   sign-macos <in-dir> <out-dir> macOS signing job: Developer ID signature
#                                 with the hardened runtime and notarization.
#                                 Needs the APPLE_* secrets.
#   assemble <artifacts> <assets> publish job, no secrets: checks the
#                                 downloaded artifacts (<artifacts>/release and
#                                 <artifacts>/signed), adds the license files
#                                 of the checkout and writes SHA256SUMS.
#   github-releases <assets>      publish job, GH_TOKEN: checks each tag and
#                                 creates or completes its GitHub Release.
#   fetch-manifests <dir>         publish job, RELEASES_R2_*: downloads the
#                                 current manifest and signature.
#   prepare-manifests <dir> <assets>  publish job, no secrets: adds the
#                                 releases with the prebuilt manifest tool.
#   sign-manifests <dir>          publish job, the signing key only.
#   upload-manifests <dir>        publish job, RELEASES_R2_*: uploads the
#                                 manifest and its signature.
#
# Environment:
#   BINARIES             plan `binaries` JSON (deploy-plan.mjs).
#   REPOSITORY           owner/repository, for the tags and download URLs.
#   SHA                  deployed revision; each tag must sit on a version
#                        commit whose parent is this revision.
#   RUNNER_TEMP          scratch directory.
#   GITHUB_STEP_SUMMARY  optional summary file.
#   RELEASE_MANIFEST_TOOL  release manifest tool that `build cross` built,
#                        for `prepare-manifests` and `sign-manifests`. A job
#                        with a secret runs no build, so these steps never
#                        compile the tool themselves.
#   APPLE_DEVELOPER_ID_CERTIFICATE_P12       base64 PKCS#12 Developer ID
#                                            Application certificate and key.
#   APPLE_DEVELOPER_ID_CERTIFICATE_PASSWORD  its password.
#   APPLE_NOTARY_API_KEY, APPLE_NOTARY_API_KEY_ID, APPLE_NOTARY_API_ISSUER_ID
#                        App Store Connect API key (`.p8` content) for
#                        notarytool.
#   NOTARY_TIMEOUT       notarytool wait limit (default 45m).
#   RELEASES_R2_ACCOUNT_ID, RELEASES_R2_ACCESS_KEY_ID,
#   RELEASES_R2_SECRET_ACCESS_KEY, RELEASES_R2_BUCKET
#                        R2 S3 API credentials of the releases bucket.
#   OWNSAND_AGENT_RELEASE_SIGNING_KEY  PKCS#8 PEM Ed25519 release key.
set -euo pipefail

# Empty arrays under `set -u` and `mapfile` need Bash 4.4 or newer.
if (( BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4) )); then
  echo "::error::$0 needs Bash 4.4 or newer, not $BASH_VERSION." >&2
  exit 2
fi

here="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
summary="${GITHUB_STEP_SUMMARY:-/dev/null}"

usage() {
  echo "usage: $0 build <cross|cgo> <out> | sign-macos <in> <out> | assemble <artifacts> <assets> | github-releases <assets> | fetch-manifests <dir> | prepare-manifests <dir> <assets> | sign-manifests <dir> | upload-manifests <dir>" >&2
  exit 2
}

# Requires the plan `binaries` output: a nonempty JSON list of objects with the
# fields that these steps read, so a missing or malformed output cannot pass
# as "nothing to release".
require_binaries() {
  if ! jq -e 'type == "array" and length > 0 and all(.[];
      type == "object"
      and (.name | type == "string" and test("^[a-z0-9][a-z0-9._-]*$"))
      and (.dir | type == "string" and test("^cmds/[a-z0-9][a-z0-9-]*$"))
      and (.version | type == "string" and test("^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$"))
      and (.binary | type == "string" and test("^[a-z0-9]([a-z0-9-]*[a-z0-9])?$"))
      and (.platforms | type == "array" and length > 0 and all(.[]; type == "string" and test("^(linux|darwin|windows)-(amd64|arm64)$")))
      and (.cgoPlatforms | type == "array" and all(.[]; . == "darwin-arm64"))
      and (.versionVariable | type == "string")
      and (.entitlements | type == "string")
      and (.licenseFiles | type == "array" and length > 0 and all(.[]; type == "string" and length > 0))
      and (.manifestObjectKey | type == "string")
      and (.manifestPublicUrl | type == "string"))' <<< "${BINARIES:-}" > /dev/null 2>&1; then
    echo "::error::BINARIES is not a nonempty list of binary releases: ${BINARIES:-<empty>}" >&2
    exit 1
  fi
}

require_env() {
  local name
  for name in "$@"; do
    if [[ -z "${!name:-}" ]]; then
      echo "::error::$name is not set. Store it as a secret of the releases environment." >&2
      exit 1
    fi
  done
}

# Prints the TSV rows name, version, dir, binary of every binary.
binary_rows() {
  jq -r '.[] | [.name, .version, .dir, .binary] | @tsv' <<< "$BINARIES"
}

# Prints the platforms of a binary that match a jq filter on (platform, cgo).
platforms_of() {
  local name="$1" filter="$2"
  jq -r --arg name "$name" ".[] | select(.name == \$name) | .cgoPlatforms as \$cgo | .platforms[] | . as \$p | (\$cgo | index(\$p) != null) as \$is_cgo | select($filter)" <<< "$BINARIES"
}

field_of() {
  jq -r --arg name "$1" --arg field "$2" '.[] | select(.name == $name) | .[$field] | if type == "array" then join(",") else . end' <<< "$BINARIES"
}

asset_name() {
  local binary="$1" platform="$2"
  if [[ "$platform" == windows-* ]]; then
    echo "$binary-$platform.exe"
  else
    echo "$binary-$platform"
  fi
}

sha256_line() {
  if command -v sha256sum > /dev/null; then
    sha256sum -- "$1"
  else
    shasum -a 256 -- "$1"
  fi
}

# Writes GO-LICENSE (the Go toolchain license; every binary statically links
# the Go standard library and runtime) and THIRD-PARTY-LICENSES.txt with the
# license files of every module that one of the platform builds links.
write_notices() {
  local name="$1" dir="$2" out="$3"
  local platform cgo modules module version module_dir found file
  local goroot
  goroot="$(go env GOROOT < /dev/null)"
  if [[ ! -f "$goroot/LICENSE" ]]; then
    echo "::error::The Go toolchain at $goroot has no LICENSE file to ship as GO-LICENSE." >&2
    exit 1
  fi
  cp "$goroot/LICENSE" "$out/GO-LICENSE"
  modules="$(mktemp)"
  while IFS=$'\t' read -r -u 3 platform cgo; do
    GOOS="${platform%-*}" GOARCH="${platform#*-}" CGO_ENABLED="$cgo" \
      go list -deps -buildvcs=false -f '{{with .Module}}{{if not .Main}}{{.Path}}{{"\t"}}{{.Version}}{{"\t"}}{{.Dir}}{{end}}{{end}}' "./$dir" < /dev/null >> "$modules"
  done 3< <(jq -r --arg name "$name" '.[] | select(.name == $name) | .cgoPlatforms as $cgo | .platforms[] | [., (if ($cgo | index(.)) != null then "1" else "0" end)] | @tsv' <<< "$BINARIES")
  {
    echo "Third-party licenses of $name"
    echo
    echo "This file holds the license files of every Go module that a release executable of $name links."
  } > "$out/THIRD-PARTY-LICENSES.txt"
  while IFS=$'\t' read -r -u 3 module version module_dir; do
    [[ -n "$module" ]] || continue
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
      } >> "$out/THIRD-PARTY-LICENSES.txt"
    done < <(find "$module_dir" -maxdepth 2 -type f \( -ipath "$module_dir/licen[cs]e*" -o -ipath "$module_dir/copying*" -o -ipath "$module_dir/notice*" -o -ipath "$module_dir/patents*" -o -ipath "$module_dir/licenses/*" \) -print0 | sort -z)
    if (( found == 0 )); then
      echo "::error::Module $module $version that $name links has no license file in $module_dir." >&2
      exit 1
    fi
  done 3< <(sort -u "$modules")
  rm -f "$modules"
}

build() {
  local mode="$1" out="$2"
  require_binaries
  case "$mode" in
    cross | cgo) ;;
    *) usage ;;
  esac
  local name version dir binary platform asset target ldflags variable printed
  local host filter
  host="$(go env GOHOSTOS < /dev/null)-$(go env GOHOSTARCH < /dev/null)"
  # jq filters of platforms_of; `$is_cgo` is a jq variable.
  # shellcheck disable=SC2016
  filter='($is_cgo | not)'
  if [[ "$mode" == cgo ]]; then
    # shellcheck disable=SC2016
    filter='$is_cgo'
  fi
  if [[ "$mode" == cross ]]; then
    # The publish job has secrets and therefore runs no build, so it gets the
    # release manifest tool from this job. Build it for this runner, which
    # has the platform of the publish runner, from the standard library only.
    mkdir -p "$out/tools"
    CGO_ENABLED=0 go build -trimpath -buildvcs=false -o "$out/tools/release-manifest" \
      ./cmds/ownsand-agent/release-manifest < /dev/null
    # An update-manifest release must not fail after the tags exist because
    # the agent has no built-in key.
    if has_manifests > /dev/null; then
      RELEASE_MANIFEST_TOOL="$out/tools/release-manifest" run_manifest_tool check-keys
    fi
  fi
  echo "## Binary builds ($mode)" >> "$summary"
  while IFS=$'\t' read -r -u 3 name version dir binary; do
    variable="$(field_of "$name" versionVariable)"
    ldflags="-s -w"
    if [[ -n "$variable" ]]; then
      ldflags+=" -X $variable=$version"
    fi
    while IFS= read -r -u 4 platform; do
      [[ -n "$platform" ]] || continue
      if [[ "$mode" == cgo && "$platform" != "$host" ]]; then
        echo "::error::$name builds $platform with cgo, which needs a $platform runner, not $host." >&2
        exit 1
      fi
      asset="$(asset_name "$binary" "$platform")"
      if [[ "$platform" == darwin-* ]]; then
        target="$out/darwin/$name"
      else
        target="$out/release/$name"
      fi
      mkdir -p "$target"
      echo "::group::Build $name@$version for $platform"
      GOOS="${platform%-*}" GOARCH="${platform#*-}" CGO_ENABLED="$([[ "$mode" == cgo ]] && echo 1 || echo 0)" \
        go build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$target/$asset" "./$dir" < /dev/null
      # `-trimpath` keeps `-ldflags` out of the build information, so run
      # the executable of the host platform: a binary with a version
      # variable prints the version with its `version` command.
      if [[ -n "$variable" && "$platform" == "$host" ]]; then
        printed="$("$target/$asset" version < /dev/null 2>&1 || true)"
        if [[ "$printed" != *"$version"* ]]; then
          echo "::error::$target/$asset version printed ${printed:-nothing}, not the version $version." >&2
          exit 1
        fi
      fi
      echo "::endgroup::"
      echo "- $name@$version: \`$asset\` ($(sha256_line "$target/$asset" | cut -d ' ' -f 1))" >> "$summary"
    done 4< <(platforms_of "$name" "$filter")
    if [[ "$mode" == cross ]]; then
      mkdir -p "$out/release/$name"
      write_notices "$name" "$dir" "$out/release/$name"
    fi
  done 3< <(binary_rows)
}

# Runs the prebuilt release manifest tool. It never falls back to `go run`,
# because the steps that call it run in a job with secrets.
run_manifest_tool() {
  local tool="${RELEASE_MANIFEST_TOOL:-}"
  if [[ -z "$tool" || -L "$tool" || ! -f "$tool" ]]; then
    echo "::error::RELEASE_MANIFEST_TOOL is not the release manifest tool that build cross built: ${tool:-<empty>}" >&2
    exit 1
  fi
  # A downloaded artifact loses its executable bit.
  [[ -x "$tool" ]] || chmod -- u+x "$tool"
  "$tool" "$@" < /dev/null
}

keychain=""
signing_work=""
cleanup_signing() {
  if [[ -n "$keychain" ]]; then
    security delete-keychain "$keychain" > /dev/null 2>&1 || true
  fi
  if [[ -n "$signing_work" ]]; then
    rm -rf -- "$signing_work"
  fi
}

# Signs and notarizes every darwin executable. A bare executable cannot carry
# a stapled ticket, so notarization is verified with the notarytool status
# and an online `codesign --check-notarization` check, and Gatekeeper checks
# the ticket online on a user's Mac.
sign_macos() {
  local in="$1" out="$2"
  require_binaries
  require_env APPLE_DEVELOPER_ID_CERTIFICATE_P12 APPLE_DEVELOPER_ID_CERTIFICATE_PASSWORD \
    APPLE_NOTARY_API_KEY APPLE_NOTARY_API_KEY_ID APPLE_NOTARY_API_ISSUER_ID
  local work="$RUNNER_TEMP/macos-signing"
  rm -rf "$work"
  mkdir -p "$work"
  chmod 700 "$work"
  keychain="$work/release.keychain-db"
  signing_work="$work"
  local keychain_password
  keychain_password="$(openssl rand -hex 32)"
  # The keychain and the key files live only for this job; remove them also
  # on failure.
  trap cleanup_signing EXIT
  ( umask 077; base64 --decode > "$work/certificate.p12" <<< "$APPLE_DEVELOPER_ID_CERTIFICATE_P12" )
  ( umask 077; printf '%s\n' "$APPLE_NOTARY_API_KEY" > "$work/notary-key.p8" )
  security create-keychain -p "$keychain_password" "$keychain"
  security set-keychain-settings -lut 21600 "$keychain"
  security unlock-keychain -p "$keychain_password" "$keychain"
  security import "$work/certificate.p12" -k "$keychain" -P "$APPLE_DEVELOPER_ID_CERTIFICATE_PASSWORD" -T /usr/bin/codesign > /dev/null
  security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$keychain_password" "$keychain" > /dev/null
  rm -f "$work/certificate.p12"
  local identities identity
  identities="$(security find-identity -v -p codesigning "$keychain" | grep -c '"Developer ID Application: ' || true)"
  if [[ "$identities" != 1 ]]; then
    echo "::error::APPLE_DEVELOPER_ID_CERTIFICATE_P12 must hold exactly one Developer ID Application identity, found $identities." >&2
    exit 1
  fi
  identity="$(security find-identity -v -p codesigning "$keychain" | sed -n 's/.*"\(Developer ID Application: .*\)"$/\1/p')"
  echo "## macOS signing" >> "$summary"
  echo "Identity: \`$identity\`" >> "$summary"
  local name version dir binary platform asset source target entitlements submission status id
  while IFS=$'\t' read -r -u 3 name version dir binary; do
    entitlements="$(field_of "$name" entitlements)"
    while IFS= read -r -u 4 platform; do
      [[ -n "$platform" ]] || continue
      asset="$(asset_name "$binary" "$platform")"
      source="$in/$name/$asset"
      if [[ ! -f "$source" || -L "$source" ]]; then
        echo "::error::The build artifact has no regular file $name/$asset." >&2
        exit 1
      fi
      mkdir -p "$out/$name"
      target="$out/$name/$asset"
      cp -- "$source" "$target"
      chmod 755 "$target"
      echo "::group::Sign and notarize $name@$version for $platform"
      local args=(--force --sign "$identity" --keychain "$keychain" --options runtime --timestamp --identifier "io.delino.$binary")
      if [[ -n "$entitlements" ]]; then
        args+=(--entitlements "$repo_root/$entitlements")
      fi
      codesign "${args[@]}" "$target" < /dev/null
      codesign --verify --strict --verbose=2 "$target" < /dev/null
      if [[ -n "$entitlements" ]] && ! codesign --display --entitlements - --xml "$target" < /dev/null 2> /dev/null | grep -q 'com.apple.security.virtualization'; then
        echo "::error::$asset has no com.apple.security.virtualization entitlement after signing." >&2
        exit 1
      fi
      # notarytool accepts a zip, a disk image or a package, not a bare
      # executable.
      ditto -c -k --keepParent "$target" "$work/$asset.zip" < /dev/null
      submission="$(xcrun notarytool submit "$work/$asset.zip" --key "$work/notary-key.p8" \
        --key-id "$APPLE_NOTARY_API_KEY_ID" --issuer "$APPLE_NOTARY_API_ISSUER_ID" \
        --wait --timeout "${NOTARY_TIMEOUT:-45m}" --output-format json < /dev/null)" || true
      status="$(jq -r '.status // empty' <<< "$submission" 2> /dev/null || true)"
      id="$(jq -r '.id // empty' <<< "$submission" 2> /dev/null || true)"
      if [[ "$status" != Accepted ]]; then
        echo "::error::Notarization of $asset ended with status ${status:-unknown} (submission ${id:-none})." >&2
        if [[ -n "$id" ]]; then
          xcrun notarytool log "$id" --key "$work/notary-key.p8" --key-id "$APPLE_NOTARY_API_KEY_ID" \
            --issuer "$APPLE_NOTARY_API_ISSUER_ID" < /dev/null >&2 || true
        fi
        exit 1
      fi
      # `spctl --assess` has types only for apps (execute), disk images
      # (open) and installer packages (install), not for a bare
      # command-line executable. Apple's DTS guidance checks such code with
      # the `notarized` code requirement, which finds the ticket online.
      if ! codesign -vvvv -R=notarized --check-notarization "$target" < /dev/null > "$work/notarization-check.txt" 2>&1; then
        echo "::error::codesign does not accept $asset as notarized: $(tr '\n' ' ' < "$work/notarization-check.txt")" >&2
        exit 1
      fi
      rm -f "$work/$asset.zip"
      echo "::endgroup::"
      echo "- $name@$version: \`$asset\`, notarization \`$id\` Accepted" >> "$summary"
    done 4< <(platforms_of "$name" 'startswith("darwin-")')
  done 3< <(binary_rows)
}

# Copies one artifact file after checking that it is a regular file.
take() {
  local source="$1" target="$2"
  if [[ ! -f "$source" || -L "$source" ]]; then
    echo "::error::Missing or not a regular file: $source" >&2
    exit 1
  fi
  cp -- "$source" "$target"
}

assemble() {
  local artifacts="$1" assets="$2"
  require_binaries
  local name version dir binary platform asset file entry
  local -A expected
  while IFS=$'\t' read -r -u 3 name version dir binary; do
    mkdir -p "$assets/$name"
    expected=()
    while IFS= read -r -u 4 platform; do
      asset="$(asset_name "$binary" "$platform")"
      if [[ "$platform" == darwin-* ]]; then
        expected["signed/$name/$asset"]=1
        take "$artifacts/signed/$name/$asset" "$assets/$name/$asset"
      else
        expected["release/$name/$asset"]=1
        take "$artifacts/release/$name/$asset" "$assets/$name/$asset"
      fi
      chmod 755 "$assets/$name/$asset"
    done 4< <(platforms_of "$name" 'true')
    for file in GO-LICENSE THIRD-PARTY-LICENSES.txt; do
      expected["release/$name/$file"]=1
      take "$artifacts/release/$name/$file" "$assets/$name/$file"
    done
    # The build artifacts must hold exactly the planned files: no symlink,
    # special file, directory or extra file may become a release asset.
    while IFS= read -r -d '' entry; do
      if [[ -z "${expected[$entry]:-}" ]]; then
        echo "::error::The artifacts of $name contain an unexpected entry: $entry" >&2
        exit 1
      fi
    done < <(cd "$artifacts" && find "release/$name" "signed/$name" -mindepth 1 -print0 2> /dev/null)
    while IFS= read -r -u 4 file; do
      take "$repo_root/$file" "$assets/$name/$(basename "$file")"
    done 4< <(jq -r --arg name "$name" '.[] | select(.name == $name) | .licenseFiles[]' <<< "$BINARIES")
    (cd "$assets/$name" && for file in *; do sha256_line "$file"; done) > "$assets/SHA256SUMS.tmp"
    mv "$assets/SHA256SUMS.tmp" "$assets/$name/SHA256SUMS"
  done 3< <(binary_rows)
}

# Checks that the tag exists, is annotated and sits on a commit whose first
# parent is the deployed revision, which the release job created.
check_tag() {
  local tag="$1" ref object commit parent
  ref="$(gh api "repos/$REPOSITORY/git/ref/tags/$tag" --jq '[.object.type, .object.sha] | @tsv' < /dev/null)" || {
    echo "::error::Tag $tag does not exist. The release job must push it first." >&2
    exit 1
  }
  if [[ "${ref%%$'\t'*}" != tag ]]; then
    echo "::error::Tag $tag is not an annotated tag of the release job." >&2
    exit 1
  fi
  object="${ref#*$'\t'}"
  commit="$(gh api "repos/$REPOSITORY/git/tags/$object" --jq 'select(.object.type == "commit") | .object.sha' < /dev/null)"
  parent="$(gh api "repos/$REPOSITORY/commits/$commit" --jq '.parents[0].sha' < /dev/null)"
  if [[ -z "$commit" || "$parent" != "$SHA" ]]; then
    echo "::error::Tag $tag is not on the version commit of the deployed revision $SHA (commit ${commit:-none}, parent ${parent:-none})." >&2
    exit 1
  fi
}

github_releases() {
  local assets="$1"
  require_binaries
  require_env REPOSITORY SHA GH_TOKEN
  echo "## GitHub Releases" >> "$summary"
  local name version dir binary tag files view notes
  while IFS=$'\t' read -r -u 3 name version dir binary; do
    tag="$name@$version"
    check_tag "$tag"
    mapfile -t files < <(find "$assets/$name" -maxdepth 1 -type f -print | sort)
    if (( ${#files[@]} == 0 )); then
      echo "::error::No assets for $tag; run assemble first." >&2
      exit 1
    fi
    if view="$(gh release view "$tag" --repo "$REPOSITORY" --json isDraft --jq .isDraft < /dev/null 2>&1)"; then
      # A rerun after a failed later step: the release exists; replace the
      # assets with the ones of this run.
      echo "::notice::Release $tag exists; uploading its assets again."
      gh release upload "$tag" --repo "$REPOSITORY" --clobber "${files[@]}" < /dev/null
      if [[ "$view" == true ]]; then
        gh release edit "$tag" --repo "$REPOSITORY" --draft=false < /dev/null
      fi
    elif [[ "$view" == *"release not found"* ]]; then
      notes="Release of \`$name\` $version from $SHA. See [the changelog](https://github.com/$REPOSITORY/blob/$name%40$version/$dir/CHANGELOG.md). Verify the downloads with \`SHA256SUMS\`."
      # Not the repository's "latest" release: the tags of every package
      # share one release list.
      gh release create "$tag" --repo "$REPOSITORY" --verify-tag --title "$tag" \
        --notes "$notes" --latest=false "${files[@]}" < /dev/null
    else
      echo "::error::Could not read release $tag: $view" >&2
      exit 1
    fi
    echo "- [\`$tag\`](https://github.com/$REPOSITORY/releases/tag/$name%40$version): ${#files[@]} assets" >> "$summary"
  done 3< <(binary_rows)
}

# Prints the rows name, version, binary, object key and public URL of every
# binary with an update manifest.
manifest_rows() {
  jq -r '.[] | select(.manifestObjectKey != "") | [.name, .version, .binary, .manifestObjectKey, .manifestPublicUrl] | @tsv' <<< "$BINARIES"
}

# Returns success when a planned binary has an update manifest. The manifest
# steps do nothing, and need no secret, for a run without one.
has_manifests() {
  if jq -e 'any(.[]; .manifestObjectKey != "")' <<< "$BINARIES" > /dev/null; then
    return 0
  fi
  echo "::notice::No planned binary release has an update manifest."
  return 1
}

r2_url() {
  echo "https://$RELEASES_R2_ACCOUNT_ID.r2.cloudflarestorage.com/$RELEASES_R2_BUCKET/$1"
}

# curl configuration with the R2 credentials, so that they are not in the
# process arguments.
r2_config() {
  printf 'user = "%s:%s"\n' "$RELEASES_R2_ACCESS_KEY_ID" "$RELEASES_R2_SECRET_ACCESS_KEY"
  printf 'aws-sigv4 = "aws:amz:auto:s3"\n'
}

# Downloads one object. Prints the HTTP status: 200, or 404 when the object
# does not exist; any other result fails.
r2_get() {
  local key="$1" out="$2" code
  code="$(curl --config <(r2_config) -sS --retry 3 -o "$out" -w '%{http_code}' "$(r2_url "$key")" < /dev/null)" || {
    echo "::error::Could not download $key from the releases bucket." >&2
    exit 1
  }
  case "$code" in
    200 | 404) echo "$code" ;;
    *)
      echo "::error::Downloading $key from the releases bucket returned HTTP $code." >&2
      exit 1
      ;;
  esac
}

r2_put() {
  local key="$1" file="$2" type="$3"
  curl --config <(r2_config) -fsS --retry 3 -X PUT -H "Content-Type: $type" \
    -H "Cache-Control: no-cache" --data-binary "@$file" "$(r2_url "$key")" -o /dev/null < /dev/null
}

require_r2() {
  require_env RELEASES_R2_ACCOUNT_ID RELEASES_R2_ACCESS_KEY_ID RELEASES_R2_SECRET_ACCESS_KEY RELEASES_R2_BUCKET
  if [[ ! "$RELEASES_R2_ACCOUNT_ID" =~ ^[0-9a-f]{32}$ || ! "$RELEASES_R2_BUCKET" =~ ^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$ ]]; then
    echo "::error::RELEASES_R2_ACCOUNT_ID or RELEASES_R2_BUCKET is not a Cloudflare account ID or R2 bucket name." >&2
    exit 1
  fi
}

fetch_manifests() {
  local dir="$1"
  require_binaries
  has_manifests || return 0
  require_r2
  local name version binary key url manifest_code sig_code
  while IFS=$'\t' read -r -u 3 name version binary key url; do
    mkdir -p "$dir/$name"
    rm -f "$dir/$name/current.json" "$dir/$name/current.json.sig"
    manifest_code="$(r2_get "$key" "$dir/$name/current.json")"
    sig_code="$(r2_get "$key.sig" "$dir/$name/current.json.sig")"
    if [[ "$manifest_code" == 404 && "$sig_code" == 404 ]]; then
      # The first release starts an empty manifest.
      rm -f "$dir/$name/current.json" "$dir/$name/current.json.sig"
      echo "::notice::$key does not exist yet; $name@$version starts a new manifest."
    elif [[ "$manifest_code" != 200 || "$sig_code" != 200 ]]; then
      echo "::error::The releases bucket has only one of $key and $key.sig (HTTP $manifest_code and $sig_code). Restore both before a new release." >&2
      exit 1
    fi
  done 3< <(manifest_rows)
}

prepare_manifests() {
  local dir="$1" assets="$2"
  require_binaries
  has_manifests || return 0
  require_env REPOSITORY
  local name version binary key url args
  while IFS=$'\t' read -r -u 3 name version binary key url; do
    mkdir -p "$dir/$name"
    args=(add-release --version "$version" --binary "$binary" --platforms "$(field_of "$name" platforms)"
      --artifacts "$assets/$name" --url-prefix "https://github.com/$REPOSITORY/releases/download/$name%40$version"
      --out "$dir/$name/manifest.json")
    if [[ -f "$dir/$name/current.json" ]]; then
      args+=(--current "$dir/$name/current.json" --current-signature "$dir/$name/current.json.sig")
    fi
    run_manifest_tool "${args[@]}"
  done 3< <(manifest_rows)
}

sign_manifests() {
  local dir="$1"
  require_binaries
  has_manifests || return 0
  require_env OWNSAND_AGENT_RELEASE_SIGNING_KEY
  local name version binary key url
  while IFS=$'\t' read -r -u 3 name version binary key url; do
    run_manifest_tool sign --manifest "$dir/$name/manifest.json" --out "$dir/$name/manifest.json.sig"
    run_manifest_tool verify --manifest "$dir/$name/manifest.json" --signature "$dir/$name/manifest.json.sig"
  done 3< <(manifest_rows)
}

upload_manifests() {
  local dir="$1"
  require_binaries
  has_manifests || return 0
  require_r2
  echo "## Release manifests" >> "$summary"
  local name version binary key url public
  while IFS=$'\t' read -r -u 3 name version binary key url; do
    for file in manifest.json manifest.json.sig; do
      if [[ ! -s "$dir/$name/$file" ]]; then
        echo "::error::$dir/$name/$file is missing; run prepare-manifests and sign-manifests first." >&2
        exit 1
      fi
    done
    # The manifest first, then its signature: an agent that reads between
    # the two uploads fails verification and retries with a later update.
    r2_put "$key" "$dir/$name/manifest.json" application/json
    r2_put "$key.sig" "$dir/$name/manifest.json.sig" text/plain
    # The public URL is the custom domain of the bucket. A mismatch is a
    # warning only: the objects are already published, and a CDN cache can
    # serve the previous object for a short time.
    public="$(mktemp)"
    if curl -fsS --retry 3 -H "Cache-Control: no-cache" -o "$public" "$url" < /dev/null && cmp -s "$public" "$dir/$name/manifest.json"; then
      echo "- $name@$version: \`$url\` serves the new manifest" >> "$summary"
    else
      echo "::warning::$url does not serve the uploaded manifest yet. Check the custom domain of the releases bucket."
      echo "- $name@$version: uploaded \`$key\`; \`$url\` does not serve it yet" >> "$summary"
    fi
    rm -f "$public"
  done 3< <(manifest_rows)
}

command="${1:-}"
case "$command" in
  build | sign-macos | assemble | prepare-manifests) (( $# == 3 )) || usage ;;
  github-releases | fetch-manifests | sign-manifests | upload-manifests) (( $# == 2 )) || usage ;;
  *) usage ;;
esac

case "$command" in
  build) build "$2" "$3" ;;
  sign-macos) sign_macos "$2" "$3" ;;
  assemble) assemble "$2" "$3" ;;
  github-releases) github_releases "$2" ;;
  fetch-manifests) fetch_manifests "$2" ;;
  prepare-manifests) prepare_manifests "$2" "$3" ;;
  sign-manifests) sign_manifests "$2" ;;
  upload-manifests) upload_manifests "$2" ;;
esac
