# protos-ownsand API contract

## Scope

This contract owns the Connect RPC schemas of [`ownsand`](project-ownsand.md):

- `protos/delino/ownsand/v1/` (`planned`): `.proto` files of the protobuf package `delino.ownsand.v1`.
- The generated Go bindings, committed next to the schemas (`planned`): message code in the Go package `github.com/delinoio/mono/protos/delino/ownsand/v1` (package name `ownsandv1`) and Connect handlers and clients in its `ownsandv1connect` subpackage.

The schemas are the single source of truth for the public API, the agent channel and the data path framing. The control plane, the device agent, the guest helper, the CLI, the web console and both SDKs consume them. The schemas and the generated code use Apache-2.0.

## Runtime and Language

- Language: Protocol Buffers (proto3), compiled for Connect RPC.
- Go bindings: committed to the repository in the root Go module `github.com/delinoio/mono`. Go consumers import them directly and do not run code generation.
- TypeScript bindings: generated for the [TypeScript SDK](packages-ownsand-sdk-foundation.md) and the [web console](apps-ownsand-console-foundation.md). The SDK contract owns where the generated TypeScript code lives.
- Code generation tool: open decision. Record the tool, its pinned version, the generation command and the CI drift check in this contract in the change that adds the first schema. The tool and the protobuf and Connect runtime libraries are new dependencies and need owner approval.

## Users and Operators

- Callers of the public API: organization members with Logto access tokens and programs with organization API keys, through the SDKs, the CLI and the web console.
- Device agents, through the agent channel.
- Guest helpers, through the guest channel inside each sandbox.

## Interfaces and Contracts

### General rules

