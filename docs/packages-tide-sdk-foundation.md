# packages-tide-sdk foundation contract

## Scope

This contract owns the tide SDKs of [`tide`](project-tide.md):

- `packages/tide-sdk/` (`planned`), component `tide-sdk-ts`: TypeScript SDK.
- `go/tide/` (`planned`), component `tide-sdk-go`: Go SDK in the root Go module, import path `github.com/delinoio/mono/go/tide`. It is under `go/` because external users import it (see the [workflow contract](repository-workflow-contract.md#public-go-packages)).

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

## Storage

The SDKs store nothing. Token storage belongs to the caller.

## Security

- The SDKs send tokens only to the configured API origin.
- Credential fields go only in set requests; the SDKs never log request messages.

## Logging

The Go SDK logs retries and reconnects at debug level through the caller's logger. The TypeScript SDK does not log. No log contains tokens or credentials.

## Build and Test

- Package scripts `build`, `lint`, `typecheck` and `test` for the TypeScript SDK, and Go tests for the Go SDK. Record them with the implementation.
- Tests: pagination, `WatchQueue` reconnects, exec attach after stream rotation and error mapping against a fake server. Go SDK sync and download tests: file selection with tracked, untracked and ignored files, incremental upload and deletion with the manifest, the size limits, case collisions and invalid Windows names, the lock, symbolic links, path traversal on extraction, and a restart after a stopped sync.

## Dependencies and Integrations

- Upstream: the generated code of `delino.tide.v1`, Connect-ES and connect-go.
- Publication: the npm package name and the publication workflow of the TypeScript SDK are open decisions, together with the same decision of OwnSand.

## Change Triggers

- Update this contract when the SDK surface or its helpers change.
- Update `packages/AGENTS.md` only when development procedures, directory ownership or development rules change; follow the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-tide.md](project-tide.md)
- [protos-tide-api-contract.md](protos-tide-api-contract.md)
- [packages-ownsand-sdk-foundation.md](packages-ownsand-sdk-foundation.md)
- [repository-defaults.md](repository-defaults.md)
