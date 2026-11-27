#!/bin/bash
set -euo pipefail

root=$(git rev-parse --show-toplevel)
cd "$root"
scratch=$(mktemp -d "$root/.t3-tmp/macos-cef-run.XXXXXX")
mkdir "$scratch"/{temp,mount,hook}.test
mountpoint="$scratch/mount.test"
export TMPDIR="$scratch/temp.test"
export CARGO_TARGET_DIR="$scratch/target"
# Use the CLI's local headless DMG mode to avoid Finder automation.
export CI=true
unset CEF_PATH CARGO_BUILD_TARGET FLATPAK NIX_CEF_BINARY
had_dist=0
if [[ -d "$root/apps/delidev/dist" ]]; then had_dist=1; fi
mounted=0
cleanup() {
  if [[ "$mounted" == 1 ]]; then hdiutil detach "$mountpoint"; fi
  if [[ "$had_dist" == 0 ]]; then rm -rf "$root/apps/delidev/dist"; fi
}
trap cleanup EXIT

git rev-parse HEAD
node --version
pnpm --version
rustc -vV
case "$(rustc -vV | sed -n 's/^host: //p')" in
  aarch64-apple-darwin) arch=aarch64 ;;
  x86_64-apple-darwin) arch=x86_64 ;;
  *) printf '%s\n' 'Run on a native macOS Rust toolchain.' >&2; exit 1 ;;
esac
version=$(awk '/^name = "cef"$/ { getline; gsub(/"/, "", $3); sub(/^[^+]+\+/, "", $3); print $3; exit }' Cargo.lock)
cache="$(node -p "require('node:os').homedir()")/Library/Caches/tauri-cef/$version"
dist="$cache/cef_macos_$arch"
bundle="$CARGO_TARGET_DIR/release/bundle"
app="$bundle/macos/DeliDev.app"
framework='Chromium Embedded Framework.framework'
resources="$app/Contents/Resources"
license="$scratch/LICENSE.txt"
asset="$scratch/provided.txt"
hook_cwd="$scratch/hook.test"

pnpm --filter delidev tauri build --bundles app --no-sign
test -d "$app/Contents/Frameworks/$framework"
metadata="$dist/archive.json"
archive_name=$(jq -er .name "$metadata")
archive="$cache/$archive_name"
sha1=$(jq -er .sha1 "$metadata")
if [[ -f "$dist/LICENSE.txt" ]]; then
  cmp "$dist/LICENSE.txt" "$resources/cef/LICENSE.txt"
fi
test "$(shasum -a 1 "$archive" | awk '{print $1}')" = "$sha1"
tar -xOf "$archive" -- "${archive_name%.tar.bz2}/LICENSE.txt" > "$license"
cmp "$license" "$resources/cef/LICENSE.txt"
cmp "$dist/CREDITS.html" "$resources/cef/CREDITS.html"
mv "$bundle" "$scratch/app-output"

# The standard cache normally has no unpacked license. If someone added one,
# make a disposable versioned cache that forces archive recovery for the DMG.
if [[ -f "$dist/LICENSE.txt" ]]; then
  cache_copy="$scratch/versioned/$version"
  mkdir -p "$cache_copy"
  ditto "$dist" "$cache_copy/cef_macos_$arch"
  rm "$cache_copy/cef_macos_$arch/LICENSE.txt"
  ln -s "$archive" "$cache_copy/$archive_name"
  CEF_PATH="$scratch/versioned" pnpm --filter delidev tauri bundle --bundles dmg --no-sign
else
  pnpm --filter delidev tauri bundle --bundles dmg --no-sign
fi
dmg=$(find "$bundle/dmg" -maxdepth 1 -name '*.dmg' -print)
test -f "$dmg"
hdiutil attach -readonly -nobrowse -mountpoint "$mountpoint" "$dmg"
mounted=1
test -d "$mountpoint/DeliDev.app/Contents/Frameworks/$framework"
cmp "$license" "$mountpoint/DeliDev.app/Contents/Resources/cef/LICENSE.txt"
cmp "$dist/CREDITS.html" "$mountpoint/DeliDev.app/Contents/Resources/cef/CREDITS.html"
hdiutil detach "$mountpoint"
mounted=0
mv "$bundle" "$scratch/dmg-output"

direct="$scratch/direct CEF"
ditto "$dist" "$direct"
cp "$license" "$direct/LICENSE.txt"
printf '%s\n' 'Caller resource' > "$asset"
config=$(jq -nc --arg cwd "$hook_cwd" --arg asset "$asset" '{
  build: { beforeBundleCommand: { cwd:$cwd, script:"test -f \"$DELIDEV_CEF_NOTICE_DIR/LICENSE.txt\" && pwd > called" } },
  bundle: { resources: { ($asset): "provided.txt" } }
}')
CEF_PATH="$direct" pnpm --filter delidev tauri bundle --bundles app --no-sign --config "$config"
cmp "$license" "$resources/cef/LICENSE.txt"
cmp "$dist/CREDITS.html" "$resources/cef/CREDITS.html"
cmp "$asset" "$resources/provided.txt"
test "$(cat "$hook_cwd/called")" = "$(cd "$hook_cwd" && pwd -P)"
mv "$bundle" "$scratch/direct-output"
test -z "$(find "$TMPDIR" -maxdepth 1 -name 'delidev-cef-*' -print)"

bad_root="$scratch/broken"
bad="$bad_root/$version/cef_macos_$arch"
mkdir -p "$bad/$framework"
cp "$dist/"{CREDITS.html,archive.json} "$bad/"
bad_archive="$bad_root/$version/$archive_name"
printf '%s\n' 'corrupt' > "$bad_archive"
rm "$hook_cwd/called"
expect_failure() {
  local log=$1
  local needle=$2
  if CEF_PATH="$bad_root" pnpm --filter delidev tauri bundle --bundles app --no-sign --config "$config" > "$scratch/$log" 2>&1; then
    cat "$scratch/$log"
    printf '%s\n' "Expected failure for $log" >&2
    exit 1
  fi
  grep -F "$needle" "$scratch/$log"
  test ! -e "$hook_cwd/called"
  test ! -e "$app"
  test -z "$(find "$TMPDIR" -maxdepth 1 -name 'delidev-cef-*' -print)"
}
expect_failure corrupt-archive.log "CEF archive checksum mismatch for $bad_archive"
rm "$bad_archive"
expect_failure missing-archive.log "$bad_archive"
rm "$bad/CREDITS.html"
expect_failure missing-credits.log "$bad/CREDITS.html"

rm "$direct/"{LICENSE.txt,CREDITS.html}
shared=$(jq -nc --arg cwd "$hook_cwd" --arg asset "$asset" '{
  build: { beforeBundleCommand: { cwd:$cwd, script:"test -z \"${DELIDEV_CEF_NOTICE_DIR:-}\" && pwd > called" } },
  bundle: { cef: { embed:false }, resources: { ($asset): "provided.txt" } }
}')
CEF_PATH="$direct" pnpm --filter delidev tauri bundle --bundles app --no-sign --config "$shared"
test "$(cat "$hook_cwd/called")" = "$(cd "$hook_cwd" && pwd -P)"
cmp "$asset" "$resources/provided.txt"
test ! -e "$app/Contents/Frameworks/$framework"
test ! -e "$resources/cef"
test -z "$(find "$TMPDIR" -maxdepth 1 -name 'delidev-cef-*' -print)"
shasum -a 256 "$license" "$dist/CREDITS.html"
printf '%s\n' "macOS packaging checks passed. Evidence: $scratch"
