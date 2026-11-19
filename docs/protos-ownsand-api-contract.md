# protos-ownsand API contract

## Scope

This contract owns the Connect RPC schemas of [`ownsand`](project-ownsand.md):

- `protos/delino/ownsand/v1/` (`planned`): `.proto` files of the protobuf package `delino.ownsand.v1`.
- The generated Go bindings, committed next to the schemas (`planned`): message code in the Go package `github.com/delinoio/mono/protos/delino/ownsand/v1` (package name `ownsandv1`) and Connect handlers and clients in its `ownsandv1connect` subpackage.

The schemas are the single source of truth for the public API, the agent channel and the data path framing. The control plane, the relay server, the device agent, the guest helper, the CLI, the web console and both SDKs consume them. The schemas and the generated code use Apache-2.0.

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
- Every public RPC that callers reach through the Railway edge is unary or server-streaming. See [the project transport rule](project-ownsand.md#protocol-and-api). `RelayService` on the relay server is also unary or server-streaming, so that clients without client or bidirectional streams can use it. Only the device-facing `RelayAgentService.Connect` is bidirectional, because the relay server terminates HTTP/2 itself.

### Public services

The control plane serves every public service except `RelayService`, which the [relay server](servers-ownsand-relay-foundation.md) serves at its own origin. Every public RPC except `OrganizationService.CreateOrganization`, `OrganizationService.ListOrganizations` and `MemberService.AcceptInvitation` takes an `organization_id`. The [control plane contract](servers-ownsand-api-foundation.md#authorization) owns the role that each RPC requires.

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
|                       | `ListSandboxes`            | unary            | List sandboxes, with optional device and state filters; ended sandboxes only on request.     |
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
|                       | `AuthorizeRelay`           | unary            | Grant relay access to one operation on a sandbox: return a `CALLER` relay ticket.            |
| `RelayService`        | `Exec`                     | server-streaming | Relay: run a command and stream stdout, stderr and the exit code.                            |
|                       | `AttachExec`               | server-streaming | Relay: continue the event stream of a running or finished exec from given output offsets.    |
|                       | `ReadFile`                 | server-streaming | Relay: stream the contents of a guest file from an offset.                                   |
|                       | `WriteFile`                | unary            | Relay: write one chunk of a guest file at an offset. See [file transfer](#file-transfer).    |

### Sandbox state rules

| RPC                                            | Required sandbox state                                                                                                                                                                                                                                                                                                                                                           | Otherwise                                                                                   |
| ---------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| `OpenSession`                                  | `RUNNING`                                                                                                                                                                                                                                                                                                                                                                        | `unavailable` for `UNREACHABLE`; `failed_precondition` for every other state                |
| `RelayService` `AttachExec`                    | `RUNNING` or `PAUSED` (on a paused sandbox, only the kept output and result of the exec are returned; for an exec that has not ended, the stream returns its kept output and then ends with a `paused` event, and the SDK watches the sandbox until it resumes, the caller cancels, or the sandbox ends; when the sandbox ends first, the exec ends with the sandbox end reason) | `failed_precondition` from the agent; `unavailable` from `AuthorizeRelay` for `UNREACHABLE` |
| `PauseSandbox`                                 | `RUNNING` (`PAUSED` succeeds without change)                                                                                                                                                                                                                                                                                                                                     | as above                                                                                    |
| `ResumeSandbox`                                | `PAUSED` (`RUNNING` succeeds without change)                                                                                                                                                                                                                                                                                                                                     | as above                                                                                    |
| `CreateSnapshot`                               | `RUNNING` or `PAUSED`                                                                                                                                                                                                                                                                                                                                                            | as above                                                                                    |
| `RelayService` `Exec`, `ReadFile`, `WriteFile` | `RUNNING`, checked by the agent before it dispatches the request; the relay server has no state and forwards the agent's `failed_precondition`                                                                                                                                                                                                                                   | `failed_precondition` from the agent; `unavailable` from `AuthorizeRelay` for `UNREACHABLE` |
| `AuthorizeRelay`                               | `EXEC` and `ATTACH_EXEC`: `RUNNING` or `PAUSED`; `READ_FILE` and `WRITE_FILE`: `RUNNING`. `UNREACHABLE` gives `unavailable` here, before any relay RPC                                                                                                                                                                                                                           | `unavailable` for `UNREACHABLE`; `failed_precondition` for every other state                |
| `DeleteSandbox`                                | any state except `DELETED` and `FAILED` (those succeed without change)                                                                                                                                                                                                                                                                                                           | —                                                                                           |

`CreateSandbox` returns the sandbox in `CREATING` without waiting for the agent; the caller watches `GetSandbox` until a stable state: `RUNNING`, `PAUSED` (another caller paused it), or the terminal `FAILED` or `DELETED` with its `SandboxEndReason` (for example, a TTL that ended or a removed device). `CreateSnapshot` and `ExportSnapshot` also return at once with a snapshot in `CREATING` or `EXPORTING`. `PauseSandbox`, `ResumeSandbox` and `DeleteSandbox` wait up to 60 seconds for the agent result and then return the current state, which the caller can watch with `GetSandbox`.

### Create request

`CreateSandboxRequest` fields:

- `organization_id`.
- `idempotency_key`: a UUID v7 that the caller generates for each create. The server stores it with the reservation for 24 hours: a retry with the same `idempotency_key` and the same parameters returns the original sandbox, and a retry with different parameters fails with `already_exists`. A caller whose create response was lost retries with the same `idempotency_key`.
- `source`: exactly one of `base_image` (a `BaseImage` enum value), `snapshot_id` or `template_id`.
- `resources`: `vcpu_count`, `memory_mib` and `disk_gib`. All three are required and must be greater than zero. For a snapshot or template source, `disk_gib` must be at least the snapshot's disk size, and the server rejects a smaller disk with `invalid_argument`. Memory state restores only when `vcpu_count` and `memory_mib` equal the snapshot's values; other values are valid and boot the disk cold, according to the [control plane scheduler](servers-ownsand-api-foundation.md#scheduler).
- `idle_ttl` and `max_ttl`: required. Both must be greater than zero, `idle_ttl` must not be greater than `max_ttl`, and `max_ttl` must not be greater than 7 days. The server rejects a request that breaks any of these rules, including a missing TTL, with `invalid_argument`. Exactly 7 days is valid. There is no default TTL.
- `placement`: optional `device_id`, or optional `host_os` and `arch` filters. `guest_os` is derived from the source. With `device_id`, the scheduler does not select another device: the request fails with `unavailable` or `failed_precondition` according to the error-code rule of the [control plane scheduler](servers-ownsand-api-foundation.md#scheduler).
- `network`: `NETWORK_MODE_OUTBOUND` (the default when the field is absent) or `NETWORK_MODE_NONE`. A present `network` message with an unspecified mode fails with `invalid_argument`. The field is a message so that later policies can extend it without a new RPC.
- `labels`: optional string map (at most 32 entries; keys at most 63 bytes, values at most 255 bytes). Labels must not contain secrets; every member and API key of the organization can read them, and the console shows them. Components never log labels.

### Exec

- `ExecRequest`: `organization_id`, `sandbox_id`, `exec_id`, `command` (argument array, the first element is the program), `env` (string map), `working_directory`, optional `timeout` and `binding_salt` (see [relay tickets](#relay-tickets)), which is part of the idempotency comparison below. When the timeout ends, the guest helper stops the command (on Linux and macOS guests it sends `SIGTERM` and, 10 seconds later, `SIGKILL`; on Windows guests it terminates the process tree) and the `exited` event carries `timed_out` set to true with the exit code or signal. The encoded `command`, `env` and `working_directory` together must not exceed 256 KiB; the server and the SDKs reject a larger request with `invalid_argument` before they frame it, and the agent checks the decoded fields again before it dispatches an exec from either transport. A shell runs only when the caller passes one, for example `["/bin/sh", "-c", "..."]`.
- The caller generates the `exec_id` (a UUID v7) before it sends the request, so it can resume the exec even when the stream ends before the `started` event. The agent applies the same timestamp window and the 30-minute memory of ended IDs to `exec_id` as to `read_id` and `write_id` (see expired operation IDs below), so a delayed duplicate request never runs a command again. While the agent keeps an exec (until 10 minutes after it ends) and across agent restarts within that time, it runs each `exec_id` at most once: a second `ExecRequest` with a known `exec_id` and the same `sandbox_id`, `command`, `env`, `working_directory`, `timeout` and `binding_salt` attaches to the existing exec from offset 0 and does not run the command again. A known `exec_id` with any other value in these fields fails with `already_exists`. Callers must not reuse an `exec_id`. The same binding applies to `read_id` (sandbox and path) and `write_id` (sandbox and target path).
- The response stream carries `ExecEvent` messages in order: one `started` event with the `exec_id`, then `stdout` and `stderr` chunks (at most 64 KiB each), then exactly one `exited` event with the exit code (an `int64`, so that 32-bit Windows codes keep their value) or, when a signal ended the command, the signal number, or a `failed` event when the command could not start, or a `sandbox_ended` event with the `SandboxEndReason` when the sandbox ended (for example, while it was paused) before the exec result could be delivered. Each output chunk carries its byte offset in its stream (stdout or stderr).
- The command runs on the agent independently of the stream. A stream may end before the command ends: the relay server ends a stream itself before it has run for 14 minutes, with an OK status and no final event (a stream rotation), and sends a `keepalive` event at least every 60 seconds while the command produces no output. A stream that ends without `exited` or `failed` does not stop the command.
- `AttachExec` takes the `exec_id` and the next stdout and stderr offsets, and continues the same event sequence. The agent keeps the last 8 MiB of each output stream and the exit result for 10 minutes after the command ends. When the requested offset is older than the kept output, the first event is `output_truncated` with the first available offsets. When the exec is unknown, or its result is no longer kept, `AttachExec` fails with `not_found`; the outcome of the command is then unknown.
- On the peer-to-peer data channel, the same exec continues after a channel failure with an `AttachExec` request frame on a new session or on the relay.
- Stdin is not part of `v1`.

### File transfer

- `ReadFile` takes a `read_id` (a UUID v7 that the caller generates right before the first call), the absolute guest `path`, the `binding_salt` and an optional start `offset`, and streams `FileChunk` messages of at most 1 MiB with the `offset` of each chunk and a final message with the file `size_bytes`. At the first call for a `read_id`, the guest helper makes a private snapshot of the file (a reflink copy where the guest file system supports it, otherwise a full copy) and serves every call for that `read_id` from the snapshot. A full copy is a point-in-time view only when no process writes the file during the copy. The guest helper compares the file identity, size and modification time before and after the copy and fails the call with `aborted` when they changed, but a writer that keeps these values unchanged is not detected; for a full copy, the guarantee is only that every retry of the `read_id` returns the same bytes. A reflink copy is always a point-in-time view. After an interrupted stream, the caller calls `ReadFile` again with the same `read_id` from the next offset, so a resumed read never combines two versions of a file. The guest helper keeps the snapshot while any call for the `read_id` is open, and until 15 minutes after the last call for it ends, also after it sent the final message, so a caller that lost the last chunks or the final message can call again. A later call fails with `not_found`. While the guest helper prepares the snapshot or the next chunk, the stream carries a `keepalive` message at least every 60 seconds; a `keepalive` does not move the offset. The snapshot uses guest disk while the read is open.
- `WriteFile` is unary. The relay server requires a request body to finish uploading within 5 minutes; the SDKs retry a chunk whose upload times out, which the `write_id` rules make safe. Connect can carry client streams over HTTP/1.1, but browser clients cannot send them, and unary chunks let a caller retry or resume one chunk after an interrupted upload. The caller generates the `write_id` (a UUID v7) right before the first call, so a retry after a lost response reaches the same write. Each call writes one chunk of at most 1 MiB at an `offset` to that `write_id`. The guest helper records the written ranges: writing the same bytes again is harmless, a chunk that overlaps written bytes with different content fails with `failed_precondition`, and `finish` fails with `failed_precondition` unless the written ranges cover exactly `[0, size_bytes)`. The last call sets `finish` with the total `size_bytes` and the file `mode`. The guest helper writes to a temporary file and renames it into place only after `finish`, so a failed upload never leaves a partial file at the target path. The guest helper deletes the temporary file of a `write_id` that receives no chunk for 15 minutes; a later call with that `write_id` fails with `not_found`. After a successful `finish`, the guest helper keeps the result for 15 minutes together with the `finish` fields (`size_bytes`, `mode`, and the offset and digest of the final chunk): a repeated `finish` for that `write_id` with the same fields returns the same result and does not write the target again, and one with different fields fails with `failed_precondition`.
- Expired operation IDs: the agent checks every new `read_id` and `write_id` when its first frame arrives, against its own host clock, which must be synchronized with NTP. It accepts an unknown ID only when the timestamp in the UUID v7 is less than 15 minutes old and not more than 5 minutes in the future. An unknown ID that is older fails with `not_found`, like a remembered expired ID, and the error message says that the operation expired or that the caller's clock may be behind; only an ID more than 5 minutes in the future fails with `failed_precondition` and a clock-skew message. The agent remembers the ID of each ended or expired operation for 30 minutes of its own monotonic time. After a memory-state restore and after a resume, the agent sets the guest clock from the host clock through the guest helper. A retry of an expired operation, also at offset zero, therefore fails with `not_found` and never starts a new snapshot or temporary file.
- Paths are absolute guest paths. The guest helper resolves them inside the guest; the device agent never resolves a guest path on the host file system.

### Data path framing

The peer-to-peer data channel and the relay carry the same `DataFrame` messages, so the agent and the guest helper handle both transports with one implementation.

- `DataFrame` has a `stream_id` (an unsigned 64-bit number; on a peer-to-peer session the caller chooses it, unique within the session, and on the relay the relay server allocates it, unique among all open relay RPCs on the agent's relay connection, so it can route every agent frame to its RPC; `stream_id` 0 with an empty `sandbox_id` is reserved for connection-level frames on the relay connection), the `sandbox_id` that the control plane authorized, a `kind` (`DataFrameKind`) and one payload: a relay stream open (with the relay ticket ID and the relay server's first credit window for the agent's frames toward the caller; relay only), a relay stream close (relay only), a relay window update (with the number of bytes that it adds to the sender's window for that `stream_id`; relay only), a keepalive (relay connection only, on `stream_id` 0), a session end (with a `SessionEndReason`), an exec request, an exec attach request (with the `exec_id` and the next stdout and stderr offsets), an exec event, a file read request, a file chunk, a file write chunk, a file write result or an error.
- On the WebRTC data channel, the frames form one byte stream: each frame is encoded as a varint length followed by the encoded `DataFrame`. The sender splits this byte stream into data channel messages of at most 16 KiB, which is below the 64 KiB default receive limit of [RFC 8841](https://www.rfc-editor.org/rfc/rfc8841.html#section-6.1). The receiver reassembles frames from the length prefixes. An encoded `DataFrame` is at most 1 MiB plus 64 KiB, which holds the largest file chunk and its envelope; the receiver checks the declared length before it buffers the frame and closes the data channel when the length is larger. The data channel is ordered and reliable. One data channel serves one `OpenSession` session and may carry several concurrent requests, distinguished by `stream_id`.
- On the relay connection (`RelayAgentService.Connect`), each Connect stream message carries one `DataFrame`, with the same 1 MiB plus 64 KiB size limit. The relay server opens each relay RPC with a relay stream open frame for a new `stream_id`; every later frame of that `stream_id` belongs to the ticket ID of that stream open frame. Each relay RPC carries one operation request, in the first request frame after the stream open. The relay server allocates `stream_id` values in increasing order from 1 on each relay connection and never reuses one on that connection, so a late frame of a closed stream cannot reach another RPC; both sides drop frames of a closed `stream_id`.
- When a relay RPC ends on the caller side (the caller cancels it, the relay server ends it at its stream limit, or the caller connection fails), the relay server sends a relay stream close frame for its `stream_id`. The agent then stops sending frames for that stream and releases its output subscription. A relay stream close does not stop an exec and does not end a read snapshot or a write: they keep the lifetime of the [exec](#exec) and [file transfer](#file-transfer) rules, so the caller can continue them with `AttachExec`, `ReadFile` or `WriteFile`.
- When the agent rejects a relay stream because the ticket ID names no live grant (unknown, expired before the first request frame arrived, or revoked, for example after the agent channel was lost), it answers with a session-end frame with `GRANT_INVALID` for that `stream_id`, and the relay server ends the RPC with `unavailable` and that reason in the error detail. The agent rejects the first request frame of a stream with an error frame (`permission_denied`) when its kind, operation ID or request digest does not match the grant.
- Flow control: credit windows apply in both directions. A window counts the bytes of encoded `DataFrame` messages. Every window and per-stream buffer is at least one maximum frame (1 MiB plus 64 KiB), so a maximum file chunk can always be sent. Only payload frames use credit (exec requests, exec attach requests, file read requests, exec events, file chunks, file write chunks and file write results); stream open, stream close, window update, keepalive, session-end and error frames do not. The agent sends the first window for its direction when it accepts a stream open frame. The agent sends payload frames of a relay stream toward the caller only within a credit window that the relay server grants for that stream. The relay server sends request payload frames (exec requests, exec attach requests, file read requests and file write chunks) toward the agent only within a credit window that the agent grants for that stream with relay window updates, and the agent buffers at most that window for each stream, so it never stops reading the shared connection because one guest is slow. The relay server opens each stream with a window equal to its per-stream buffer, carried in the relay stream open frame, and sends a relay window update as the caller consumes data. A slow caller therefore stops only its own stream, and the relay server never needs to stop reading the shared connection or drop frames.
- When a relay connection closes or another connection replaces it, the agent ends every stream of that connection as if it received a relay stream close frame for each, and keeps the exec and file operation state under the same rules.
- The agent serves a frame only for the sandbox bound to its peer-to-peer session, or, on the relay, for the `sandbox_id` of the live relay grant whose ticket ID the relay stream open frame names, and rejects any other `sandbox_id`, an unknown or expired ticket ID and a revoked grant. Expiry applies to the first request frame of a stream only: a grant that expires does not end streams whose first request the agent already accepted; it only stops new ones. The agent ends the streams that are still open at the grant's stream deadline, according to the [control plane contract](servers-ownsand-api-foundation.md#data-path-signaling-and-relay).
- The `RelayService` RPCs (`Exec`, `AttachExec`, `ReadFile`, `WriteFile`) translate their requests into `DataFrame` messages for the agent and translate the agent's frames back into responses.

### Relay services

| Service             | RPC       | Kind          | Caller       | Purpose                                                                                                               |
| ------------------- | --------- | ------------- | ------------ | --------------------------------------------------------------------------------------------------------------------- |
| `RelayService`      | see above | see above     | SDK, CLI     | Relay exec and file requests to the agent of the ticket's device. Authenticated with a `CALLER` relay ticket.         |
| `RelayAgentService` | `Connect` | bidirectional | Device agent | Carry `DataFrame` messages between the relay server and one device agent. Authenticated with an `AGENT` relay ticket. |

The relay server serves both services at its origin. `RelayService` requests keep their `organization_id` and `sandbox_id` fields, and the relay server checks them against the ticket. The [relay server contract](servers-ownsand-relay-foundation.md) owns timeouts, keepalives and limits.

### Relay tickets

- A relay ticket is the text `osr_`, the unpadded base64url encoding of a `RelayTicketClaims` message, a period, and the unpadded base64url encoding of the Ed25519 signature over the encoded `RelayTicketClaims` bytes. It travels in the `Authorization: Bearer` header to the relay server, never to the control plane.
- `RelayTicketClaims` fields: `key_id`, `kind` (`RelayTicketKind`), `ticket_id` (a UUID v7), `relay_origin`, `organization_id`, `device_id`, `sandbox_id`, `operation_kind` (`RelayOperationKind`), `operation_id` and `request_digest` (`CALLER` only), `principal_kind` and `principal_id` (`CALLER` only), `issued_at` and `expires_at`.
- `request_digest` is the SHA-256 of a `RelayRequestBinding` message that the caller builds from the request it will send and the agent builds again from the first request frame. The message holds `binding_salt`, `sandbox_id`, `operation_kind`, `operation_id` and, for `EXEC`, `command`, `env` (as a repeated key and value entry list sorted by key bytes, so that map order does not change the digest), `working_directory` and `timeout`; for `READ_FILE` and `WRITE_FILE`, the absolute guest `path`. An `ATTACH_EXEC` grant has no request digest: `AttachExec` cannot start a command, so a caller that knows only the `exec_id` (for example, another program, or a program that restarted) can attach to an exec of the sandbox on either transport. The digest input is the protobuf binary encoding of that message with fields in field-number order and no unknown fields. A required test checks that the Go and TypeScript encodings of the same binding give the same digest.
- `binding_salt` is 32 random bytes that the caller generates for each operation and sends in the operation's request (`ExecRequest`, every `ReadFile` request and the first `WriteFile` chunk), never to the control plane. The salt keeps the control plane from confirming a guessed command, environment value or path from the digest.
- The agent records the digest of each exec, read and write when it first accepts the operation, on either transport (also for an exec that started on a peer-to-peer session). `AttachExec` and later `WriteFile` chunks do not carry the bound fields: under an `EXEC` or `WRITE_FILE` grant they are checked by comparing the grant digest with the recorded digest of the operation that the `exec_id` or `write_id` names. Under an `ATTACH_EXEC` grant, which has no digest, the agent checks only the sandbox, the `exec_id` and that the request is `AttachExec`. Every `ReadFile` request carries the path and the salt, so the agent builds the digest from the request and also requires it to equal the recorded digest of the `read_id` when one exists. A retry of the identical `ExecRequest` builds the same digest. A caller whose first `ReadFile` request was lost before the agent received it therefore resends the same request; when the agent already made the snapshot for that `read_id` and the digest matches, it serves the existing snapshot. When the agent no longer keeps an exec or write operation, the continuation fails with `not_found`, as without a grant check; a digest mismatch fails with `permission_denied`. The SDK keeps the original request fields and salt of each open operation, so it can call `AuthorizeRelay` for a continuation.
- `AuthorizeRelay` takes an `authorization_id` and an optional `replaces_ticket_id` according to the [control plane replacement rule](servers-ownsand-api-foundation.md#data-path-signaling-and-relay).
- `AuthorizeRelay` returns the `CALLER` ticket, the `relay_origin` and the `expires_at`. The relay grant on the agent channel carries the same `ticket_id`, `sandbox_id`, operation kind, ID and request digest, principal and `expires_at`, and the `AGENT` ticket for the same relay origin.

### Agent channel

`AgentService` is the device-facing API. It is not part of the SDKs.

| RPC               | Kind       | Purpose                                                                                                                                              |
| ----------------- | ---------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Enroll`          | unary      | Exchange an enrollment token and the device public key for a `device_id`.                                                                            |
| `CreateChallenge` | unary      | Return a one-time nonce for the device.                                                                                                              |
| `Authenticate`    | unary      | Exchange a signed nonce for a short-lived `osd_` device session credential, or return the removal notice.                                            |
| `ConfirmRemoval`  | unary      | Confirm the local clean-up of a removed device with a signed nonce. Accepted only for removed devices.                                               |
| `Channel`         | long-lived | Carry `AgentEnvelope` messages in both directions. The [control plane contract](servers-ownsand-api-foundation.md#agent-channel) owns the transport. |

`AgentEnvelope` carries one of: heartbeat, capability report, state report (all sandboxes, snapshots and images on the device), command (create, delete, pause, resume, snapshot, export, import, clean up, update), command result, configuration (STUN server URLs and the device limits), sandbox update (a sandbox ID, its state, its end reason when it ended, and its idle deadline), heartbeat acknowledgement (with the heartbeat sequence number), acknowledgement of a state report or sandbox update, signaling message (SDP offer with the session ID, sandbox ID, principal and absolute expiry; SDP answer), session revocation (with a revocation ID and its target: the session ID or the relay ticket ID that it ends), revocation acknowledgement, signed URL request and signed URL grant (snapshot ID and part numbers; the agent asks for fresh part URLs before the old ones expire, so an export or import may run longer than 15 minutes), relay grant (ticket ID, the optional `replaces_ticket_id`, sandbox ID, operation kind, ID and request digest, principal, expiry, stream deadline, relay origin and the `AGENT` relay ticket), relay grant acknowledgement (with the result: accepted, or rejected with a reason such as the per-device grant limit) and relay grant release (ticket ID). The agent channel carries no `DataFrame`; relay data uses `RelayAgentService` on the relay server. Signaling is non-trickle: both peers finish ICE candidate gathering before they send the offer or the answer, which carry all candidates. Each sandbox entry in a state report, each sandbox update and each acknowledgement has the sandbox revision. Each sandbox entry in a state report has its state, its current idle deadline and, for an ended sandbox, its `SandboxEndReason`. The server acknowledges each state report, and the agent then drops the records of ended sandboxes that the report contained.

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
| `EnrollmentTokenState`   | `PENDING`, `USED`, `REVOKED`, `EXPIRED`                                                                                                 |
| `InvitationState`        | `PENDING`, `ACCEPTED`, `REVOKED`, `EXPIRED`                                                                                             |
| `ApiKeyState`            | `ACTIVE`, `REVOKED`                                                                                                                     |
| `AgentUpdateState`       | `UP_TO_DATE`, `PENDING_IDLE`, `DOWNLOADING`, `INSTALLING`, `FAILED`, `REVOKED_PENDING_IDLE`                                             |
| `SessionEndReason`       | `IDLE`, `EXPIRED`, `REVOKED`, `AGENT_CHANNEL_LOST`, `GRANT_INVALID` and `GRANT_REPLACED` (relay only)                                   |
| `DataPathTransport`      | `PEER_TO_PEER`, `RELAY`                                                                                                                 |
| `RelayOperationKind`     | `EXEC` (`Exec` and `AttachExec`), `ATTACH_EXEC` (`AttachExec` only, without a digest), `READ_FILE`, `WRITE_FILE`                        |
| `RelayTicketKind`        | `CALLER`, `AGENT`                                                                                                                       |
| `DataFrameKind`          | One value per payload in [data path framing](#data-path-framing)                                                                        |
| `AuditAction`            | The values in the [control plane audit table](servers-ownsand-api-foundation.md#audit-events)                                           |

A sandbox that is `UNREACHABLE` also reports its last known state in `last_known_state`. `DELETED` and `FAILED` sandboxes report a `SandboxEndReason`. `LOST_ON_DEVICE` means that the sandbox's VM or its record no longer exists on the device, for example after a host reboot.

A deleted snapshot has no terminal state: its record is removed. A snapshot whose local copy was removed with its device keeps its record only when it is exported; its location then names no device.

## Storage

The schemas define no storage. The [control plane contract](servers-ownsand-api-foundation.md#storage) and the [agent contract](cmds-ownsand-agent-foundation.md#storage) own persisted data.

## Security

- Signed URL fields (in the signed URL grant) and SDP offer and answer bodies are also never logged, and generated code that logs messages redacts them. Secret fields (`enrollment_token`, `invitation_secret`, `api_key_secret`, `device_session_credential`, `relay_ticket`) appear only in the response that issues them and in the request that uses them. Mark them with a field comment `Secret: never log.` Generated code that logs messages must redact them.
- `DataFrame` payloads, `ExecRequest.command`, `ExecRequest.env` and file chunks are sandbox content. Components never log them.

## Logging

The schemas define no logging. Components log RPC names, identifiers, enum values, sizes and durations, never secret fields or sandbox content.

## Build and Test

- Before the first schema, record the generation command and the lint and breaking-change checks in this section.
- CI must fail when the committed Go bindings differ from the generated output.
- Go consumers run `gofmt`, `go vet ./...` and `go test ./...` from the repository root according to `cmds/AGENTS.md` and `servers/AGENTS.md`.

## Dependencies and Integrations

- Upstream: the protobuf compiler and the Connect code generators (open decision, needs owner approval).
- Downstream: [control plane](servers-ownsand-api-foundation.md), [relay server](servers-ownsand-relay-foundation.md), [device agent and guest helper](cmds-ownsand-agent-foundation.md), [CLI](cmds-ownsand-cli-foundation.md), [web console](apps-ownsand-console-foundation.md) and [SDKs](packages-ownsand-sdk-foundation.md).

## Change Triggers

- Update the control plane, relay server, agent, CLI, console and SDK contracts when an RPC, message or enum that they use changes.
- Update [project-ownsand.md](project-ownsand.md) when the package version, the transport rule or the data path changes.
- Update `protos/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-ownsand.md](project-ownsand.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-dependency-security-contract.md](repository-dependency-security-contract.md)
- [Connect protocol](https://connectrpc.com/docs/protocol)
