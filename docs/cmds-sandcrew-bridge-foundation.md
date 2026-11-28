# cmds-sandcrew-bridge foundation contract

## Scope

This contract owns the SandCrew guest bridge, component `sandcrew-bridge`, of [`sandcrew`](project-sandcrew.md) at `cmds/sandcrew-bridge/` (Apache-2.0). The bridge runs in the sandbox of one SandCrew session. It installs and starts the vendor agent, speaks ACP with it over local stdio, reads input from an inbox directory, and writes normalized events to its stdout and to an append-only guest event log.

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

| Path                   | Writer                   | Content                                                                                                                                                             |
| ---------------------- | ------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `bin/sandcrew-bridge`  | `sandcrew-api`           | The bridge binary (`sandcrew-bridge.exe` on Windows).                                                                                                               |
| `start.json`           | `sandcrew-api`           | Start configuration (`BridgeStartConfig` as protobuf JSON): session ID, vendor, credential kind, permission mode, requested mode and configuration options.         |
| `inbox/`               | `sandcrew-api`           | One `BridgeInput` file per input.                                                                                                                                   |
| `events.jsonl`         | bridge                   | The append-only [event log](#event-log).                                                                                                                            |
| `agents/`              | bridge                   | Downloaded runtimes (`agents/node/<version>/`), ACP adapter installations (`agents/<vendor directory>/<adapter version>/`) and the npm cache (`agents/npm-cache/`). |
| `codex-home/auth.json` | `sandcrew-api` and Codex | The Codex ChatGPT sign-in file, when the session uses `CODEX_AUTH_JSON`.                                                                                            |
| `previous-session.md`  | `sandcrew-api`           | The previous transcript, for a session started from a transcript.                                                                                                   |
| `bridge.log`           | bridge                   | Structured bridge log, rotated at 10 MiB with one previous file.                                                                                                    |

- The agent runs with `<workspace>` as its working directory, so it can read and change these files. This is accepted: the sandbox is the security boundary, and the files hold nothing that the session's owner and agent may not see.

### Start

- `sandcrew-api` writes the binary, `start.json` and the credential file, then runs one long-running `ExecDevSession` with the command `<state>/bin/sandcrew-bridge run --state-dir <state>` and an environment with `SANDCREW_SESSION_ID`, `SANDCREW_VENDOR_CREDENTIAL` (for `API_KEY` and `CLAUDE_OAUTH_TOKEN`) and the selected session secrets. The exec has no timeout.
- The bridge removes every `SANDCREW_*` variable from the environment that it gives to child processes. It passes the credential to the agent in the form of the [vendor table](#vendors) and passes the session secrets unchanged.
- Order at start: write the `hello` event and `session_state` `STARTING`; check the vendor credential; install or find the pinned agent components; start the ACP adapter with the pinned Node.js (`node <adapter entry>`) and `<workspace>` as working directory; send ACP `initialize` (protocol version 1), `authenticate` when the [vendor table](#vendors) names a method, and `session/new` with `<workspace>` as `cwd` and no MCP servers; write `agent_ready` with the agent capabilities, then `config_options` and `mode_changed` with the advertised values; apply the start permission mode (`AUTO_APPROVE` when `start.json` has none), mode and configuration options; set `AgentActivity` `IDLE`; then process the inbox. Inbox files that arrive earlier wait in the inbox.
- The bridge offers no ACP client file system or terminal capability to the agent. The agent uses its own tools in the sandbox.
- A failure before the first `IDLE` writes a `bridge_error` (`AGENT_INSTALL_FAILED`, `CHECKSUM_MISMATCH`, `AGENT_START_FAILED` or `CREDENTIAL_MISSING`) and exits with code `2`. A missing credential for the selected vendor, an empty `SANDCREW_VENDOR_CREDENTIAL`, a missing `codex-home/auth.json` for `CODEX_AUTH_JSON` and a credential kind that the vendor does not accept are `CREDENTIAL_MISSING`. A missing or unparsable `start.json` and an agent that fails `initialize`, `authenticate` or `session/new` (within 5 minutes) are `AGENT_START_FAILED`; before the first prompt the message includes the agent's JSON-RPC error message, shortened to 200 characters.
- Exit codes: `0` after `agent_exited`, `1` for invalid arguments or a missing state directory (no event), `2` for a start failure, `3` for an event log failure. `SIGTERM` and `SIGINT` stop the agent; the bridge then writes `agent_exited` and exits with `0`.

### Inbox

- `sandcrew-api` writes each input as `inbox/<input_id>.json` with `WriteDevSessionFile`, which makes the file appear only after its final chunk (an atomic rename in the guest), so the bridge never reads a partial file. The bridge ignores other names and directories.
- The bridge reads the inbox directory after each file system notification for it (create and write events, and notification errors such as a full queue) and every 2 seconds, which finds a file whose notification was lost. When the platform or the file system gives no notifications, the bridge logs a warning and reads the directory every 250 milliseconds. It handles the files in `input_id` order (UUID v7 time order). Only names of the form `<canonical UUID v7>.json` are inputs.
- For each file, the bridge validates the `BridgeInput`, writes `input_accepted` or `input_rejected` (with `InputRejectionReason`) to the event log and stdout, applies or queues an accepted input, and then deletes the file. The acceptance is logged before the effect, so a restarted bridge never applies an input twice. An `input_id` that the event log already contains is deleted without a new event, so a repeated write delivers the input at most once. A file that is larger than 1 MiB, does not parse, has an `input_id` that differs from its name, or has a prompt without text or an empty required ID is rejected with `MALFORMED`; an input without a known case (a case of a later protocol minor version) is rejected with `UNSUPPORTED`.
- `prompt`: when no turn runs, the bridge writes a `user_message_chunk` with the text, sends ACP `session/prompt` and sets `WORKING`. During a turn, it queues the prompt and sends it after the turn ends. An `attachment_path` is mentioned in the prompt text by `sandcrew-api`; the bridge does not read the file.
- `permission_answer`: answers the pending ACP `session/request_permission` with the option and writes `permission_response`; an unknown request is rejected with `UNKNOWN_PERMISSION_REQUEST` and an option that the request does not offer with `UNKNOWN_OPTION`.
- `cancel_turn`: sends ACP `session/cancel`, answers every pending permission request with the outcome `cancelled`, and drops queued prompts. Without an active turn it is rejected with `NO_ACTIVE_TURN`.
- `set_mode`: with a `mode_id`, sends ACP `session/set_mode` (or sets the configuration option of category `mode` when the agent advertises modes that way) and writes `mode_changed`; a mode that the agent rejects writes a `bridge_error` `OPTION_REJECTED`. With a `permission_mode`, switches between `AUTO_APPROVE` and `ASK` as in [Permissions](#permissions).
- `set_config_option`: sends ACP `session/set_config_option`. An option or value that the agent rejects writes a `bridge_error` `OPTION_REJECTED`.

### Event log

- Every event except `credential_update` is first appended to `events.jsonl` as one line of compact protobuf JSON followed by `\n`, and then written to stdout as the same line. `sequence` starts at 1 and increases by 1; `log_end_offset` is the byte length of the log after the line.
- A new bridge process on an existing log reads it first: it continues after the highest `sequence`, and it remembers the input IDs of its `input_accepted` and `input_rejected` events. A partial last line (from a crash) is ended with `\n` and stays as it is.
- The bridge flushes the log after each line and calls `fsync` at each `turn_ended` and every 5 seconds while lines were written. A failed append is retried 3 times (a partial write continues with the rest of the line); then the bridge writes a `bridge_error` `EVENT_LOG_WRITE_FAILED` to stdout with the last logged sequence, stops the agent and exits with code `3`.
- `sandcrew-api` reads the log from a stored `log_end_offset` to recover events that it missed on stdout. The bridge never rewrites or truncates the log.
- Mapping from ACP follows the [protos contract](protos-sandcrew-api-contract.md#bridge-schema). ACP updates without a mapped case become `unknown_update` with the original JSON. ACP JSON-RPC errors of the agent, malformed messages, mapped updates that do not parse and agent requests for client methods that the bridge does not offer (answered with JSON-RPC `-32601`) become `bridge_error` `ACP_PROTOCOL_ERROR`; the bridge continues when the agent process continues. A failed `session/prompt` writes `ACP_PROTOCOL_ERROR` with the JSON-RPC error code only (the agent message can repeat prompt content), then `turn_ended` without a stop reason.
- `session_state` events report `AgentActivity` changes: `STARTING` at start, `WORKING` while a turn runs, `WAITING_FOR_PERMISSION` while an ASK-mode permission request is pending, and `IDLE` after `turn_ended` when no prompt is queued. A queued prompt starts directly after `turn_ended` without an `IDLE`.
- When the agent process ends, the bridge writes `agent_exited` with its exit code or signal and exits with code `0`.

### Permissions

- New sessions start in `AUTO_APPROVE`. The bridge starts the vendor's auto-approve mode from the [vendor table](#vendors) when the agent advertises it. It answers every remaining ACP permission request with the first option of kind `ALLOW_ONCE`, otherwise `ALLOW_ALWAYS`, and writes `permission_request` and then `permission_response` with `auto_approved` true. A request without an allow option is handled as in `ASK`. A switch to `AUTO_APPROVE` answers the pending requests in the same way.
- In `ASK`, the bridge switches the agent to the vendor's asking mode from the [vendor table](#vendors) when the agent advertises it, writes `permission_request` and waits for a `permission_answer` without a time limit. The pending request counts as waiting for input. A `cancel_turn` or the end of the turn answers it with the outcome `cancelled`.
- `mode_changed` reports the agent mode and the `PermissionMode` together.
- Permission prompts are advisory. The agent and the bridge run in the same sandbox, and an agent with shell access can bypass them.

### Vendors

| Vendor (`AgentVendor`) | MVP | Integration                                                                                                                                                                | Credential to the agent                                                                                                                                                                                 | Auto-approve mode   | Asking mode       |
| ---------------------- | --- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------- | ----------------- |
| `CLAUDE_CODE`          | yes | The ACP adapter `@agentclientprotocol/claude-agent-acp` (listed in the ACP agents directory) with Node.js; it bundles Claude Code through `@anthropic-ai/claude-agent-sdk` | `CLAUDE_OAUTH_TOKEN` as `CLAUDE_CODE_OAUTH_TOKEN`; `API_KEY` as `ANTHROPIC_API_KEY`. The bridge also sets `IS_SANDBOX=1`, because the adapter offers `bypassPermissions` to a root process only with it | `bypassPermissions` | `default`         |
| `OPENAI_CODEX`         | yes | The ACP adapter `@agentclientprotocol/codex-acp` (listed in the ACP agents directory) with Node.js; it starts Codex from `@openai/codex`                                   | `CODEX_HOME=<state>/codex-home` always; `CODEX_AUTH_JSON` through `codex-home/auth.json`; `API_KEY` as `CODEX_API_KEY`, and the bridge calls ACP `authenticate` with the method `api-key`               | `agent-full-access` | `workspace-write` |

- Reserved environment names, which session secrets must not use: every `SANDCREW_*` name, `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`, `OPENAI_API_KEY`, `CODEX_API_KEY`, `CODEX_HOME` and `IS_SANDBOX`. The bridge removes these names from the environment that the agent inherits (case-insensitively, for Windows) and then sets the names of the vendor row.
- Headless fallback: a vendor without ACP is wrapped by a bridge adapter that runs the vendor's headless CLI mode and converts its output to the same events (`hello.wrapped_headless` true). The bridge defines the adapter interface (`HeadlessAdapter` in `internal/agent`) but has no implementation, because both MVP vendors use ACP. Candidates recorded for later vendors, to be verified against official documentation before use: Claude Code `claude -p --output-format stream-json` with `--resume`, Codex `codex exec --json` with `codex exec resume`, Cursor `agent -p --output-format stream-json` with `--resume`, Devin `devin -p`.
- Recorded decisions, checked against the adapter sources at the pinned versions (`claude-agent-acp` 0.113.0, `codex-acp` 2.12.1):
  - Guest OSes: both adapters and their agents run on Linux (glibc), macOS and Windows on every release target, through the per-platform optional packages of `@anthropic-ai/claude-agent-sdk` and `@openai/codex`. The official Node.js archives need glibc; a musl guest needs a preinstalled Node.js of the pinned version.
  - Session load and resume: both adapters advertise `loadSession` and `sessionCapabilities.resume`; `agent_ready` reports them. The MVP does not call `session/load` or `session/resume`.
  - Modes: Claude Code advertises `default`, `acceptEdits`, `plan`, `auto` and `bypassPermissions`. Codex advertises `read-only`, `workspace-write`, `agent` (its default, which reviews actions automatically) and `agent-full-access`. Both also expose the mode as a configuration option of category `mode`, and the model, the reasoning level and the speed as configuration options.
  - The bridge sends no `clientCapabilities.session.configOptions.boolean`, so the adapters send their boolean options as select options. The bridge maps an ACP boolean option to the value IDs `true` and `false` and sets it with `type: "boolean"`.

### Pinned agent installation

- Each bridge release embeds a pin file (`cmds/sandcrew-bridge/internal/pins/pins.json`) with the runtime and, for each vendor, the ACP adapter package, the agent package that it brings and the release targets. The runtime is Node.js (the version of the repository `.nvmrc`), with its official download URL, its archive root and layout and the SHA-256 of the archive for each release target, copied from the release's `SHASUMS256.txt` (`.tar.gz` on Linux and macOS, `.zip` on Windows). The adapter is pinned by a committed `package.json` and `package-lock.json` in `internal/pins/npm/<vendor directory>/` whose `sha512` integrity hashes cover every package, including the per-platform optional packages.
- At start, the bridge uses a component that the image already contains when its reported version equals the pinned version: `node` on `PATH` whose `node --version` equals the pin and that has npm at the archive layout, and the adapter package in the global `node_modules` of that Node.js when its `package.json` version equals the pin. Otherwise it downloads the pinned archive, verifies the SHA-256 before it extracts it (entries and links must stay inside the target directory) into `agents/node/<version>/`, and installs the adapter into `agents/<vendor directory>/<adapter version>/` with `npm ci --ignore-scripts` of the pinned Node.js and the embedded lockfile, which verifies each package's integrity. A marker file with the archive or lockfile digest marks a complete installation, so a later start reuses it.
- A download that fails (after 3 attempts with backoff, at most 10 minutes in total) and an `npm ci` that fails 3 times write `AGENT_INSTALL_FAILED`; an archive digest mismatch and an npm `EINTEGRITY` failure write `CHECKSUM_MISMATCH` without a retry. Both fail the session start with that reason.
- Renovate updates the pins: the npm manager updates the adapter `package.json` and its lockfile, and a custom manager updates the Node.js version in `pins.json`. A Node.js update must copy the new checksums and set `checksumsVersion`; the pin tests fail until it does, and they check that `pins.json` names the adapter and agent versions of the lockfile. A pin change is a bridge change and reaches only new sessions.

### Protocol versions

- The first event of every bridge process is `hello` with `protocol_version`. The [protos contract](protos-sandcrew-api-contract.md#protocol-versions) owns the major and minor rules.
- SandCrew never replaces the bridge or the agent of an existing session. A session keeps the bridge and the pinned agent components of its start for its whole life, also across pauses.

### Credential updates

- For `CODEX_AUTH_JSON`, the bridge watches `codex-home/auth.json` (modification time and SHA-256). It checks the file 200 milliseconds after the last file system notification for it, so a check does not see a partly written file, and every 10 seconds; it watches the `codex-home` directory, so a file that Codex replaces with a rename stays watched. Without notifications, it logs a warning and checks only every 10 seconds. When the content changes, it writes a `credential_update` event with an increased `revision` to stdout only. A new modification time with the same content, and a deleted file, are no change. The event carries no content; `sandcrew-api` reads the file itself.
- A failure to watch the file writes `bridge_error` `CREDENTIAL_WATCH_FAILED` once and the session continues.
- Open decision: whether a Codex refresh in one session invalidates the token in other sessions that use the same credential. It needs a real ChatGPT account and was not verified with the adapter implementation; it stays open until a real-account test records the result here.

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

- The workspace package `cmds/sandcrew-bridge/package.json` (`sandcrew-bridge`, private) has `build` (`scripts/build.mjs`: cross-compilation of the five targets with `CGO_ENABLED=0` into `dist/<target>/`, with the version and commit set through `-ldflags`), `lint` (`go vet`) and `test` (`go test`) scripts that wrap the Go toolchain.
- Unit tests with a fake ACP agent that speaks JSON-RPC over pipes (`internal/bridge`), and package tests for the ACP connection, the event log, the installer (fake archives, checksums and a fake HTTP server), the credential watcher, the vendor environment and the pins: the mapping of every event kind and of unknown updates, inbox handling (prompt, queued prompt, permission answer, cancel, mode change, configuration option, repeated input, malformed file, delivery by notification without polling), event log sequence numbers and offsets, auto-approval and its record, `ASK` mode, the `hello` event and its version, pinned component checks with a digest mismatch and a download failure, `auth.json` change detection by interval and by notification (also for a rename over the file), and the exit codes.
- Run `git ls-files -z -- '*.go' | xargs -0 --no-run-if-empty gofmt -l`, `go vet ./...` and `go test ./...` from the repository root according to `cmds/AGENTS.md`.
- Acceptance on real Linux, macOS and Windows guests with real vendor accounts is not performed in the MVP and is recorded as not performed.

## Dependencies and Integrations

- Upstream: the vendor agents and ACP adapters at their pinned versions, and the runtimes they need.
- Downstream: [`sandcrew-api`](servers-sandcrew-api-foundation.md), through SandRun dev session RPCs.
- Schema: [`protos/delino/sandcrew/v1/`](protos-sandcrew-api-contract.md). The bridge must not import code from `servers/`.
- Third-party Go modules: `github.com/fsnotify/fsnotify` (BSD-3-Clause) for the inbox and credential file notifications (inotify on Linux, kqueue on macOS, `ReadDirectoryChangesW` on Windows; pure Go, so the binaries stay static with `CGO_ENABLED=0`), `golang.org/x/sys` (BSD-3-Clause) for fsnotify and the archive extraction, and `google.golang.org/protobuf` (BSD-3-Clause) for the schema.
- Other new dependencies need owner approval and a [dependency security](repository-dependency-security-contract.md) review in the change that adds them.

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
