# packages-ownsand-sdk foundation contract

## Scope

This contract owns the two OwnSand SDKs of [`ownsand`](project-ownsand.md):

- `packages/ownsand-sdk/` (`planned`): TypeScript SDK, component `ownsand-sdk-ts`.
- `packages/ownsand-sdk-go/` (`planned`): Go SDK, component `ownsand-sdk-go`, import path `github.com/delinoio/mono/packages/ownsand-sdk-go`.

Both SDKs give programs and the [CLI](cmds-ownsand-cli-foundation.md) one client for the public API of the [protos contract](protos-ownsand-api-contract.md), including the data path. Both use Apache-2.0.

## Runtime and Language

- TypeScript SDK: ES modules for Node.js 24 (the `.nvmrc` baseline). It uses Connect for ECMAScript with the TypeScript bindings generated from the schemas. Record where the generated TypeScript code lives with the first schema. Browser support is not part of this version; the web console uses the generated bindings with `@connectrpc/connect-query` directly.
- Go SDK: Go in the root module. It uses the committed Go bindings in `protos/delino/ownsand/v1/` and does not generate code.
- Node.js has no built-in WebRTC implementation, so the TypeScript SDK's peer-to-peer data path needs a WebRTC library. The library is an open decision and a new dependency. Until it is approved, the TypeScript SDK uses the relay only.
- The Go SDK uses pion for the peer-to-peer data path. pion is a new dependency that needs owner approval.

## Users and Operators

- Programs that use an organization API key: CI jobs, AI agents and services.
- People who use a Logto access token that their own program obtained.
- The [CLI](cmds-ownsand-cli-foundation.md), which is built on the Go SDK.

## Interfaces and Contracts

### Client construction

- The client takes the API origin (default `https://ownsand.api.delino.io`) and one credential: an API key or a function that returns a current Logto access token.
- Without an explicit credential, both SDKs read the `OWNSAND_API_KEY` environment variable. They read no other environment variables for credentials.
- The client takes an optional organization ID and uses it for every RPC that needs one; without it, the SDKs read `OWNSAND_ORG`. A call can override it. An RPC that needs an organization fails before it is sent when neither is set.
- The client never logs or includes the credential in errors.

### Operations

