#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Generate and check the repository protobuf schemas under protos/ with a
# pinned buf CLI. The owning contract is docs/protos-ownsand-api-contract.md.
#
# Usage:
#   scripts/protos.sh generate          Format the schemas and regenerate the
#                                       committed Go bindings.
#   scripts/protos.sh check             Lint, check formatting and fail when
#                                       the committed Go bindings differ from
#                                       the generated output.
#   scripts/protos.sh breaking <base>   Fail on breaking schema changes
#                                       against the Git revision <base>.
set -euo pipefail

# Exact tool versions. protoc-gen-go and protoc-gen-connect-go are pinned in
# protos/buf.gen.yaml.
readonly BUF_MODULE="github.com/bufbuild/buf/cmd/buf"
readonly BUF_VERSION="v1.74.0"

root="$(git rev-parse --show-toplevel)"
readonly root
readonly protos_dir="$root/protos"

buf() {
  (cd "$protos_dir" && go run "${BUF_MODULE}@${BUF_VERSION}" "$@")
}

# Generated Go files, relative to protos/, sorted.
generated_files() {
  local dir="$1"
  (cd "$dir" && find . -type f \( -name '*.pb.go' -o -name '*.connect.go' \) | LC_ALL=C sort)
}

generate() {
  buf format --write
  # Remove the old generated files first, so that a removed schema file
  # leaves no stale binding behind.
  (cd "$protos_dir" && find . -type f \( -name '*.pb.go' -o -name '*.connect.go' \) -delete)
  buf generate
  echo "protos: regenerated Go bindings with buf ${BUF_VERSION}"
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
  buf generate --output "$tmp"
  if ! diff -u <(generated_files "$protos_dir") <(generated_files "$tmp"); then
    echo "protos: the set of committed Go bindings differs from the generated output; run scripts/protos.sh generate" >&2
    exit 1
  fi
  local file drift=0
  while IFS= read -r file; do
    if ! cmp -s "$protos_dir/$file" "$tmp/$file"; then
      echo "protos: committed binding differs from the generated output: protos/${file#./}" >&2
      drift=1
    fi
  done < <(generated_files "$tmp")
  if [[ "$drift" -ne 0 ]]; then
    echo "protos: run scripts/protos.sh generate and commit the result" >&2
    exit 1
  fi
  echo "protos: lint, format and generated Go bindings are up to date"
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
