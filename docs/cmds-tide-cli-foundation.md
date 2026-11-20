# cmds-tide-cli foundation contract

## Scope

This contract owns the tide command-line interface, component `tide-cli`, of [`tide`](project-tide.md) at `cmds/tide/` (`planned`). The binary name is `tide`. It lets people inspect the queue, the runners and the device pool, and manage organizations, runner profiles and integrations from a terminal.

## Runtime and Language

- Language: Go in the root module `github.com/delinoio/mono`. Logging: `log/slog`.
- The CLI uses the [Go SDK](packages-tide-sdk-foundation.md). It must not import code from `servers/`.
- Distribution: release artifacts for Linux, macOS and Windows on `amd64` and `arm64`. Record the release workflow with the implementation.

## Users and Operators

Organization owners, admins and members who sign in with Logto.

## Interfaces and Contracts

| Command            | Subcommands                                                                   |
| ------------------ | ----------------------------------------------------------------------------- |
| `tide login`       | Sign in with the Logto device authorization flow.                             |
| `tide logout`      | Delete the stored tokens.                                                     |
| `tide org`         | `list`, `create`, `use`, `delete`                                             |
| `tide member`      | `list`, `invite`, `revoke-invite`, `set-role`, `remove`, `leave`              |
| `tide ownsand`     | `link`, `show`, `unlink`                                                      |
| `tide github`      | `install-url`, `list`, `runner-groups`, `set-runner-group`, `unlink`          |
| `tide integration` | `create`, `list`, `get`, `update`, `delete`                                   |
| `tide profile`     | `create`, `list`, `get`, `update`, `delete`                                   |
| `tide request`     | `list`, `get`, `cancel`                                                       |
| `tide runner`      | `list`, `get`, `delete`                                                       |
| `tide status`      | Show the queue, the runners by state and the device pool of the organization. |

- `tide login` uses the Logto device authorization flow, like the [OwnSand CLI](cmds-ownsand-cli-foundation.md). The CLI is a Logto native application. Record the application registration and scopes with the implementation.
- Credential storage: a file in the user configuration directory that only the user can read, with the same rules as the OwnSand CLI.
- `--org` selects the organization. Without `--org`, the CLI uses the organization that `tide org use` stored.
- Credentials (the OwnSand API key and agent integration credentials) are read from standard input or from a file named by a `--*-file` flag. The CLI never accepts a credential as a command-line argument, because other local users can read process arguments.
- GitHub installation needs a browser: `tide github install-url` prints the installation URL, and the console callback completes the link.
- Output: tables by default and `--output json` for scripts. Output uses ANSI color by default; `--no-color` and the `NO_COLOR` environment variable turn color off.
- Exit codes: `0` on success, `1` on a server or network error, `2` on invalid usage.

## Storage

Only the stored Logto tokens and the selected organization in the user configuration directory.

## Security

- The CLI sends tokens only to the configured API origin and the Logto tenant.
- The CLI never prints a stored token or a credential.

## Logging

`--verbose` writes structured debug logs to standard error. Logs never contain tokens or credentials.

## Build and Test

- Package scripts wrap the Go toolchain. Record them with the implementation.
- Tests: argument parsing, credential input from standard input and files, output formats and error exit codes against a fake `tide-api`.

## Dependencies and Integrations

- Upstream: the [Go SDK](packages-tide-sdk-foundation.md), `tide-api` and the Logto shared tenant.

## Change Triggers

- Update this contract with command or flag changes.
- Update `cmds/AGENTS.md` only when development procedures, directory ownership or development rules change; follow the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-tide.md](project-tide.md)
- [cmds-ownsand-cli-foundation.md](cmds-ownsand-cli-foundation.md)
- [repository-defaults.md](repository-defaults.md)
