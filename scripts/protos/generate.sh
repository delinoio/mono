#!/usr/bin/env bash
# Lint the protobuf schemas under protos/ and regenerate the committed Go
# bindings. Pass --check to fail when the committed bindings differ from the
# generated output instead of leaving the regenerated files in place.
set -euo pipefail

# Pinned code generation tools. buf and the Go plugins run through
# `go run <module>@<version>`, which uses the module cache and does not add
# them to the root go.mod. Keep these versions in step with
# docs/protos-ownsand-api-contract.md.
readonly BUF="github.com/bufbuild/buf/cmd/buf@v1.74.0"

root="$(git rev-parse --show-toplevel)"
cd "$root/protos"

go run "$BUF" lint
go run "$BUF" generate

if [[ "${1:-}" == "--check" ]]; then
  cd "$root"
  if ! git diff --exit-code --stat -- protos; then
    echo "::error::committed protobuf bindings differ from the generated output; run scripts/protos/generate.sh"
    exit 1
  fi
  untracked="$(git ls-files --others --exclude-standard -- protos)"
  if [[ -n "$untracked" ]]; then
    echo "::error::generated protobuf bindings are not committed:"
    echo "$untracked"
    exit 1
  fi
fi
