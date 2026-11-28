# cmds-sandcrew-bridge foundation contract

## Scope

This contract owns the SandCrew guest bridge, component `sandcrew-bridge`, of [`sandcrew`](project-sandcrew.md) at `cmds/sandcrew-bridge/` (`planned`, Apache-2.0). The bridge runs in the sandbox of one SandCrew session. It installs and starts the vendor agent, speaks ACP with it over local stdio, reads input from an inbox directory, and writes normalized events to its stdout and to an append-only guest event log.

The bridge has no network connection to SandCrew. [`sandcrew-api`](servers-sandcrew-api-foundation.md) reaches it only through SandRun dev session RPCs: file writes for input, one long-running exec for output, and file reads for recovery. This avoids a SandCrew credential in the sandbox, where the agent could read it. SandRun distributes no device program; SandCrew owns, builds and copies the bridge.

## Runtime and Language

- Go in the root module `github.com/delinoio/mono`, import path `github.com/delinoio/mono/cmds/sandcrew-bridge/...`. Logging: `log/slog`.
- One static binary per release target: `linux-amd64`, `linux-arm64`, `windows-amd64`, `windows-arm64` and `darwin-arm64` (the OwnSand guest platforms). The binary uses only the Go standard library, the generated schema types of `protos/delino/sandcrew/v1/` and approved dependencies.
- Distribution: `sandcrew-api` embeds the binaries of the same commit and copies the matching one into each new session. The bridge version is the version of `servers/sandcrew-api/package.json` and the commit, embedded at build time. There is no separate download or GitHub Release of the bridge in the MVP.

## Users and Operators

- `sandcrew-api`, which writes the bridge, its start configuration and inbox files, starts it and reads its events.
- The vendor agent and its ACP adapter, which the bridge starts as child processes.
- Operators and developers who read the bridge log during debugging.

## Interfaces and Contracts

### State directory

The bridge keeps its files in `<workspace>/.sandcrew/`, where `<workspace>` is the `workspace_path` of the session's runner profile (with the path syntax of the guest OS):

