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
# TypeScript package below (protos/delino/ownsand, protos/delino/accounts and
# protos/delino/sandrun).
readonly BUF_MODULE="github.com/bufbuild/buf/cmd/buf"
readonly BUF_VERSION="v1.72.0"

root="$(git rev-parse --show-toplevel)"
readonly root
readonly protos_dir="$root/protos"
# TypeScript targets: schema paths under protos/. Each schema path is also
# the directory of a workspace package that owns its template `buf.gen.yaml`,
# its pinned protoc-gen-es and the generated `*_pb.ts` bindings next to the
# schemas.
# - delino/ownsand: @delino/ownsand-protos
#   (docs/protos-ownsand-api-contract.md).
# - delino/accounts: @delino/accounts-protos
#   (docs/protos-delino-accounts-api-contract.md).
# - delino/sandrun: @delino/sandrun-protos (docs/protos-sandrun-api-contract.md).
readonly ts_targets=(
  "delino/ownsand"
  "delino/accounts"
  "delino/sandrun"
)

# The scratch directory that the EXIT trap removes. It is a script-global
# because the functions that create it exit the script directly, and the trap
# runs after their locals are gone. The trap text never contains the path, so
# a temporary path with spaces, quotes or other shell metacharacters is safe.
scratch_dir=""

cleanup() {
  if [[ -n "$scratch_dir" ]]; then
    rm -rf -- "$scratch_dir"
  fi
}
trap cleanup EXIT

buf() {
  (cd "$protos_dir" && go run "${BUF_MODULE}@${BUF_VERSION}" "$@")
}

# Runs `buf generate` for the TypeScript target $1 (schema path under
# protos/, which is also the package directory) into the directory $2. The
# plugin comes from the package's node_modules, which `pnpm install` creates.
buf_generate_ts() {
  local schemas="$1" out="$2"
  local package="$protos_dir/$schemas"
  local plugin_bin="$package/node_modules/.bin"
  if [[ ! -x "$plugin_bin/protoc-gen-es" ]]; then
    echo "protos: $plugin_bin/protoc-gen-es is missing; run pnpm install at the repository root" >&2
    exit 1
  fi
  PATH="$plugin_bin:$PATH" buf generate --template "$package/buf.gen.yaml" --path "$schemas" --output "$out"
}

# Generated Go files, relative to protos/, sorted.
generated_files() {
  local dir="$1"
  (cd "$dir" && find . -type f \( -name '*.pb.go' -o -name '*.connect.go' \) | LC_ALL=C sort)
}

# Generated TypeScript files of the schema path $2 under the directory $1,
# relative to $1, sorted. The package's node_modules and dist are skipped.
generated_ts_files() {
  local dir="$1" schemas="$2"
  if [[ ! -d "$dir/$schemas" ]]; then
    return 0
  fi
  (cd "$dir" && find "./$schemas" \( -name node_modules -o -name dist \) -prune -o -type f -name '*_pb.ts' -print | LC_ALL=C sort)
}

generate() {
  buf format --write
  # Generate every language and target into a temporary directory first. The
  # committed bindings are replaced only after every generator succeeded, so
  # a failing generator or a missing TypeScript plugin leaves every committed
  # binding untouched. buf_generate_ts exits the script on a missing plugin;
  # the EXIT trap still removes the temporary directory then.
  scratch_dir="$(mktemp -d)"
  local tmp="$scratch_dir"
  local go_out="$tmp/go"
  mkdir -p "$go_out"
  buf generate --output "$go_out"
  if [[ -z "$(generated_files "$go_out")" ]]; then
    echo "protos: a generator produced no files; the committed bindings are unchanged" >&2
    exit 1
  fi
  local schemas ts_out="$tmp/ts"
  mkdir -p "$ts_out"
  for schemas in "${ts_targets[@]}"; do
    buf_generate_ts "$schemas" "$ts_out"
    if [[ -z "$(generated_ts_files "$ts_out" "$schemas")" ]]; then
      echo "protos: a generator produced no files; the committed bindings are unchanged" >&2
      exit 1
    fi
  done

  # Replace the committed bindings. Removing the old generated files first
  # leaves no stale binding behind for a removed schema file.
  local file
  (cd "$protos_dir" && find . -type f \( -name '*.pb.go' -o -name '*.connect.go' \) -delete)
  while IFS= read -r file; do
    mkdir -p "$protos_dir/$(dirname "$file")"
    cp "$go_out/$file" "$protos_dir/$file"
  done < <(generated_files "$go_out")
  for schemas in "${ts_targets[@]}"; do
    while IFS= read -r file; do
      rm -f "$protos_dir/$file"
    done < <(generated_ts_files "$protos_dir" "$schemas")
    while IFS= read -r file; do
      mkdir -p "$protos_dir/$(dirname "$file")"
      cp "$ts_out/$file" "$protos_dir/$file"
    done < <(generated_ts_files "$ts_out" "$schemas")
  done
  echo "protos: regenerated Go and TypeScript bindings with buf ${BUF_VERSION}"
}

# Compares the committed TypeScript bindings of the target $1 (schema path)
# with a fresh generation in the temporary directory $2.
check_ts() {
  local schemas="$1" tmp="$2"
  buf_generate_ts "$schemas" "$tmp"
  if ! diff -u <(generated_ts_files "$protos_dir" "$schemas") <(generated_ts_files "$tmp" "$schemas"); then
    echo "protos: the set of committed TypeScript bindings in protos/$schemas differs from the generated output; run scripts/protos.sh generate" >&2
    exit 1
  fi
  local file drift=0
  while IFS= read -r file; do
    if ! cmp -s "$protos_dir/$file" "$tmp/$file"; then
      echo "protos: committed binding differs from the generated output: protos/${file#./}" >&2
      drift=1
    fi
  done < <(generated_ts_files "$tmp" "$schemas")
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
  scratch_dir="$(mktemp -d)"
  local tmp="$scratch_dir"
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
  local schemas
  mkdir -p "$tmp/ts"
  for schemas in "${ts_targets[@]}"; do
    check_ts "$schemas" "$tmp/ts"
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
  scratch_dir="$(mktemp -d)"
  local tmp="$scratch_dir"
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
