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
# protos/buf.gen.yaml; protoc-gen-es is pinned in
# packages/ownsand-sdk/package.json.
readonly BUF_MODULE="github.com/bufbuild/buf/cmd/buf"
readonly BUF_VERSION="v1.72.0"

root="$(git rev-parse --show-toplevel)"
readonly root
readonly protos_dir="$root/protos"
# TypeScript bindings of the OwnSand schemas, owned by the TypeScript SDK
# (docs/packages-ownsand-sdk-foundation.md).
readonly ts_package_dir="$root/packages/ownsand-sdk"
readonly ts_template="$ts_package_dir/buf.gen.yaml"
readonly ts_gen_dir="$ts_package_dir/src/gen"

buf() {
  (cd "$protos_dir" && go run "${BUF_MODULE}@${BUF_VERSION}" "$@")
}

# Runs `buf generate` with the TypeScript template into the package
# directory $1. The plugin comes from the package's node_modules, which
# `pnpm install` creates.
buf_generate_ts() {
  local out="$1"
  local plugin_bin="$ts_package_dir/node_modules/.bin"
  if [[ ! -x "$plugin_bin/protoc-gen-es" ]]; then
    echo "protos: $plugin_bin/protoc-gen-es is missing; run pnpm install at the repository root" >&2
    exit 1
  fi
  PATH="$plugin_bin:$PATH" buf generate --template "$ts_template" --path delino/ownsand --output "$out"
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
  # Remove the old generated files first, so that a removed schema file
  # leaves no stale binding behind.
  (cd "$protos_dir" && find . -type f \( -name '*.pb.go' -o -name '*.connect.go' \) -delete)
  buf generate
  rm -rf "$ts_gen_dir"
  buf_generate_ts "$ts_package_dir"
  echo "protos: regenerated Go and TypeScript bindings with buf ${BUF_VERSION}"
}

# Compares the committed TypeScript bindings with a fresh generation in a
# temporary directory.
check_ts() {
  local tmp="$1"
  buf_generate_ts "$tmp"
  if ! diff -u <(generated_ts_files "$ts_gen_dir") <(generated_ts_files "$tmp/src/gen"); then
    echo "protos: the set of committed TypeScript bindings differs from the generated output; run scripts/protos.sh generate" >&2
    exit 1
  fi
  local file drift=0
  while IFS= read -r file; do
    if ! cmp -s "$ts_gen_dir/$file" "$tmp/src/gen/$file"; then
      echo "protos: committed binding differs from the generated output: packages/ownsand-sdk/src/gen/${file#./}" >&2
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
  local go_out="$tmp/go" ts_out="$tmp/ts"
  mkdir -p "$go_out" "$ts_out"
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
  check_ts "$ts_out"
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
