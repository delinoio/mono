# cmds-ownsand-cli foundation contract

## Scope

This contract owns the OwnSand command-line interface, component `ownsand-cli`, of [`ownsand`](project-ownsand.md) at `cmds/ownsand/` (`planned`). The binary name is `ownsand`. The CLI uses Apache-2.0 and must not import code from `servers/`.

The device agent has its own commands; the [agent contract](cmds-ownsand-agent-foundation.md#commands) owns them.

## Runtime and Language

- Language: Go in the root module. The CLI is built on the [Go SDK](packages-ownsand-sdk-foundation.md).
- Targets: Linux, macOS and Windows on `amd64` and `arm64`. Record the release and distribution process with the implementation.

## Users and Operators

- Organization owners, admins and members who sign in with Logto.
- Programs and CI jobs that use an organization API key.

## Interfaces and Contracts

### Authentication

- `ownsand login` signs in with the Logto device authorization flow: the CLI shows a verification URL and a user code, and the user completes the sign-in in a browser. The CLI is a Logto native application with device flow. Record the application registration and the scopes with the implementation.
- `ownsand logout` deletes the stored tokens.
- When `OWNSAND_API_KEY` is set, the CLI uses the API key and ignores stored Logto tokens. Commands that API keys cannot use fail with the server's `permission_denied` error.
- Credential storage: a file in the user configuration directory that only the user can read (mode `0600` on Linux and macOS, a user-only ACL on Windows). Storage in the OS keychain is an open decision and a new dependency.

### Commands

| Command group              | Subcommands                                                                          |
| -------------------------- | ------------------------------------------------------------------------------------ |
| `ownsand org`              | `list`, `create`, `get`, `delete`, `transfer`                                        |
| `ownsand member`           | `list`, `invite`, `invitations`, `revoke-invitation`, `accept`, `set-role`, `remove` |
| `ownsand device`           | `list`, `get`, `set-limits`, `remove`                                                |
| `ownsand enrollment-token` | `create`, `list`, `revoke`                                                           |
| `ownsand api-key`          | `create`, `list`, `revoke`                                                           |
| `ownsand agent-version`    | `get`, `set`                                                                         |
| `ownsand sandbox`          | `create`, `get`, `list`, `delete`, `pause`, `resume`, `exec`                         |
| `ownsand file`             | `read`, `write`                                                                      |
| `ownsand snapshot`         | `create`, `get`, `list`, `delete`, `export`                                          |
| `ownsand template`         | `create`, `list`, `delete`                                                           |

- The organization comes from `--org` or the `OWNSAND_ORG` environment variable. `ownsand org` commands that need no organization and `ownsand member accept`, which takes the organization from the invitation, ignore them.
- `sandbox create` requires `--idle-ttl` and `--max-ttl`, `--vcpu`, `--memory` and `--disk`. It has no default TTL.
- `sandbox exec` streams stdout and stderr to the CLI's own standard streams and exits with the command's exit code. When the data path fails, it exits with status `255` and a data path error on standard error.
- `member accept` reads the invitation secret from standard input or from the file named by `--secret-file`. It never accepts the secret as a command-line argument, because other local users can read process arguments.
- When an exec loses output (`output_truncated`), `sandbox exec` writes a warning with the missing offsets to standard error and exits with status `255` after the command ends.
- Exit status `255` means a data path failure or lost output, but a guest command can also exit with `255`. A script that must tell them apart uses `--format json`.
- `file read` writes the raw file contents to standard output or to the path in `--to`. `file write` reads from standard input or from the path in `--from`. `--format` does not apply to these raw contents.
- Commands that create a secret (`member invite`, `enrollment-token create`, `api-key create`) print the secret once on standard output and the other fields on standard error, so that a script can capture only the secret. With `--format json`, they print one JSON object with the secret and the other fields on standard output.
- `--format json` prints machine-readable JSON for every command that prints data, except the raw file contents of `file read`. With `--format json`, `sandbox exec` prints one JSON object per line on standard output for each exec event (`started`, `stdout` and `stderr` chunks as UTF-8 text with invalid bytes replaced, or base64 with `--binary`, `output_truncated`, `exited` with the exit code, `failed`, and a `data_path_error` event), and the CLI exits with `0` unless it fails itself. The default is `--format text`.
- Output uses ANSI color by default. `--no-color` and `NO_COLOR` turn it off.

## Storage

- The credential file described in [Authentication](#authentication).
- No other persistent state. Sandbox content is never cached on disk.

## Security

- The CLI never prints stored tokens or API keys, except a new secret once at issue time.
- The CLI never sends credentials to any origin except the configured API origin and the Logto tenant.

## Logging

`--verbose` writes `log/slog` records to standard error: RPC names, IDs, transport selection, Connect codes and durations. They never contain credentials, exec content or file content.

## Build and Test

- From the repository root: `gofmt` check, `go vet ./...` and `go test ./...`, according to `cmds/AGENTS.md`.
- Required tests: argument validation (missing TTL and resources), exit code propagation, the data path error exit status, secret printing to standard output only, `OWNSAND_API_KEY` precedence and credential file permissions.

## Dependencies and Integrations

- Upstream: the [Go SDK](packages-ownsand-sdk-foundation.md), the [control plane](servers-ownsand-api-foundation.md) and the Logto shared tenant.

## Change Triggers

- Update this contract when a public RPC in the [protos contract](protos-ownsand-api-contract.md) or the Go SDK surface changes the command set.
- Update [project-ownsand.md](project-ownsand.md) when the binary name or the distribution changes.
- Update `cmds/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-ownsand.md](project-ownsand.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [Logto device flow](https://docs.logto.io/quick-starts/device-flow)