| Path                   | Writer                   | Content                                                                                             |
| ---------------------- | ------------------------ | --------------------------------------------------------------------------------------------------- |
| `bin/sandcrew-bridge`  | `sandcrew-api`           | The bridge binary (`sandcrew-bridge.exe` on Windows).                                               |
| `start.json`           | `sandcrew-api`           | Start configuration: session ID, vendor, permission mode, requested mode and configuration options. |
| `inbox/`               | `sandcrew-api`           | One `BridgeInput` file per input.                                                                   |
| `events.jsonl`         | bridge                   | The append-only [event log](#event-log).                                                            |
| `agents/`              | bridge                   | Downloaded agent CLIs, ACP adapters and runtimes.                                                   |
| `codex-home/auth.json` | `sandcrew-api` and Codex | The Codex ChatGPT sign-in file, when the session uses `CODEX_AUTH_JSON`.                            |
| `previous-session.md`  | `sandcrew-api`           | The previous transcript, for a session started from a transcript.                                   |
| `bridge.log`           | bridge                   | Structured bridge log, rotated at 10 MiB with one previous file.                                    |

- The agent runs with `<workspace>` as its working directory, so it can read and change these files. This is accepted: the sandbox is the security boundary, and the files hold nothing that the session's owner and agent may not see.

### Start

- `sandcrew-api` writes the binary, `start.json` and the credential file, then runs one long-running `ExecDevSession` with the command `<state>/bin/sandcrew-bridge run --state-dir <state>` and an environment with `SANDCREW_SESSION_ID`, `SANDCREW_VENDOR_CREDENTIAL` (for `API_KEY` and `CLAUDE_OAUTH_TOKEN`) and the selected session secrets. The exec has no timeout.
- The bridge removes every `SANDCREW_*` variable from the environment that it gives to child processes. It passes the credential to the agent in the form of the [vendor table](#vendors) and passes the session secrets unchanged.
- Order at start: write the `hello` event; install or find the pinned agent components; start the agent and the ACP adapter; send ACP `initialize` and `session/new` with `<workspace>` as `cwd` and no MCP servers; apply the start mode and configuration options; set `AgentActivity` `IDLE`; then process the inbox.
- The bridge offers no ACP client file system or terminal capability to the agent. The agent uses its own tools in the sandbox.
- A failure before the first `IDLE` writes a `bridge_error` (`AGENT_INSTALL_FAILED`, `CHECKSUM_MISMATCH`, `AGENT_START_FAILED` or `CREDENTIAL_MISSING`) and exits with code `2`. A missing credential for the selected vendor is `CREDENTIAL_MISSING`.

### Inbox

- `sandcrew-api` writes each input as `inbox/<input_id>.json` with `WriteDevSessionFile`, which makes the file appear only after its final chunk (an atomic rename in the guest), so the bridge never reads a partial file. The bridge ignores other names and directories.
- The bridge looks for new files every 250 milliseconds and on file system notifications where the guest OS provides them, and handles them in `input_id` order (UUID v7 time order).
- For each file, the bridge validates the `BridgeInput`, applies or queues it, writes `input_accepted` or `input_rejected` (with `InputRejectionReason`) to the event log and stdout, and then deletes the file. An `input_id` that the event log already contains is deleted without a new event, so a repeated write delivers the input at most once.
- `prompt`: when no turn runs, the bridge writes a `user_message_chunk` with the text, sends ACP `session/prompt` and sets `WORKING`. During a turn, it queues the prompt and sends it after the turn ends. An `attachment_path` is mentioned in the prompt text by `sandcrew-api`; the bridge does not read the file.
- `permission_answer`: answers the pending ACP `session/request_permission` with the option; an unknown request or option is rejected.
- `cancel_turn`: sends ACP `session/cancel`, answers every pending permission request with the outcome `cancelled`, and drops queued prompts. Without an active turn it is rejected with `NO_ACTIVE_TURN`.
- `set_mode`: with a `mode_id`, sends ACP `session/set_mode` (or sets the configuration option of category `mode` when the agent advertises modes that way). With a `permission_mode`, switches between `AUTO_APPROVE` and `ASK` as in [Permissions](#permissions).
- `set_config_option`: sends ACP `session/set_config_option`. An option or value that the agent rejects writes a `bridge_error` `OPTION_REJECTED`.

### Event log

- Every event except `credential_update` is first appended to `events.jsonl` as one line of protobuf JSON followed by `\n`, and then written to stdout as the same line. `sequence` starts at 1 and increases by 1; `log_end_offset` is the byte length of the log after the line.
- The bridge flushes the log after each line and calls `fsync` at each `turn_ended` and every 5 seconds while lines were written. A failed append is retried 3 times; then the bridge writes a `bridge_error` `EVENT_LOG_WRITE_FAILED` to stdout with the last logged sequence, stops the agent and exits with code `3`.
- `sandcrew-api` reads the log from a stored `log_end_offset` to recover events that it missed on stdout. The bridge never rewrites or truncates the log.
- Mapping from ACP follows the [protos contract](protos-sandcrew-api-contract.md#bridge-schema). ACP updates without a mapped case become `unknown_update` with the original JSON. ACP JSON-RPC errors of the agent and malformed messages become `bridge_error` `ACP_PROTOCOL_ERROR`; the bridge continues when the agent process continues.
- `session_state` events report `AgentActivity` changes: `STARTING` at start, `WORKING` while a turn runs, `WAITING_FOR_PERMISSION` while an ASK-mode permission request is pending, and `IDLE` after `turn_ended`.
- When the agent process ends, the bridge writes `agent_exited` with its exit code or signal and exits with code `0`.

### Permissions

- New sessions start in `AUTO_APPROVE`. The bridge starts the vendor's auto-approve mode from the [vendor table](#vendors) where one exists. It answers every remaining ACP permission request with the first option of kind `ALLOW_ONCE`, otherwise `ALLOW_ALWAYS`, and writes `permission_request` and then `permission_response` with `auto_approved` true. A request without an allow option is handled as in `ASK`.
- In `ASK`, the bridge switches the agent to its default (asking) mode where the vendor has one, writes `permission_request` and waits for a `permission_answer` without a time limit. The pending request counts as waiting for input.
- `mode_changed` reports the agent mode and the `PermissionMode` together.
- Permission prompts are advisory. The agent and the bridge run in the same sandbox, and an agent with shell access can bypass them.

### Vendors

| Vendor (`AgentVendor`) | MVP | Integration                                                                       | Credential to the agent                                                                                       | Auto-approve mode                        |
| ---------------------- | --- | --------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------- | ---------------------------------------- |
| `CLAUDE_CODE`          | yes | The Claude Agent SDK ACP adapter listed in the ACP agents directory, with Node.js | `CLAUDE_OAUTH_TOKEN` as `CLAUDE_CODE_OAUTH_TOKEN`; `API_KEY` in the form the pinned adapter documents         | Recorded with the adapter implementation |
| `OPENAI_CODEX`         | yes | The Codex ACP adapter listed in the ACP agents directory                          | `CODEX_AUTH_JSON` through `CODEX_HOME=<state>/codex-home`; `API_KEY` in the form the pinned version documents | Recorded with the adapter implementation |

- Reserved environment names, which session secrets must not use: every `SANDCREW_*` name, `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`, `OPENAI_API_KEY`, `CODEX_API_KEY` and `CODEX_HOME`. The adapter implementation adds each further variable that it sets to this list.
- Headless fallback: a vendor without ACP is wrapped by a bridge adapter that runs the vendor's headless CLI mode and converts its output to the same events (`hello.wrapped_headless` true). Candidates recorded for later vendors, to be verified against official documentation before use: Claude Code `claude -p --output-format stream-json` with `--resume`, Codex `codex exec --json` with `codex exec resume`, Cursor `agent -p --output-format stream-json` with `--resume`, Devin `devin -p`.
- Open decisions (recorded here with the adapter implementation, before it is merged): for each MVP vendor, the adapter package and version, the guest OSes on which the agent and the adapter run, the auto-approve mode and its ID, the API key variable, and whether the agent advertises `session/load` or `session/resume`.

### Pinned agent installation

- Each bridge release embeds a pin file (`cmds/sandcrew-bridge/internal/pins/pins.json`) with, for each vendor and release target, every component that the agent needs: the agent CLI, the ACP adapter and the runtime (for example Node.js), each with its version, its official download URL and the SHA-256 of the archive. npm-based components are pinned by a committed `package-lock.json` whose integrity hashes cover every package.
- At start, the bridge uses a component that the image already contains when its reported version equals the pinned version. Otherwise it downloads the pinned archive into `agents/<component>/<version>/`, verifies the SHA-256 before it extracts it, and installs npm components with `npm ci --ignore-scripts` of the pinned Node.js and the embedded lockfile, which verifies each package's integrity.
- A download that fails (after 3 attempts with backoff, at most 10 minutes in total) writes `AGENT_INSTALL_FAILED`; a digest or integrity mismatch writes `CHECKSUM_MISMATCH`. Both fail the session start with that reason.
- Renovate updates the pins. A pin change is a bridge change and reaches only new sessions.

### Protocol versions

- The first event of every bridge process is `hello` with `protocol_version`. The [protos contract](protos-sandcrew-api-contract.md#protocol-versions) owns the major and minor rules.
- SandCrew never replaces the bridge or the agent of an existing session. A session keeps the bridge and the pinned agent components of its start for its whole life, also across pauses.

### Credential updates

- For `CODEX_AUTH_JSON`, the bridge watches `codex-home/auth.json` (modification time and SHA-256, checked every 10 seconds and on file system notifications). When the content changes, it writes a `credential_update` event with an increased `revision` to stdout only. The event carries no content; `sandcrew-api` reads the file itself.
- A failure to watch the file writes `bridge_error` `CREDENTIAL_WATCH_FAILED` once and the session continues.
- Open decision: whether a Codex refresh in one session invalidates the token in other sessions that use the same credential. The adapter implementation verifies it and records the result here before it is merged.

## Storage

- Guest files in the [state directory](#state-directory). The bridge stores nothing outside it except what the agent itself writes.
- The event log is the guest copy of the transcript and stays until the sandbox is deleted.

## Security

- The bridge receives only the session's model credential and its selected session secrets. It never receives a SandCrew, SandRun or OwnSand credential.
- The bridge never writes a credential, a secret or `auth.json` content to the event log, stdout, stderr or `bridge.log`.
- Inbox files are untrusted input from the guest's point of view as well: the bridge limits a file to 1 MiB and rejects files that do not parse.

## Logging

- `bridge.log` and stderr carry structured JSON records (`log/slog`) with the session ID, the bridge version, the protocol version, the vendor, the agent version, the event kind and sequence, input IDs and error kinds. They are machine-read, so they use no ANSI color.
- Never log prompts, agent messages, thoughts, tool input or output, diffs, file contents, credentials or secrets.

## Build and Test

- A workspace package `cmds/sandcrew-bridge/package.json` with `build` (cross-compilation of the five targets) and `test` scripts that wrap the Go toolchain, created with the path.
- Unit tests with a fake ACP agent: the mapping of every event kind and of unknown updates, inbox handling (prompt, queued prompt, permission answer, cancel, mode change, configuration option, repeated input, malformed file), event log sequence numbers and offsets, auto-approval and its record, `ASK` mode, the `hello` event and its version, pinned component checks with a digest mismatch and a download failure, `auth.json` change detection, and the exit codes.
- Run `git ls-files -z -- '*.go' | xargs -0 --no-run-if-empty gofmt -l`, `go vet ./...` and `go test ./...` from the repository root according to `cmds/AGENTS.md`.
- Acceptance on real Linux, macOS and Windows guests with real vendor accounts is not performed in the MVP and is recorded as not performed.

## Dependencies and Integrations

- Upstream: the vendor agents and ACP adapters at their pinned versions, and the runtimes they need.
- Downstream: [`sandcrew-api`](servers-sandcrew-api-foundation.md), through SandRun dev session RPCs.
- Schema: [`protos/delino/sandcrew/v1/`](protos-sandcrew-api-contract.md). The bridge must not import code from `servers/`.
- New dependencies (for example, a file system notification library) need owner approval and a [dependency security](repository-dependency-security-contract.md) review in the change that adds them.

## Change Triggers

- Update the [protos contract](protos-sandcrew-api-contract.md) with every event or input change, and the [control plane contract](servers-sandcrew-api-foundation.md) when the start, the inbox, the event log or the credential files change.
- Update the [project index](project-sandcrew.md) when the release targets, the distribution or the vendor list change.

## References

- [project-sandcrew.md](project-sandcrew.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-license-contract.md](repository-license-contract.md)
- [ACP agents directory](https://agentclientprotocol.com/get-started/agents)
- [ACP session setup](https://agentclientprotocol.com/protocol/session-setup)
- [ACP tool calls](https://agentclientprotocol.com/protocol/tool-calls)
