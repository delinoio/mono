#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Generate and check the repository protobuf schemas under protos/ with a
# pinned buf CLI. The owning contract is docs/protos-ownsand-api-contract.md.
#
# Usage:
#   scripts/protos.sh generate          Format the schemas and regenerate the
#                                       committed Go and TypeScript bindings.
#   scripts/protos.sh check             Lint, check formatting and fail when
#                                       the committed Go or TypeScript
#                                       bindings differ from the generated
#                                       output.
#   scripts/protos.sh breaking <base>   Fail on breaking schema changes
#                                       against the Git revision <base>.
set -euo pipefail

# Exact tool versions. protoc-gen-go and protoc-gen-connect-go are pinned in
# protos/buf.gen.yaml; protoc-gen-es is pinned in the package.json of each
# TypeScript target below (packages/ownsand-sdk and apps/delino-accounts).
readonly BUF_MODULE="github.com/bufbuild/buf/cmd/buf"
readonly BUF_VERSION="v1.72.0"

root="$(git rev-parse --show-toplevel)"
readonly root
readonly protos_dir="$root/protos"
# TypeScript targets: "<package directory relative to the root>:<schema
# path under protos/>". Each package owns its template `buf.gen.yaml`, its
# pinned protoc-gen-es and the generated bindings in `src/gen/`.
# - packages/ownsand-sdk: the OwnSand schemas, owned by the TypeScript SDK
#   (docs/packages-ownsand-sdk-foundation.md).
# - apps/delino-accounts: the delino-accounts schemas, owned by the web app
#   (docs/protos-delino-accounts-api-contract.md).
readonly ts_targets=(
  "packages/ownsand-sdk:delino/ownsand"
  "apps/delino-accounts:delino/accounts"
)

buf() {
  (cd "$protos_dir" && go run "${BUF_MODULE}@${BUF_VERSION}" "$@")
}

# Runs `buf generate` for the TypeScript target $1 (package directory
# relative to the root) and schema path $2 into the directory $3. The plugin
# comes from the package's node_modules, which `pnpm install` creates.
buf_generate_ts() {
  local package="$1" schemas="$2" out="$3"
  local plugin_bin="$root/$package/node_modules/.bin"
  if [[ ! -x "$plugin_bin/protoc-gen-es" ]]; then
    echo "protos: $plugin_bin/protoc-gen-es is missing; run pnpm install at the repository root" >&2
    exit 1
  fi
  PATH="$plugin_bin:$PATH" buf generate --template "$root/$package/buf.gen.yaml" --path "$schemas" --output "$out"
}

# Generated Go files, relative to protos/, sorted.
generated_files() {
  local dir="$1"
  (cd "$dir" && find . -type f \( -name '*.pb.go' -o -name '*.connect.go' \) | LC_ALL=C sort)
}

# Generated TypeScript files, relative to the gen directory $1, sorted.
generated_ts_files() {
  local dir="$1"
  if [[ ! -d "$dir" ]]; then
    return 0
  fi
  (cd "$dir" && find . -type f | LC_ALL=C sort)
}

generate() {
  buf format --write
  # Generate every language and target into a temporary directory first. The
  # committed bindings are replaced only after every generator succeeded, so
  # a failing generator or a missing TypeScript plugin leaves every committed
  # binding untouched. buf_generate_ts exits the script on a missing plugin;
  # the EXIT trap still removes the temporary directory then.
  local tmp
  tmp="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '$tmp'" EXIT
  local go_out="$tmp/go"
  mkdir -p "$go_out"
  buf generate --output "$go_out"
  if [[ -z "$(generated_files "$go_out")" ]]; then
    echo "protos: a generator produced no files; the committed bindings are unchanged" >&2
    exit 1
  fi
  local target package schemas i=0
  for target in "${ts_targets[@]}"; do
    package="${target%%:*}"
    schemas="${target#*:}"
    mkdir -p "$tmp/ts$i"
    buf_generate_ts "$package" "$schemas" "$tmp/ts$i"
    if [[ -z "$(generated_ts_files "$tmp/ts$i/src/gen")" ]]; then
      echo "protos: a generator produced no files; the committed bindings are unchanged" >&2
      exit 1
    fi
    i=$((i + 1))
  done

  # Replace the committed bindings. Removing the old generated files first
  # leaves no stale binding behind for a removed schema file.
  local file
  (cd "$protos_dir" && find . -type f \( -name '*.pb.go' -o -name '*.connect.go' \) -delete)
  while IFS= read -r file; do
    mkdir -p "$protos_dir/$(dirname "$file")"
    cp "$go_out/$file" "$protos_dir/$file"
  done < <(generated_files "$go_out")
  i=0
  for target in "${ts_targets[@]}"; do
    package="${target%%:*}"
    rm -rf "$root/$package/src/gen"
    mkdir -p "$root/$package/src/gen"
    cp -R "$tmp/ts$i/src/gen/." "$root/$package/src/gen/"
    i=$((i + 1))
  done
  echo "protos: regenerated Go and TypeScript bindings with buf ${BUF_VERSION}"
}

