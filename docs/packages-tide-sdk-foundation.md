# packages-tide-sdk foundation contract

## Scope

This contract owns the tide SDKs of [`tide`](project-tide.md):

- `packages/tide-sdk/` (workspace package `tide-sdk`), component `tide-sdk-ts`: TypeScript SDK.
- `go/tide/`, component `tide-sdk-go`: Go SDK in the root Go module, import path `github.com/delinoio/mono/go/tide`, and its test fake `go/tide/tidetest`. It is under `go/` because external users import it (see the [workflow contract](repository-workflow-contract.md#public-go-packages)).

The SDKs give programs and the [CLI](cmds-tide-cli-foundation.md) typed access to the public services of the [protos contract](protos-tide-api-contract.md).

## Runtime and Language

- TypeScript SDK: ES modules for Node.js 24 and browsers, built on Connect-ES and the generated TypeScript code.
- Go SDK: Go in the root module `github.com/delinoio/mono`, built on connect-go and the committed Go bindings. Logging: `log/slog` through a logger that the caller passes.
- Both SDKs use Apache-2.0 and must not import code from `servers/`.

## Users and Operators

- The tide CLI and the `tide mcp` MCP server (Go SDK).
- Programs of tide users that read the queue or manage runner profiles with a Logto access token, or that use dev sessions with a Logto access token or a personal API key.

## Interfaces and Contracts

- A client takes the API origin (default `https://tide.api.delino.io`) and a token source that returns a Logto access token for the API resource `https://tide.api.delino.io`, or a personal API key.
- The SDKs expose every public service of the protos contract with the generated request and response types. They add no behavior that the server does not define.
- Helpers:
  - Pagination iterators for every list RPC.
  - A `WatchQueue` helper that reconnects after each stream end with exponential backoff (1 second to 30 seconds) and reports a gap when it reconnects, so the caller can reload the state.
  - Both SDKs: an exec helper that generates the `exec_id`, continues with `AttachDevSessionExec` after each stream end and reports lost output.
  - Go SDK only: a dev session wait helper (repeated `GetDevSession` until the session is `RUNNING`, ends, or the caller's deadline passes), and the [sync](servers-tide-api-foundation.md#dev-session-sync) and [download](servers-tide-api-foundation.md#dev-session-download) helpers that the CLI and the MCP server share. The Go SDK owns their file selection, manifest, lock, archive, limit and path-safety rules, and the exact guest commands per guest OS, which it records with the implementation.
- Errors keep the Connect error code.

### TypeScript SDK surface

- `TideClient({ origin?, token, fetch?, clock? })`. `token` is a string or a function that returns one (a Logto access token or a personal API key); the client calls the function before every call and stores no token. The signal of a call also stops the wait for the function, and a token that arrives after the abort starts no request. The transport is Connect-Web with the binary Connect protocol. An interceptor and a guarded fetch refuse every URL outside the configured origin, and the token is redacted from error messages, also from the messages of the `cause` chain.
- Service clients with the generated types: `organizations`, `members`, `ownSandLink`, `gitHub`, `agentIntegrations`, `runnerProfiles`, `work`, `apiKeys`, `devSessions` and `audit`. `createTideServices(transport)` creates the same clients on another transport, for example the console's.
- Pagination: `client.iterate.<list>` (`organizations`, `invitations`, `members`, `gitHubInstallations`, `agentIntegrations`, `runnerProfiles`, `requests`, `runners`, `apiKeys`, `devSessionProfiles`, `devSessions` and `auditEvents`), one async iterator for each list RPC with a page token, built on `paginate`. An iterator ends with a `TideError` when the server repeats a page token.
- `watchQueue(work, { organizationId, signal })` (also `client.watchQueue`) yields `Request`, `Runner`, `SnapshotComplete` and `Gap` events and skips keepalives. After a clean end or a retryable failure it waits 1 second, doubled for each stream that ended before its snapshot was complete, at most 30 seconds, then yields `Gap` and opens a new stream. A complete snapshot resets the delay. Retryable failures are transport interruptions without a server status and the codes `unavailable`, `resource_exhausted`, `aborted` and `deadline_exceeded`; every other status ends the iterator with that `ConnectError`. The signal ends it without an error.
- `execDevSession(devSessions, input)` and `runDevSessionExec` (also on the client) generate a UUID v7 `exec_id` (Web Crypto), call `ExecDevSession` exactly once and continue with `AttachDevSessionExec` from the next stdout and stderr offsets after each stream end: at once after a clean end, with the same backoff after a retryable failure. They yield output in order without duplicates, report missing ranges (`output_truncated` or a skipped offset) as `OutputLost`, and end with `Exited`, `Failed` or `SandboxEnded`. When `ExecDevSession` failed before any event and the attach answers `not_found`, the command never started and the helper ends with the error of `ExecDevSession`.
- Errors: server errors stay `ConnectError` values with their code; `connectCodeOf` reads the code through `cause` links. The SDK's own errors are `TideError` and `TideConfigError`.
- Runtime sources import no `node:` module, so the package runs in Node.js 24 and in browsers.

### Go SDK surface

- `tide.New(tide.Options{BaseURL, APIKey | TokenSource, HTTPClient, Logger})` returns the client. `BaseURL` defaults to `tide.DefaultBaseURL`. The client sends the credential as `Authorization: Bearer` only to that origin, follows no redirects and uses no cookie jar. An `http://` origin uses HTTP/2 with prior knowledge (h2c).
- `Organizations()`, `Members()`, `OwnSandLinks()`, `GitHub()`, `AgentIntegrations()`, `RunnerProfiles()`, `Work()`, `APIKeys()`, `Audit()` and `DevSessions()` return the generated `tidev1connect` clients. Errors are `*connect.Error` values with the server code; the SDK removes the credential from error messages and keeps the code, details and wire flag.
- Pagination: `AllOrganizations`, `AllInvitations`, `AllMembers`, `AllGitHubInstallations`, `AllAgentIntegrations`, `AllRunnerProfiles`, `AllRequests`, `AllRunners`, `AllAPIKeys`, `AllAuditEvents`, `AllDevSessionProfiles` and `AllDevSessions` return `iter.Seq2` iterators; `tide.Collect` gathers one into a slice.
- `WatchQueue(ctx, organizationID, handler)` reports `QueueEventRequest`, `QueueEventRunner`, `QueueEventSnapshotComplete` and, before each reconnect, `QueueEventGap`. It reconnects after transient failures and normal stream ends and stops on other server codes.
- `Exec` and `AttachExec` report `ExecEventStarted`, `ExecEventStdout`, `ExecEventStderr` and `ExecEventOutputTruncated` (with the missing byte ranges) and return an `ExecResult` (`exited`, `failed` or `sandbox_ended`, `Incomplete` and the next offsets). Exec sends `ExecDevSession` once. A server error of that call is returned unchanged. A later failure, or the end of the caller context, returns `*ExecStreamError` with the next offsets.
- `WaitDevSession` polls `GetDevSession` every 2 seconds by default. When the caller context ends, it returns the last dev session that it read and the context error.
- `Sync` returns `SyncResult` (uploaded, deleted and skipped counts, submodules, bytes sent, warnings). `Download` returns `DownloadResult` (local path, file count, bytes, warnings). Both report `TransferProgress`. SDK-side failures are `*connect.Error` values: `failed_precondition` wraps `*LimitError`, `*PathRuleError`, `ErrSyncLocked` and `ErrNotGitWorkTree`, `not_found` a missing guest path.
- `go/tide/tidetest` is an in-memory fake of `tide-api` for tests. Its guest is the local machine: exec runs local processes and file RPCs read and write local paths, so a test uses a local directory as the workspace path. It is not a sandbox.

### Go SDK sync and download rules

The SDK implements the [sync](servers-tide-api-foundation.md#dev-session-sync) and [download](servers-tide-api-foundation.md#dev-session-download) rules with these details:

- File selection: `git -C <dir> -c core.quotepath=off ls-files -z --cached --others --exclude-standard`, after `git rev-parse --is-inside-work-tree`. `git ls-files -z --stage` finds submodules (mode `160000`), which are skipped. An untracked nested repository (a listed path that ends with `/`) is also skipped. A tracked path that is missing locally counts as deleted. On a Windows local machine the executable bit comes from the index mode.
- Manifest `<workspace_path>.tide-sync.json`: `{"version":1,"files":{"<path>":{"size":N,"sha256":"<hex>","mode":M}}}`. The path is slash-separated and relative to the workspace. The mode is the Git mode: `0o100644`, `0o100755` or `0o120000` for a symbolic link, whose size and SHA-256 describe the link target. A manifest with another version or invalid JSON counts as empty.
- Path rules: on Windows and macOS guests, the case collision check compares every file and every parent directory after Unicode upper-then-lower case mapping. On Windows guests, a path element is invalid when it is a reserved device name (`CON`, `PRN`, `AUX`, `NUL`, `CONIN$`, `CONOUT$`, `COM1` to `COM9`, `LPT1` to `LPT9` and the superscript variants), also with an extension, when it contains `<>:"\|?*` or a control character, or when it ends with a space or a dot.
- Manifest check under the lock: after the lock command succeeds, the SDK reads the manifest again and compares its entries with the first read. When they differ, it makes the plan again (changed, deleted and the size limits) from the new manifest before it uploads anything.
- Order of step 7: the archive `<workspace_path>.tide-sync-<uuid>.tar` (PAX tar, regular files with mode `0644` or `0755`, parent directories with `0755`) is uploaded. The SDK hashes each file again while it writes the archive: when the size or the SHA-256 of a file changed after the SDK listed it, the upload fails before the guest extracts the archive, and the sync asks for a new sync. Then the deletion list `<workspace_path>.tide-sync-<uuid>.delete` is uploaded: NUL-separated on Linux and macOS, newline-separated with backslashes on Windows. When a changed file or link has the path of a guest directory that holds deleted entries, the prune list `<workspace_path>.tide-sync-<uuid>.prune` (same format) is uploaded. It lists the directories from the parent of each such deleted entry up to that path, deeper directories first. On Windows guests, the symbolic links are not in the archive. They are in the link list `<workspace_path>.tide-sync-<uuid>.links` (`<path>\t<target>` lines). One apply command deletes the listed files, removes the listed directories when they are empty, extracts the archive and creates the links. The SDK deletes before it extracts, so that a path can change from a file to a directory, and from a directory to a file. The apply command removes no directory that holds other files, such as files that the guest created: the apply then fails and the guest keeps those files. Then it writes the new manifest with `WriteDevSessionFile`, and one clean-up command removes the temporary files and the lock. A symbolic link that a Windows guest cannot create is a warning and is left out of the manifest, so the next sync tries it again. The SDK reads every `TIDE-LINK-FAILED` line of the apply output, not only the start of the output that it keeps. When the apply output is incomplete (the exec lost output, or a line is longer than 64 KiB), the SDK cannot tell which links the guest created. It then leaves every link of that sync out of the manifest with a warning. When a sync fails after it took the lock, the SDK runs the clean-up command, so the old manifest stays and the lock is released. A stopped process leaves the lock for `force`.
- Uploads use one `write_id` per file, chunks of at most 1 MiB and `final` on the last chunk. The SDK retries a chunk after a transient failure with the same `write_id` and offset. A read uses one `read_id`: after a transient failure, or a stream that ends without its end message, the SDK continues from the last offset with the same `read_id`, so the guest serves the rest from the same file snapshot.
- Download: the probe command reports `file <size>` or `dir <archive size>`. For a directory, it first packs the directory into `<workspace_path>.tide-download-<uuid>.tar`. Exit code `66` means that the path does not exist. The SDK always removes the guest archive afterwards. Local extraction uses `os.Root` in a temporary directory `.tide-download-*` next to the destination. It rejects absolute entries, entries outside the destination, symbolic links whose targets leave the destination and hard links to entries that are not in the archive. A link target that stays inside as text can still leave the destination through other links of the archive (for example `a/b -> ..` and `a/b/l -> ../x`). After the last entry, the SDK therefore resolves every symbolic link of the extracted tree from the directory where the link is, through the other links, and rejects the download when a target leaves the destination or a chain has more than 255 links. It skips other entry types with a warning. A symbolic link that the local system cannot create is a warning. The result replaces an existing destination only after the download succeeded.
- Guest commands get every path through environment variables of the exec request (`TIDE_SYNC_LOCK`, `TIDE_SYNC_FORCE`, `TIDE_SYNC_WORKSPACE`, `TIDE_SYNC_ARCHIVE`, `TIDE_SYNC_DELETE_LIST`, `TIDE_SYNC_PRUNE_LIST`, `TIDE_SYNC_LINK_LIST`, `TIDE_DOWNLOAD_PATH` and `TIDE_DOWNLOAD_ARCHIVE`), never in the command text. On macOS guests the SDK also sets `COPYFILE_DISABLE=1`, so that tar adds no AppleDouble files.
- Linux and macOS guests (`sh -c <script> tide`; GNU, BSD and BusyBox `tar`, `xargs -0`):
  - lock: `if [ "$TIDE_SYNC_FORCE" = 1 ]; then rm -rf "$TIDE_SYNC_LOCK" || exit 1; fi; if mkdir "$TIDE_SYNC_LOCK"; then exit 0; fi; if [ -e "$TIDE_SYNC_LOCK" ]; then exit 75; fi; exit 1`. Exit code `75` means that the lock exists.
  - apply: `set -e; mkdir -p "$TIDE_SYNC_WORKSPACE"; cd "$TIDE_SYNC_WORKSPACE"; xargs -0 rm -f -- < "$TIDE_SYNC_DELETE_LIST"` (only with a deletion list); `rmdir` of each directory in `$TIDE_SYNC_PRUNE_LIST` that exists and is not a symbolic link, through `xargs -0 sh -c` (only with a prune list); `tar -x -m -o -f "$TIDE_SYNC_ARCHIVE"` (only with an archive). `-m` gives the extracted files the current time, so that guest builds see them as changed. `-o` keeps the guest user as the owner.
  - clean-up: `rm -f` of the archive, the deletion list and the prune list, then `rm -rf "$TIDE_SYNC_LOCK"`.
  - download probe: `tar -c -f "$TIDE_DOWNLOAD_ARCHIVE" -C "$TIDE_DOWNLOAD_PATH" .` for a directory, `wc -c` for the size, and exit code `66` for a missing path. The archive is removed with `rm -f "$TIDE_DOWNLOAD_ARCHIVE"`.
- Windows guests (`powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand <base64 of the UTF-16LE script>`, Windows PowerShell 5.1 and `%SystemRoot%\System32\tar.exe` of Windows 10 and later). The encoded command contains only base64 characters, so no Windows command-line quoting applies:
  - lock: removes the lock with `Remove-Item -Recurse -Force` when force is set, then runs `cmd.exe /d /c 'mkdir "%TIDE_SYNC_LOCK%" 2>nul'`. The `mkdir` of `cmd.exe` fails when the directory exists, so the check and the creation are one step. Exit code `75` means that the lock exists.
  - apply: `[System.IO.Directory]::CreateDirectory` of the workspace; `Remove-Item -LiteralPath -Force` for each listed deletion; `[System.IO.Directory]::Delete` (not recursive) for each listed directory that exists; `tar.exe -x -m -f <archive> -C <workspace>`; `New-Item -ItemType SymbolicLink` for each link, with a `TIDE-LINK-FAILED <path>` line on standard output when it fails.
  - clean-up: `Remove-Item -LiteralPath -Force` of the temporary files and `Remove-Item -Recurse -Force` of the lock.
  - download probe: `tar.exe -c -f <archive> -C <path> .` for a directory and `(Get-Item -LiteralPath <path>).Length` for the size.
- Verification: the Go tests run the Linux and macOS commands against the local `sh` and `tar` through `tidetest`. The Windows commands are only generated and checked for their form. They have not yet run on a real Windows guest, and the Linux and macOS commands have not yet run on real OwnSand guests.

## Storage

The SDKs store nothing. Token storage belongs to the caller.

## Security

- The SDKs send tokens only to the configured API origin.
- Credential fields go only in set requests; the SDKs never log request messages.

## Logging

The Go SDK logs retries and reconnects at debug level through the caller's logger. The TypeScript SDK does not log. No log contains tokens or credentials.

## Build and Test

- TypeScript SDK package scripts: `build` (`tsc -p tsconfig.build.json` into the ignored `dist/`), `lint` (`node scripts/lint.mjs`: the Apache-2.0 SPDX header, no `console` calls, no `node:` imports in runtime sources and no imports from `servers/`), `typecheck` (`tsc --noEmit`) and `test` (`rstest run`). The tests run the real Connect-Web transport against an in-memory fake server (`src/testing/fake-server.ts`, a Connect router behind a fetch function) and a recording clock.
- Go SDK commands (from the repository root): `go test ./go/tide/...` (the sync and download tests need `git`, `sh` and `tar` on the local machine and skip on Windows hosts), `go vet ./go/tide/...`, and `GOOS=windows go vet ./go/tide/...` and `GOOS=darwin go vet ./go/tide/...` for the other targets. The Go SDK is not a workspace package and has no package scripts; CI runs it in the root Go jobs.
- Tests: pagination, `WatchQueue` reconnects, exec attach after stream rotation and error mapping against a fake server. Go SDK sync and download tests: file selection with tracked, untracked and ignored files, incremental upload and deletion with the manifest, the size limits, case collisions and invalid Windows names, the lock, symbolic links, path traversal on extraction, and a restart after a stopped sync.

## Dependencies and Integrations

- Upstream: the generated code of `delino.tide.v1`, Connect-ES and connect-go.
- Publication (recorded decision): the workspace package is named `tide-sdk` and is `private`, so neither pnpm nor Changesets publishes it, like `ownsand-sdk`. The npm package name and the publication workflow of the TypeScript SDK stay an [open decision](project-tide.md#open-decisions), together with the same decision of OwnSand.

## Change Triggers

- Update this contract when the SDK surface or its helpers change.
- Update `packages/AGENTS.md` only when development procedures, directory ownership or development rules change; follow the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-tide.md](project-tide.md)
- [protos-tide-api-contract.md](protos-tide-api-contract.md)
- [packages-ownsand-sdk-foundation.md](packages-ownsand-sdk-foundation.md)
- [repository-defaults.md](repository-defaults.md)
