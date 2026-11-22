# servers working instructions

- Follow the root instruction-update policy and the nearest parent instructions.
- This file covers `servers/` and its descendants unless a more specific instruction file applies.
- Read the owning contracts below before changing behavior, including affected cross-domain consumers.

## Licensing

- Source under `servers/` is licensed under FSL-1.1-ALv2 (`servers/LICENSE`), not the repository-wide Apache-2.0 license. Each version becomes available under Apache-2.0 on the second anniversary of its release.
- Use `SPDX-License-Identifier: FSL-1.1-ALv2` in server source headers and `FSL-1.1-ALv2` in server package manifests that declare a license.
- Root `LICENSE-FSL` is a scoped copy of `servers/LICENSE`, and `cmds/ownsand-agent/LICENSE` and `cmds/ownsand-guest/LICENSE` have the same text. When `servers/LICENSE` changes, update those files and the text after the `LICENSE-FSL` scope statement in the same change.
- Put generic Go packages that only servers share under `servers/internal/<name>/` (FSL-1.1-ALv2) and register them in the [shared internal Go package table](../docs/repository-workflow-contract.md#shared-internal-go-packages). Keep server-specific code in the server directory.
- Code shared with apps or packages belongs outside `servers/` under Apache-2.0. Do not move server code into Apache-2.0 directories without an explicit relicensing decision. Follow `docs/repository-license-contract.md`.

## Development and validation

- Follow root `AGENTS.md` and the owning project/domain contracts.

- Write Go code and comments in English; use `log/slog` structured logging and never log secrets or sensitive payloads.
- Each server deployed to the shared DigitalOcean Droplet declares a `deploy` field in its `package.json`, keeps a `Dockerfile` in its package root, reads its listen address from `ADDR` and serves `GET /healthz`. Follow [the deployment workflow](../docs/repository-workflow-contract.md#deployment-workflow). Declare a public UDP port in `deploy.udpPorts` and an HTTP/2-only upstream in `deploy.upstream`. A server that needs another kind of public port declares no `deploy` field until the deployment workflow supports that port.
- Read runtime configuration from environment variables without a project or service prefix (`DATABASE_URL`, not `OWNSAND_DATABASE_URL`). Follow [the environment contract](../docs/repository-environment-contract.md#configuration-ownership).
- Server Go code uses the root Go module `github.com/delinoio/mono`; do not add a `go.mod` under `servers/`. Each server has a workspace `package.json` whose scripts wrap the Go toolchain and also run the server's launcher tests. Before finishing, run these from the repository root: `pnpm exec turbo run test --filter=./servers/<server>` for each changed server (`go test ./...` followed by the launcher tests), `go vet ./...`, and `git ls-files -z -- '*.go' | xargs -0 --no-run-if-empty gofmt -l` (it must print nothing; bare `gofmt` reads standard input and checks no file).

## Owning contracts

- [Repository defaults](../docs/repository-defaults.md)
- [Repository workflow](../docs/repository-workflow-contract.md)
- [Authentication](../docs/repository-auth-contract.md)
- [Billing currency and precision](../docs/repository-billing-contract.md)
- [Repository licensing](../docs/repository-license-contract.md)
- `servers/accounts-api/`: [project index](../docs/project-delino-accounts.md) and [API server foundation contract](../docs/servers-accounts-api-foundation.md)
- `servers/ownsand-api/`: [project index](../docs/project-ownsand.md) and [control plane foundation contract](../docs/servers-ownsand-api-foundation.md)
- `servers/tide-api/` (`planned`): [project index](../docs/project-tide.md) and [control plane foundation contract](../docs/servers-tide-api-foundation.md)
- Register each new project and its domain contracts in [the documentation catalog](../docs/README.md) before implementation.