The SDKs expose the public services of the [protos contract](protos-ownsand-api-contract.md#public-services) with idiomatic names, and a sandbox handle with these operations:

- `create`, `get`, `list`, `delete`, `pause`, `resume`.
- `exec`: returns a stream of stdout and stderr chunks and the exit code. A data path failure is an error, never an exit code. When a continuation loses output (`output_truncated`), the stream reports a truncation event with the missing offsets, and the result is marked incomplete; the SDK never presents truncated output as complete.
- `readFile` and `writeFile` (Go: `ReadFile` and `WriteFile`): stream contents in chunks and accept readers and byte buffers. `writeFile` uploads in chunks and finishes the write only after the last chunk, as the [file transfer contract](protos-ownsand-api-contract.md#file-transfer) requires. The SDK keeps the `write_id`, the offset and the chunk or `finish` request that is not acknowledged yet. After a failure or a session end, it sends that exact request again, on the new session or on the relay, before it reads more input.
- `snapshot`, plus snapshot export and template operations.

`create` requires `idleTtl` and `maxTtl` (Go: `IdleTTL` and `MaxTTL`) and the resources. The SDKs do not supply a default TTL.

### Data path

- For exec and file operations, an SDK that supports peer-to-peer calls `GetIceServers` and then `OpenSession` once per sandbox handle and tries a WebRTC data channel to the agent. A relay-only SDK, and a client with the transport forced to `RELAY`, does not call `OpenSession`; it calls the relay RPCs directly.
- When the data channel does not open within 10 seconds, or fails during a session, the SDK uses the relay RPCs for that sandbox handle. The SDK never runs an exec again, because the command may have run. The SDK generates the `exec_id` before it sends an exec, so it can continue an exec that was in progress with `AttachExec` on the relay, also when the stream ended before the `started` event. It generates a `read_id` for each file read and continues the read from the next offset with the same `read_id`. When `AttachExec` fails with `not_found`, the exec fails with a data path error and its outcome is unknown.
- A relay stream can end before the operation ends because of the Railway edge limits. The SDK continues it with `AttachExec` or `ReadFile` from the next offset without a visible error. A `ReadFile` that fails with `not_found` because its `read_id` expired is a visible error.
- After any session end (15 minutes idle, the 1-hour absolute expiry, a revocation or the agent losing its channel), the SDK opens a new session only when an operation is open or the caller starts one, so that an idle handle never keeps the sandbox active. It then calls `GetIceServers` and `OpenSession` and tries peer-to-peer again. It continues every operation that was open on the ended session on the new session: an exec with `AttachExec` from the received offsets, a file read with the same `read_id` from the next offset and a file write with its unacknowledged request. When a new session cannot be authorized, those operations end with that authorization error. Only an ICE failure, or a new session that cannot open, moves the handle to the relay. The SDK recognizes a session end by the session-end frame that the agent sends before it closes the data channel; a data channel that closes without that frame is a failure. A revoked credential then fails with `unauthenticated` or `permission_denied`, and a user whose membership ended gets `not_found`, according to the [control plane authorization rules](servers-ownsand-api-foundation.md#authorization).
- The caller can force a transport (`PEER_TO_PEER` or `RELAY`) for tests. The SDK reports the transport in use.
- Both transports carry the same `DataFrame` messages and give the same results.

## Storage

The SDKs persist nothing. They keep sessions and credentials in memory only.

## Security

- Credentials stay in memory. The SDKs never write them to disk or logs.
- The SDKs connect only to the configured API origin, the STUN servers that `GetIceServers` returns and the agent's ICE candidates.
- The data channel accepts only the DTLS fingerprint in the agent's answer that `OpenSession` returned over the authenticated control plane connection.

## Logging

The SDKs log nothing by default. An optional logger receives RPC names, sandbox IDs, transport selection, durations and Connect codes, never credentials, exec content or file content.

## Build and Test

- TypeScript SDK: package scripts `build`, `lint`, `typecheck` and `test`, run through root Turborepo tasks. Record them with the implementation.
- Go SDK: `gofmt` check, `go vet ./...` and `go test ./...` from the repository root.
- Required tests: TTL validation before the request, transport fallback from peer-to-peer to relay, relay-only operation without `OpenSession`, exec continuation with `AttachExec` (also for a command that produces no output for more than 5 minutes), an exec that is never run again, chunked file writes, and credential redaction in errors.

## Dependencies and Integrations

- Upstream: the [control plane](servers-ownsand-api-foundation.md) and the [protos contract](protos-ownsand-api-contract.md).
- Peers: the [device agent](cmds-ownsand-agent-foundation.md#data-path).
- Downstream: the [CLI](cmds-ownsand-cli-foundation.md) uses the Go SDK.
- New dependencies: Connect and protobuf runtimes for both languages, pion for Go and a WebRTC library for Node.js. Each needs owner approval.

## Change Triggers

- Update this contract with every public RPC change in the [protos contract](protos-ownsand-api-contract.md).
- Update the [CLI contract](cmds-ownsand-cli-foundation.md) when the Go SDK surface that the CLI uses changes.
- Update [project-ownsand.md](project-ownsand.md) when the package paths, the publication target or the data path changes.
- Update `packages/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-ownsand.md](project-ownsand.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-dependency-security-contract.md](repository-dependency-security-contract.md)
- [Connect for ECMAScript](https://connectrpc.com/docs/web/getting-started)
- [Connect for Go](https://connectrpc.com/docs/go/getting-started)
- [pion WebRTC](https://github.com/pion/webrtc)
