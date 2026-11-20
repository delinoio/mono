# cmds-ownsand-cli foundation contract

## Scope

This contract owns the OwnSand command-line interface, component `ownsand-cli`, of [`ownsand`](project-ownsand.md) at `cmds/ownsand/`. The binary name is `ownsand`. The CLI uses Apache-2.0 and must not import code from `servers/`.

The device agent has its own commands; the [agent contract](cmds-ownsand-agent-foundation.md#commands) owns them.

## Runtime and Language

- Language: Go in the root module. The CLI is built on the [Go SDK](packages-ownsand-sdk-foundation.md).
- Targets: Linux, macOS and Windows on `amd64` and `arm64`. Every target cross-compiles from the root module (`GOOS=<os> GOARCH=<arch> go build ./cmds/ownsand`).
- The CLI uses only the standard library `flag` package for arguments; it has no CLI framework dependency.
- Release and distribution: open. No release workflow, signing or package-manager template builds or publishes the `ownsand` binary yet. Record the process here before the first release.

## Users and Operators

- Organization owners, admins and members who sign in with Logto.
- Programs and CI jobs that use an organization API key.

## Interfaces and Contracts

### Authentication

- `ownsand login` signs in with the Logto device authorization flow: the CLI shows a verification URL and a user code, and the user completes the sign-in in a browser. The CLI is a Logto native application with device flow. Record the application registration and the scopes with the implementation.
  - Implementation: `POST <endpoint>/oidc/device/auth` with `client_id`, `scope` and `resource` (the API origin, `https://ownsand.api.delino.io` by default), then polling `POST <endpoint>/oidc/token` with the device code grant, which follows `authorization_pending` and `slow_down` ([RFC 8628](https://www.rfc-editor.org/rfc/rfc8628)). The CLI refreshes an access token 30 seconds before it expires with the `refresh_token` grant and the same `resource`, and stores the rotated refresh token.
  - The Logto endpoint comes from `--logto-endpoint` or `OWNSAND_LOGTO_ENDPOINT` and must use `https` (plain `http` only on loopback, for tests). The application ID comes from `--app-id` or `OWNSAND_LOGTO_APP_ID`. The default scopes are `openid offline_access`; `--scope` or `OWNSAND_LOGTO_SCOPES` replaces them.
  - Open: the Logto native application of the CLI is not registered, so there is no built-in endpoint or application ID, and the OwnSand API scope names are not defined yet (the [control plane contract](servers-ownsand-api-foundation.md#authentication) records them with token validation). Add both as defaults when they exist. `login` was tested only against a fake device flow server, not against the Logto tenant.
- `ownsand logout` deletes the stored tokens.
- When `OWNSAND_API_KEY` is set, the CLI uses the API key and ignores stored Logto tokens. Commands that API keys cannot use fail with the server's error: `permission_denied` for a forbidden operation in the key's own organization and for the commands without an organization, and `not_found` when `--org` names another organization.
- Credential storage: a file in the user configuration directory that only the user can read (mode `0600` on Linux and macOS, a user-only ACL on Windows). Storage in the OS keychain is an open decision and a new dependency.
  - Implementation: `<user configuration directory>/ownsand/credentials.json` (`os.UserConfigDir`; `OWNSAND_CONFIG_DIR` replaces the directory) with the Logto endpoint, application ID, resource, access token, refresh token and expiry. The CLI writes a temporary file with mode `0600` in a `0700` directory and renames it into place. On Windows it removes inherited ACL entries and grants full control only to the current user with the built-in `icacls` tool, because the standard library cannot set ACLs and no Windows API dependency is approved; this path is cross-compiled and vetted but not verified on a Windows host.

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
- `sandbox create` generates one `idempotency_key` and reuses it when it retries the create after a lost response.
- `sandbox create` requires exactly one source flag (`--image linux|macos|windows`, `--snapshot <id>` or `--template <id>`), `--idle-ttl` and `--max-ttl`, `--vcpu`, `--memory` in MiB and `--disk` in GiB. Optional flags are `--device <id>`, `--os`, `--arch`, `--network outbound|none` and repeated `--label key=value`. It has no default TTL.
- `sandbox exec` streams stdout and stderr to the CLI's own standard streams and exits with the command's exit code when it is between 0 and 255. A nonzero code outside that range exits with status `255`, and a command that a signal ended exits with `128` plus the signal number (or `255` when that is above 255), so a failed command never exits with `0`. `--format json` keeps the exact value. When the data path fails, it exits with status `255` and a data path error on standard error.
- `member accept` reads the invitation secret from standard input or from the file named by `--secret-file`. It never accepts the secret as a command-line argument, because other local users can read process arguments.
- When an exec loses output (`output_truncated`), `sandbox exec` writes a warning with the missing offsets to standard error and exits with status `255` after the command ends.
- When the sandbox ended before the exec result (a `sandbox_ended` event), `sandbox exec` writes the end reason to standard error and exits with status `125`. The `--format json` events include `sandbox_ended` with the end reason.
- When the command could not start (a `failed` event), `sandbox exec` writes the reason to standard error and exits with status `127`. When the exec `timeout` ended (`timed_out` in the `exited` event), it exits with status `124`; `--format json` keeps the exit code or signal and the `timed_out` flag.
- In text mode, when more than one rule applies, the first one in this order sets the exit status (`--format json` uses only its own rule below): data path failure or lost output (`255`), sandbox ended before the result (`125`), command could not start (`127`), `timeout` ended (`124`), signal (`128` plus N), exit code.
- Exit status `255` means a data path failure or lost output, `127` means that the command could not start, `124` means that the exec `timeout` ended, and `128` plus N means a signal, but a guest command can also exit with any of these values. A script that must tell them apart uses `--format json`.
- `file read` writes the raw file contents to standard output or to the path in `--to`. `file write` reads from standard input or from the path in `--from`. `--format` does not apply to these raw contents.
- Commands that create a secret (`member invite`, `enrollment-token create`, `api-key create`) print the secret once on standard output and the other fields on standard error, so that a script can capture only the secret. With `--format json`, they print one JSON object with the secret and the other fields on standard output.
- `--format json` prints machine-readable JSON for every command that prints data, except the raw file contents of `file read`. With `--format json`, `sandbox exec` prints one JSON object per line on standard output for each exec event (`started`, `stdout` and `stderr` chunks as UTF-8 text with invalid bytes replaced, or base64 with `--binary`, `output_truncated`, `exited` with exactly one of `exit_code` or `signal`, `failed` with the reason the command could not start, and a `data_path_error` event), and the CLI exits with `0` after an `exited`, `failed` or `sandbox_ended` event, and with `255` after a `data_path_error` event or an output loss, so the exit status of `--format json` shows only whether the CLI received the complete result. The default is `--format text`.
- Output uses ANSI color by default. `--no-color` and `NO_COLOR` turn it off.
- Implementation details:
  - Global flags (`--org`, `--format`, `--no-color`, `--verbose`, `--api-origin` and `--transport auto|relay|peer-to-peer`) are accepted before the command and after the subcommand. The API origin comes from `--api-origin`, `OWNSAND_API_ORIGIN` or the default `https://ownsand.api.delino.io`. `--transport` forces the data path for tests.
  - `sandbox exec <sandbox-id> [--env K=V] [--cwd DIR] [--timeout D] [--binary] -- <program> [args...]`. Durations accept Go durations and a `d` (24-hour day) suffix.
  - Text output prints the top-level fields of each record as `name: value` lines; JSON output prints one object, or `{"items": [...], "next_page_token": ...}` for lists. Commands without a result print nothing in text mode and `{}` in JSON mode.
  - `sandbox exec --format json` events: `{"event":"started","exec_id":...}`, `{"event":"stdout"|"stderr","offset":N,"data":...}` (with `"encoding":"base64"` under `--binary`), `{"event":"output_truncated","gaps":[{"stream","from","to"}]}`, `{"event":"exited","exit_code"|"signal":N,"timed_out":B}`, `{"event":"failed","reason":...}`, `{"event":"sandbox_ended","reason":"<SandboxEndReason without prefix>"}` and `{"event":"data_path_error","code","message"}`.
  - Other exit statuses: `0` on success, `1` for an error of the command (for example an RPC error), `2` for a usage error that the CLI detects before any request (missing TTL or resources, an unknown flag), and `255` for a data path failure of `file read` or `file write`.

## Storage

- The credential file described in [Authentication](#authentication).
- No other persistent state. Sandbox content is never cached on disk.

## Security

- The CLI never prints stored tokens or API keys, except a new secret once at issue time.
- The CLI sends its API key and its Logto tokens only to the configured API origin, which also serves the relay, and the Logto tenant.

## Logging

`--verbose` writes `log/slog` records to standard error: RPC names, IDs, transport selection, Connect codes and durations. They never contain credentials, exec content or file content.

## Build and Test

- From the repository root: `gofmt` check, `go vet ./...` and `go test ./...`, according to `cmds/AGENTS.md`.
- Required tests: argument validation (missing TTL and resources), exit code propagation, the data path error exit status, secret printing to standard output only, `OWNSAND_API_KEY` precedence and credential file permissions.
- The tests in `cmds/ownsand/main_test.go` cover the required tests with the in-process fake control plane of the Go SDK (`ownsandtest`) and a fake Logto device flow server, and also cover lost output, JSON events, `member accept` input, token refresh, raw file transfer, color and verbose logging. Cross-compile check: `GOOS=windows go vet ./cmds/ownsand/...` and `GOOS=darwin go vet ./cmds/ownsand/...`.

## Dependencies and Integrations

- Upstream: the [Go SDK](packages-ownsand-sdk-foundation.md), the [control plane](servers-ownsand-api-foundation.md) (API, relay and STUN) and the Logto shared tenant.

## Change Triggers

- Update this contract when a public RPC in the [protos contract](protos-ownsand-api-contract.md) or the Go SDK surface changes the command set.
- Update [project-ownsand.md](project-ownsand.md) when the binary name or the distribution changes.
- Update `cmds/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-ownsand.md](project-ownsand.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [Logto device flow](https://docs.logto.io/quick-starts/device-flow)