# Compares the committed TypeScript bindings of the target $1 (package
# directory) and $2 (schema path) with a fresh generation in the temporary
# directory $3.
check_ts() {
  local package="$1" schemas="$2" tmp="$3"
  local gen_dir="$root/$package/src/gen"
  buf_generate_ts "$package" "$schemas" "$tmp"
  if ! diff -u <(generated_ts_files "$gen_dir") <(generated_ts_files "$tmp/src/gen"); then
    echo "protos: the set of committed TypeScript bindings in $package/src/gen differs from the generated output; run scripts/protos.sh generate" >&2
    exit 1
  fi
  local file drift=0
  while IFS= read -r file; do
    if ! cmp -s "$gen_dir/$file" "$tmp/src/gen/$file"; then
      echo "protos: committed binding differs from the generated output: $package/src/gen/${file#./}" >&2
      drift=1
    fi
  done < <(generated_ts_files "$tmp/src/gen")
  if [[ "$drift" -ne 0 ]]; then
    echo "protos: run scripts/protos.sh generate and commit the result" >&2
    exit 1
  fi
}

check() {
  buf lint
  if ! buf format --diff --exit-code; then
    echo "protos: schemas are not formatted; run scripts/protos.sh generate" >&2
    exit 1
  fi
  local tmp
  tmp="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '$tmp'" EXIT
  local go_out="$tmp/go"
  mkdir -p "$go_out"
  buf generate --output "$go_out"
  if ! diff -u <(generated_files "$protos_dir") <(generated_files "$go_out"); then
    echo "protos: the set of committed Go bindings differs from the generated output; run scripts/protos.sh generate" >&2
    exit 1
  fi
  local file drift=0
  while IFS= read -r file; do
    if ! cmp -s "$protos_dir/$file" "$go_out/$file"; then
      echo "protos: committed binding differs from the generated output: protos/${file#./}" >&2
      drift=1
    fi
  done < <(generated_files "$go_out")
  if [[ "$drift" -ne 0 ]]; then
    echo "protos: run scripts/protos.sh generate and commit the result" >&2
    exit 1
  fi
  local target i=0
  for target in "${ts_targets[@]}"; do
    mkdir -p "$tmp/ts$i"
    check_ts "${target%%:*}" "${target#*:}" "$tmp/ts$i"
    i=$((i + 1))
  done
  echo "protos: lint, format and generated Go and TypeScript bindings are up to date"
}

breaking() {
  local base="${1:-}"
  if [[ -z "$base" ]]; then
    echo "protos: breaking needs a base revision" >&2
    exit 2
  fi
  if ! git -C "$root" cat-file -e "${base}^{commit}" 2>/dev/null; then
    echo "protos: base revision $base is not in this checkout" >&2
    exit 2
  fi
  if ! git -C "$root" cat-file -e "${base}:protos/buf.yaml" 2>/dev/null; then
    echo "protos: base revision $base has no protos/buf.yaml; nothing to compare"
    return 0
  fi
  # Extract the base schemas with git archive instead of a buf Git input,
  # because a buf Git input cannot read a linked worktree's `.git` file.
  local tmp
  tmp="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '$tmp'" EXIT
  git -C "$root" archive --format=tar "$base" -- protos | tar -x -C "$tmp"
  buf breaking --against "$tmp/protos"
  echo "protos: no breaking changes against $base"
}

case "${1:-}" in
  generate) generate ;;
  check) check ;;
  breaking) breaking "${2:-}" ;;
  *)
    echo "usage: scripts/protos.sh generate | check | breaking <base>" >&2
    exit 2
    ;;
esac
