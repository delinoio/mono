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
- The client never logs or includes the credential in errors.

### Operations

The SDKs expose the public services of the [protos contract](protos-ownsand-api-contract.md#public-services) with idiomatic names, and a sandbox handle with these operations:

- `create`, `get`, `list`, `delete`, `pause`, `resume`.
- `exec`: returns a stream of stdout and stderr chunks and the exit code. A data path failure is an error, never an exit code.
- `readFile` and `writeFile` (Go: `ReadFile` and `WriteFile`): stream contents in chunks and accept readers and byte buffers. `writeFile` uploads in chunks and finishes the write only after the last chunk, as the [file transfer contract](protos-ownsand-api-contract.md#file-transfer) requires.
- `snapshot`, plus snapshot export and template operations.

`create` requires `idleTtl` and `maxTtl` (Go: `IdleTTL` and `MaxTTL`) and the resources. The SDKs do not supply a default TTL.

### Data path

- For exec and file operations, the SDK calls `OpenSession` once per sandbox handle and tries a peer-to-peer WebRTC data channel to the agent.
- When the data channel does not open within 10 seconds, or fails during a session, the SDK uses the relay RPCs for that sandbox handle. A request that was in progress on the failed channel fails with a data path error; the SDK does not retry an exec automatically, because the command may have run.
- The caller can force a transport (`PEER_TO_PEER` or `RELAY`) for tests. The SDK reports the transport in use.
- Both transports carry the same `DataFrame` messages and give the same results.

## Storage

The SDKs persist nothing. They keep sessions and credentials in memory only.

## Security

- Credentials stay in memory. The SDKs never write them to disk or logs.
- The SDKs connect only to the configured API origin, the STUN service in the session offer and the agent's ICE candidates.
- The data channel accepts only the DTLS fingerprint in the agent's answer that `OpenSession` returned over the authenticated control plane connection.

## Logging

The SDKs log nothing by default. An optional logger receives RPC names, sandbox IDs, transport selection, durations and Connect codes, never credentials, exec content or file content.

## Build and Test

- TypeScript SDK: package scripts `build`, `lint`, `typecheck` and `test`, run through root Turborepo tasks. Record them with the implementation.
- Go SDK: `gofmt` check, `go vet ./...` and `go test ./...` from the repository root.
- Required tests: TTL validation before the request, transport fallback from peer-to-peer to relay, a failed exec stream that is not retried, chunked file writes, and credential redaction in errors.

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
