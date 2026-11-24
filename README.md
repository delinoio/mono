# Delino

Delino is the monorepo for Delino services. It contains web apps, a desktop and mobile app, Go servers, SDKs, command-line tools, Connect RPC schemas and shared tooling.

## Repository contents

| Path             | Contents                                                                                                                                                                                                                                                 |
| ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `apps/`          | The `delino-site`, `delino-docs`, `delino-accounts` and `ownsand` web apps, the tide web console in `apps/tide` (package `tide-console`) and the `delidev` desktop and mobile app                                                                        |
| `servers/`       | The `accounts-api`, `ownsand-api` and `tide-api` Go servers                                                                                                                                                                                              |
| `packages/`      | The shared React UI component package `delino-ui` and the TypeScript SDKs `ownsand-sdk` and `tide-sdk`                                                                                                                                                   |
| `go/`            | The public Go package tree, including the Go SDKs `ownsand-sdk-go` in `go/ownsand` and `tide-sdk-go` in `go/tide`                                                                                                                                        |
| `cmds/`          | The Go command tools `cmds/ownsand`, `cmds/ownsand-agent`, `cmds/ownsand-guest` and `cmds/tide`                                                                                                                                                          |
| `protos/delino/` | Connect RPC schemas in `protos/delino/ownsand/v1`, `protos/delino/accounts/v1` and `protos/delino/tide/v1`, with committed Go bindings and the TypeScript binding packages `@delino/ownsand-protos`, `@delino/accounts-protos` and `@delino/tide-protos` |
| `crates/`        | Reserved. It holds only its scoped `AGENTS.md` instructions today.                                                                                                                                                                                       |

The repository also contains shared instructions, skills, documentation and formatting tooling.

The root Cargo workspace has one member: `apps/delidev/src-tauri`.

## License

This repository uses two licenses. The license depends on the directory.

| Path                         | License                                                                                           |
| ---------------------------- | ------------------------------------------------------------------------------------------------- |
| `servers/`                   | [Functional Source License 1.1, ALv2 Future License](servers/LICENSE) (`FSL-1.1-ALv2`)            |
| `cmds/ownsand-agent/`        | [Functional Source License 1.1, ALv2 Future License](cmds/ownsand-agent/LICENSE) (`FSL-1.1-ALv2`) |
| `cmds/ownsand-guest/`        | [Functional Source License 1.1, ALv2 Future License](cmds/ownsand-guest/LICENSE) (`FSL-1.1-ALv2`) |
| All other repository content | [Apache License 2.0](LICENSE) (`Apache-2.0`)                                                      |

The `FSL-1.1-ALv2` directories contain the backend services (`servers/`), the OwnSand device agent (`cmds/ownsand-agent/`) and the OwnSand guest helper (`cmds/ownsand-guest/`). This code is source-available, not Apache-2.0:

- You can use, change and redistribute it for any purpose other than a Competing Use, as defined in [servers/LICENSE](servers/LICENSE).
- Each version becomes available under Apache-2.0 two years after its release.

The OwnSand CLI in `cmds/ownsand/`, the SDKs and the Connect RPC schemas use Apache-2.0.

[LICENSE-FSL](LICENSE-FSL) is a root copy of the FSL-1.1-ALv2 license files. It exists so that the GitHub license summary shows both licenses. Its terms apply only to `servers/`, `cmds/ownsand-agent/` and `cmds/ownsand-guest/`.

Imported skills keep their original licenses and notices. See [NOTICE](NOTICE).

## Setup

### Requirements

- Node.js 24
- pnpm 10.26.2
- The Go toolchain that the root `go.mod` selects (Go 1.26.1 or later)

The root `pnpm build`, `pnpm dev`, `pnpm lint` and `pnpm test` tasks also run the Go server scripts in `servers/accounts-api`, `servers/ownsand-api` and `servers/tide-api`. The scripts of these servers call `go`.

### Install and check

Install the dependencies, run the tests and check the formatting:

```sh
pnpm install --frozen-lockfile
pnpm test
pnpm format:check --all
```

### Development variables (optional)

`pnpm dev` works without a secret provider.

Maintainers with access to the team's Infisical development project can get the development variables from Infisical:

1. Run `infisical login`.
2. Run `infisical init` in the repository root.

The development servers then receive their development variables from Infisical. See [the environment contract](docs/repository-environment-contract.md#local-development-injection).

## Add a project

1. Read [the documentation catalog](docs/README.md) and [the repository instructions](AGENTS.md).
2. Before runtime implementation, create a project index and at least one domain contract.
3. Add language workspaces and build, test and release workflows when the first project that owns them is introduced.