- Package: `delino.ownsand.v1`. A breaking change needs a new package version (`v2`). Within `v1`, do not reuse or renumber fields, do not change field types, and reserve the numbers and names of removed fields.
- Identifiers that OwnSand or its callers generate are UUID v7 values in canonical text form in `string` fields named `<entity>_id`. User ID fields (`user_id`) hold the Logto `sub` unchanged as an opaque string and are not validated as UUIDs.
- Times use `google.protobuf.Timestamp`. Durations use `google.protobuf.Duration`.
- Every enum has a zero value `<ENUM>_UNSPECIFIED`. The server rejects `UNSPECIFIED` in a required enum field with `invalid_argument`. Optional enum fields, such as list filters and placement filters, use proto3 `optional`; an absent optional field means no filter.
- Sizes use `int64` fields with a unit suffix: `memory_mib`, `disk_gib`, `size_bytes`.
- List RPCs use `page_size` (default 50, maximum 200) and an opaque `page_token`.
- Errors use Connect error codes. Detail messages never contain secrets or sandbox content.
- Every public RPC that callers reach through the Railway edge is unary or server-streaming. See [the project transport rule](project-ownsand.md#protocol-and-api).

### Public services

Every public RPC except `OrganizationService.CreateOrganization`, `OrganizationService.ListOrganizations` and `MemberService.AcceptInvitation` takes an `organization_id`. The [control plane contract](servers-ownsand-api-foundation.md#authorization) owns the role that each RPC requires.

| Service               | RPC                        | Kind             | Purpose                                                                                      |
| --------------------- | -------------------------- | ---------------- | -------------------------------------------------------------------------------------------- |
| `OrganizationService` | `CreateOrganization`       | unary            | Create an organization. The caller becomes its `owner`.                                      |
|                       | `GetOrganization`          | unary            | Read one organization.                                                                       |
|                       | `ListOrganizations`        | unary            | List the organizations of the signed-in user.                                                |
|                       | `DeleteOrganization`       | unary            | Delete the organization and start its clean-up.                                              |
|                       | `TransferOwnership`        | unary            | Make another member the `owner`. The previous owner becomes `admin`.                         |
| `MemberService`       | `CreateInvitation`         | unary            | Create a one-time invitation with a role (`admin` or `member`). Returns the secret once.     |
|                       | `ListInvitations`          | unary            | List pending invitations without secrets.                                                    |
|                       | `RevokeInvitation`         | unary            | Revoke a pending invitation.                                                                 |
|                       | `AcceptInvitation`         | unary            | Join the organization with an invitation secret, as the signed-in user.                      |
|                       | `ListMembers`              | unary            | List members, roles and display names (empty when unavailable).                              |
|                       | `UpdateMemberRole`         | unary            | Change a member role between `admin` and `member`.                                           |
|                       | `RemoveMember`             | unary            | Remove a member. An `admin` or `member` may also remove itself; the `owner` cannot.          |
| `DeviceService`       | `CreateEnrollmentToken`    | unary            | Create a one-time enrollment token with an expiry. Returns the secret once.                  |
|                       | `ListEnrollmentTokens`     | unary            | List enrollment tokens without secrets.                                                      |
|                       | `RevokeEnrollmentToken`    | unary            | Revoke an unused enrollment token.                                                           |
|                       | `ListDevices`              | unary            | List devices with state, capabilities, limits and allocation.                                |
|                       | `GetDevice`                | unary            | Read one device.                                                                             |
|                       | `UpdateDeviceLimits`       | unary            | Set the upper limits of vCPU, memory and disk that sandboxes on the device may use together. |
|                       | `RemoveDevice`             | unary            | Revoke the device credentials and request local clean-up.                                    |
|                       | `GetAgentVersionPolicy`    | unary            | Read the agent version policy of the organization.                                           |
|                       | `UpdateAgentVersionPolicy` | unary            | Follow the default target, pin a version or hold updates.                                    |
| `ApiKeyService`       | `CreateApiKey`             | unary            | Create an organization API key. Returns the secret once.                                     |
|                       | `ListApiKeys`              | unary            | List API keys without secrets.                                                               |
|                       | `RevokeApiKey`             | unary            | Revoke an API key.                                                                           |
| `SandboxService`      | `CreateSandbox`            | unary            | Create a sandbox. See [the create request](#create-request).                                 |
|                       | `GetSandbox`               | unary            | Read one sandbox.                                                                            |
|                       | `ListSandboxes`            | unary            | List sandboxes, with optional device and state filters.                                      |
|                       | `DeleteSandbox`            | unary            | Delete a sandbox.                                                                            |
|                       | `PauseSandbox`             | unary            | Pause a running sandbox.                                                                     |
|                       | `ResumeSandbox`            | unary            | Resume a paused sandbox.                                                                     |
| `SnapshotService`     | `CreateSnapshot`           | unary            | Take a snapshot of a sandbox on its device.                                                  |
|                       | `GetSnapshot`              | unary            | Read one snapshot with its location and export state.                                        |
|                       | `ListSnapshots`            | unary            | List snapshots.                                                                              |
|                       | `DeleteSnapshot`           | unary            | Delete a snapshot. Fails with `failed_precondition` while a template references it.          |
|                       | `ExportSnapshot`           | unary            | Export a snapshot to R2 so that other devices with the same OS and architecture can use it.  |
|                       | `CreateTemplate`           | unary            | Make a named template from a snapshot.                                                       |
|                       | `ListTemplates`            | unary            | List templates.                                                                              |
|                       | `DeleteTemplate`           | unary            | Delete a template. The source snapshot stays.                                                |
| `DataPathService`     | `GetIceServers`            | unary            | Return the STUN server URLs that a caller uses to gather candidates before `OpenSession`.    |
|                       | `OpenSession`              | unary            | Signal a peer-to-peer data channel: send the caller's SDP offer, return the agent's answer.  |
|                       | `Exec`                     | server-streaming | Relay: run a command and stream stdout, stderr and the exit code.                            |
|                       | `AttachExec`               | server-streaming | Relay: continue the event stream of a running or finished exec from given output offsets.    |
|                       | `ReadFile`                 | server-streaming | Relay: stream the contents of a guest file from an offset.                                   |
|                       | `WriteFile`                | unary            | Relay: write one chunk of a guest file at an offset. See [file transfer](#file-transfer).    |

### Create request

`CreateSandboxRequest` fields:

- `organization_id`.
- `source`: exactly one of `base_image` (a `BaseImage` enum value), `snapshot_id` or `template_id`.
- `resources`: `vcpu_count`, `memory_mib` and `disk_gib`. All three are required and must be greater than zero. For a snapshot or template source, `disk_gib` must be at least the snapshot's disk size, and the server rejects a smaller disk with `invalid_argument`. Memory state restores only when `vcpu_count` and `memory_mib` equal the snapshot's values; other values are valid and boot the disk cold, according to the [control plane scheduler](servers-ownsand-api-foundation.md#scheduler).
- `idle_ttl` and `max_ttl`: required. Both must be greater than zero, `idle_ttl` must not be greater than `max_ttl`, and `max_ttl` must not be greater than 7 days. The server rejects a request that breaks any of these rules, including a missing TTL, with `invalid_argument`. Exactly 7 days is valid. There is no default TTL.
- `placement`: optional `device_id`, or optional `host_os` and `arch` filters. `guest_os` is derived from the source. With `device_id`, the scheduler does not select another device when that device cannot run the sandbox; the request fails with `failed_precondition`.
- `network`: `NETWORK_MODE_OUTBOUND` (the default when the field is absent) or `NETWORK_MODE_NONE`. The field is a message so that later policies can extend it without a new RPC.
- `labels`: optional string map (at most 32 entries; keys at most 63 bytes, values at most 255 bytes). Labels must not contain secrets; they appear in logs and in the console.

### Exec

- `ExecRequest`: `organization_id`, `sandbox_id`, `exec_id`, `command` (argument array, the first element is the program), `env` (string map), `working_directory` and optional `timeout`. A shell runs only when the caller passes one, for example `["/bin/sh", "-c", "..."]`.
- The caller generates the `exec_id` (a UUID v7) before it sends the request, so it can resume the exec even when the stream ends before the `started` event. While the agent keeps an exec (until 10 minutes after it ends) and across agent restarts within that time, it runs each `exec_id` at most once: a second `ExecRequest` with a known `exec_id` and the same `sandbox_id` and `command` attaches to the existing exec from offset 0 and does not run the command again. A known `exec_id` with a different `sandbox_id` or `command` fails with `already_exists`. Callers must not reuse an `exec_id`. The same binding applies to `read_id` (sandbox and path) and `write_id` (sandbox and target path).
- The response stream carries `ExecEvent` messages in order: one `started` event with the `exec_id`, then `stdout` and `stderr` chunks (at most 64 KiB each), then exactly one `exited` event with the exit code, or a `failed` event when the command could not start. Each output chunk carries its byte offset in its stream (stdout or stderr).
- The command runs on the agent independently of the stream. A stream may end before the command ends: the relay ends a stream itself before the Railway edge limit and sends a `keepalive` event at least every 60 seconds while the command produces no output. A stream that ends without `exited` or `failed` does not stop the command.
- `AttachExec` takes the `exec_id` and the next stdout and stderr offsets, and continues the same event sequence. The agent keeps the last 8 MiB of each output stream and the exit result for 10 minutes after the command ends. When the requested offset is older than the kept output, the first event is `output_truncated` with the first available offsets. When the exec is unknown, or its result is no longer kept, `AttachExec` fails with `not_found`; the outcome of the command is then unknown.
- On the peer-to-peer data channel, the same exec continues after a channel failure with an `AttachExec` request frame on a new session or on the relay.
- Stdin is not part of `v1`.

### File transfer

- `ReadFile` takes a caller-generated `read_id` (a UUID v7) and an optional start `offset`, and streams `FileChunk` messages of at most 1 MiB with the `offset` of each chunk and a final message with the file `size_bytes`. At the first call for a `read_id`, the guest helper makes a private snapshot of the file (a reflink copy where the guest file system supports it, otherwise a full copy) and serves every call for that `read_id` from the snapshot. A full copy is a point-in-time view only when no process writes the file during the copy: the guest helper compares the file identity, size and modification time before and after the copy and fails the call with `aborted` when they changed. A reflink copy is always a point-in-time view. After an interrupted stream, the caller calls `ReadFile` again with the same `read_id` from the next offset, so a resumed read never combines two versions of a file. The guest helper keeps the snapshot while any call for the `read_id` is open, and until 15 minutes after the last call for it ends, also after it sent the final message, so a caller that lost the last chunks or the final message can call again. A later call fails with `not_found`. While the guest helper prepares the snapshot or the next chunk, the stream carries a `keepalive` message at least every 60 seconds; a `keepalive` does not move the offset. The snapshot uses guest disk while the read is open.
- `WriteFile` is unary. Connect can carry client streams over HTTP/1.1, but browser clients cannot send them, and unary chunks let a caller retry or resume one chunk after an interrupted upload. The caller generates the `write_id` (a UUID v7) before the first call, so a retry after a lost response reaches the same write. Each call writes one chunk of at most 1 MiB at an `offset` to that `write_id`; writing the same chunk again is harmless. The last call sets `finish` with the total `size_bytes` and the file `mode`. The guest helper writes to a temporary file and renames it into place only after `finish`, so a failed upload never leaves a partial file at the target path. The guest helper deletes the temporary file of a `write_id` that receives no chunk for 15 minutes; a later call with that `write_id` fails with `not_found`. After a successful `finish`, the guest helper keeps the result for 15 minutes: a repeated `finish` for that `write_id` returns the same result and does not write the target again.
- Paths are absolute guest paths. The guest helper resolves them inside the guest; the device agent never resolves a guest path on the host file system.

### Data path framing

The peer-to-peer data channel and the relay carry the same `DataFrame` messages, so the agent and the guest helper handle both transports with one implementation.

- `DataFrame` has a `request_id`, the `sandbox_id` that the control plane authorized, a `kind` (`DataFrameKind`) and one payload: a session end (with a `SessionEndReason`), an exec request, an exec attach request (with the `exec_id` and the next stdout and stderr offsets), an exec event, a file read request, a file chunk, a file write chunk, a file write result or an error.
- On the WebRTC data channel, the frames form one byte stream: each frame is encoded as a varint length followed by the encoded `DataFrame`. The sender splits this byte stream into data channel messages of at most 16 KiB, which is below the 64 KiB default receive limit of [RFC 8841](https://www.rfc-editor.org/rfc/rfc8841.html#section-6.1). The receiver reassembles frames from the length prefixes. The data channel is ordered and reliable. One data channel serves one `OpenSession` session and may carry several concurrent requests, distinguished by `request_id`.
- The agent serves a frame only for the sandbox bound to its peer-to-peer session, or, on the relay, for the `sandbox_id` that the control plane authorized for that RPC, and rejects any other `sandbox_id`.
- The relay RPCs (`Exec`, `AttachExec`, `ReadFile`, `WriteFile`) translate their requests into `DataFrame` messages for the agent and translate the agent's frames back into responses.

### Agent channel

`AgentService` is the device-facing API. It is not part of the SDKs.

| RPC               | Kind       | Purpose                                                                                                                                              |
| ----------------- | ---------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Enroll`          | unary      | Exchange an enrollment token and the device public key for a `device_id`.                                                                            |
| `CreateChallenge` | unary      | Return a one-time nonce for the device.                                                                                                              |
| `Authenticate`    | unary      | Exchange a signed nonce for a short-lived `osd_` device session credential, or return the removal notice.                                            |
| `ConfirmRemoval`  | unary      | Confirm the local clean-up of a removed device with a signed nonce. Accepted only for removed devices.                                               |
| `Channel`         | long-lived | Carry `AgentEnvelope` messages in both directions. The [control plane contract](servers-ownsand-api-foundation.md#agent-channel) owns the transport. |

`AgentEnvelope` carries one of: heartbeat, capability report, state report (all sandboxes, snapshots and images on the device), command (create, delete, pause, resume, snapshot, export, import, clean up, update), command result, configuration (STUN server URLs and the device limits), sandbox update (a sandbox ID, its state, its end reason when it ended, and its idle deadline), acknowledgement of a state report or sandbox update, signaling message (SDP offer with the session ID, sandbox ID, principal and absolute expiry; SDP answer; ICE candidate), session revocation and relay `DataFrame`. Each sandbox entry in a state report, each sandbox update and each acknowledgement has the sandbox revision. Each sandbox entry in a state report has its state, its current idle deadline and, for an ended sandbox, its `SandboxEndReason`. The server acknowledges each state report, and the agent then drops the records of ended sandboxes that the report contained.

### Guest channel

`GuestService` runs inside the guest over the hypervisor socket that the [agent contract](cmds-ownsand-agent-foundation.md#guest-helper) defines. It serves `DataFrame` exec and file requests and a readiness check. It never reaches the control plane.

### Enums

| Enum                     | Values (without the enum prefix)                                                                                                        |
| ------------------------ | --------------------------------------------------------------------------------------------------------------------------------------- |
| `Role`                   | `OWNER`, `ADMIN`, `MEMBER`                                                                                                              |
| `HostOs`                 | `LINUX`, `MACOS`, `WINDOWS`                                                                                                             |
| `Arch`                   | `AMD64`, `ARM64`                                                                                                                        |
| `GuestOs`                | `LINUX`, `MACOS`, `WINDOWS`                                                                                                             |
| `BaseImage`              | `LINUX`, `MACOS`, `WINDOWS`                                                                                                             |
| `DeviceState`            | `ONLINE`, `OFFLINE`, `REMOVED`                                                                                                          |
| `SandboxState`           | `CREATING`, `RUNNING`, `PAUSED`, `DELETING`, `DELETED`, `FAILED`, `UNREACHABLE`                                                         |
| `SandboxEndReason`       | `DELETED_BY_CALLER`, `IDLE_TTL_EXPIRED`, `MAX_TTL_EXPIRED`, `DEVICE_REMOVED`, `ORGANIZATION_DELETED`, `CREATE_FAILED`, `LOST_ON_DEVICE` |
| `SnapshotState`          | `CREATING`, `READY`, `FAILED`, `DELETING`                                                                                               |
| `SnapshotExportState`    | `NOT_EXPORTED`, `EXPORTING`, `EXPORTED`, `EXPORT_FAILED`                                                                                |
| `NetworkMode`            | `OUTBOUND`, `NONE`                                                                                                                      |
| `AgentVersionPolicyMode` | `FOLLOW_DEFAULT`, `PIN`, `HOLD`                                                                                                         |
| `PrincipalKind`          | `USER`, `API_KEY`, `DEVICE`, `SYSTEM`                                                                                                   |
| `AuditTargetKind`        | `ORGANIZATION`, `MEMBERSHIP`, `INVITATION`, `DEVICE`, `ENROLLMENT_TOKEN`, `API_KEY`, `SANDBOX`, `AGENT_VERSION_POLICY`                  |
| `AuditResult`            | `SUCCEEDED`, `FAILED`                                                                                                                   |
| `EnrollmentTokenState`   | `PENDING`, `USED`, `REVOKED`, `EXPIRED`                                                                                                 |
| `InvitationState`        | `PENDING`, `ACCEPTED`, `REVOKED`, `EXPIRED`                                                                                             |
| `ApiKeyState`            | `ACTIVE`, `REVOKED`                                                                                                                     |
| `AgentUpdateState`       | `UP_TO_DATE`, `PENDING_IDLE`, `DOWNLOADING`, `INSTALLING`, `FAILED`, `REVOKED_PENDING_IDLE`                                             |
| `SessionEndReason`       | `IDLE`, `EXPIRED`, `REVOKED`, `AGENT_CHANNEL_LOST`                                                                                      |
| `DataPathTransport`      | `PEER_TO_PEER`, `RELAY`                                                                                                                 |
| `DataFrameKind`          | One value per payload in [data path framing](#data-path-framing)                                                                        |
| `AuditAction`            | The values in the [control plane audit table](servers-ownsand-api-foundation.md#audit-events)                                           |

A sandbox that is `UNREACHABLE` also reports its last known state in `last_known_state`. `DELETED` and `FAILED` sandboxes report a `SandboxEndReason`. `LOST_ON_DEVICE` means that the sandbox's VM or its record no longer exists on the device, for example after a host reboot.

A deleted snapshot has no terminal state: its record is removed. A snapshot whose local copy was removed with its device keeps its record only when it is exported; its location then names no device.

## Storage

The schemas define no storage. The [control plane contract](servers-ownsand-api-foundation.md#storage) and the [agent contract](cmds-ownsand-agent-foundation.md#storage) own persisted data.

## Security

- Secret fields (`enrollment_token`, `invitation_secret`, `api_key_secret`, `device_session_credential`) appear only in the response that issues them and in the request that uses them. Mark them with a field comment `Secret: never log.` Generated code that logs messages must redact them.
- `DataFrame` payloads, `ExecRequest.command`, `ExecRequest.env` and file chunks are sandbox content. Components never log them.

## Logging

The schemas define no logging. Components log RPC names, identifiers, enum values, sizes and durations, never secret fields or sandbox content.

## Build and Test

- Before the first schema, record the generation command and the lint and breaking-change checks in this section.
- CI must fail when the committed Go bindings differ from the generated output.
- Go consumers run `gofmt`, `go vet ./...` and `go test ./...` from the repository root according to `cmds/AGENTS.md` and `servers/AGENTS.md`.

## Dependencies and Integrations

- Upstream: the protobuf compiler and the Connect code generators (open decision, needs owner approval).
- Downstream: [control plane](servers-ownsand-api-foundation.md), [device agent and guest helper](cmds-ownsand-agent-foundation.md), [CLI](cmds-ownsand-cli-foundation.md), [web console](apps-ownsand-console-foundation.md) and [SDKs](packages-ownsand-sdk-foundation.md).

## Change Triggers

- Update the control plane, agent, CLI, console and SDK contracts when an RPC, message or enum that they use changes.
- Update [project-ownsand.md](project-ownsand.md) when the package version, the transport rule or the data path changes.
- Update `protos/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-ownsand.md](project-ownsand.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-dependency-security-contract.md](repository-dependency-security-contract.md)
- [Connect protocol](https://connectrpc.com/docs/protocol)
