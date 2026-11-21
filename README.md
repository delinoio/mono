# Delino

Monorepo for Delino services. It currently contains the `delino-site`, `delino-docs`, `delino-accounts` and `ownsand` web apps, the `delidev` desktop and mobile app, the `accounts-api` and `ownsand-api` Go servers in `servers/`, the shared React UI component package `delino-ui` and the TypeScript SDK `ownsand-sdk` in `packages/`, the Go SDK `ownsand-sdk-go` in the public Go package tree `go/ownsand`, the Go command tools `cmds/ownsand`, `cmds/ownsand-agent` and `cmds/ownsand-guest`, the OwnSand Connect RPC schemas in `protos/delino/ownsand/v1` with committed Go bindings (the TypeScript bindings are in `packages/ownsand-sdk`), and shared instructions, skills, documentation and formatting tooling. The root Cargo workspace has one member, `apps/delidev/src-tauri`. Only the `crates/` directory is still reserved: it holds only its scoped `AGENTS.md` instructions today.

## License

This repository uses two licenses. The license depends on the directory.

| Path                         | License                                                                                           |
| ---------------------------- | ------------------------------------------------------------------------------------------------- |
| `servers/`                   | [Functional Source License 1.1, ALv2 Future License](servers/LICENSE) (`FSL-1.1-ALv2`)            |
| `cmds/ownsand-agent/`        | [Functional Source License 1.1, ALv2 Future License](cmds/ownsand-agent/LICENSE) (`FSL-1.1-ALv2`) |
| `cmds/ownsand-guest/`        | [Functional Source License 1.1, ALv2 Future License](cmds/ownsand-guest/LICENSE) (`FSL-1.1-ALv2`) |
| All other repository content | [Apache License 2.0](LICENSE) (`Apache-2.0`)                                                      |

The backend services in `servers/`, the OwnSand device agent in `cmds/ownsand-agent/` and the OwnSand guest helper in `cmds/ownsand-guest/` are source-available, not Apache-2.0. You can use, change and redistribute them for any purpose other than a Competing Use, as defined in [servers/LICENSE](servers/LICENSE). Each version becomes available under Apache-2.0 two years after its release. The OwnSand CLI in `cmds/ownsand/`, the SDKs and the Connect RPC schemas use Apache-2.0.

[LICENSE-FSL](LICENSE-FSL) is a root copy of the FSL-1.1-ALv2 license files. It is there so that the GitHub license summary shows both licenses. Its terms apply only to `servers/`, `cmds/ownsand-agent/` and `cmds/ownsand-guest/`.

Imported skills retain their original licenses and notices. See [NOTICE](NOTICE).

## Setup

Use Node.js 24, pnpm 10.26.2 and the Go toolchain selected by the root `go.mod` (Go 1.26.1 or later). The root `pnpm build`, `pnpm dev`, `pnpm lint` and `pnpm test` tasks run the Go servers `servers/accounts-api` and `servers/ownsand-api`, whose scripts call `go`.

```sh
pnpm install --frozen-lockfile
pnpm test
pnpm format:check --all
```

## Add a project

Start with [the documentation catalog](docs/README.md) and [repository instructions](AGENTS.md). Create a project index and at least one domain contract before runtime implementation. Add language workspaces and build, test and release workflows when their first owning project is introduced.
