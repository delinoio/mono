# servers-ownsand-api foundation contract

## Scope

This contract owns the `ownsand-api` control plane of [`ownsand`](project-ownsand.md) at `servers/ownsand-api/` (`planned`). The control plane is the system of record for organizations, memberships, devices, credentials, sandbox records, snapshot records, templates, agent version policies and audit events. It schedules sandboxes, sends commands to device agents, signals peer-to-peer data channels and relays data when peer-to-peer fails.

The control plane does not run sandboxes and does not store sandbox content. It is licensed under `FSL-1.1-ALv2` according to the [license contract](repository-license-contract.md).

Planned layout, following the `accounts-api` server conventions:

- `servers/ownsand-api/cmd/ownsand-api/`: process entry point.
- `servers/ownsand-api/internal/`: configuration, HTTP and Connect handlers, authentication, authorization, scheduler, agent channel, relay, storage and audit packages.
- `servers/ownsand-api/package.json`, `turbo.json`, `.env.example`, `Dockerfile` and `railway.json`: workspace wiring, the `deploy` field and the Railway runtime.

## Runtime and Language

- Language: Go in the root module `github.com/delinoio/mono`. Import path: `github.com/delinoio/mono/servers/ownsand-api/...`.
- Runtime: one long-running HTTP server process with Connect handlers. Logging: `log/slog`.
- Deployment target: the Railway service `ownsand-api`, deployed by the [deployment workflow](repository-workflow-contract.md#deployment-workflow) like `accounts-api`. Production origin: `https://ownsand.api.delino.io`. The Railway edge terminates TLS.
- The Railway edge forwards HTTP/1.1 to the container. Public RPCs are unary or server-streaming according to the [project transport rule](project-ownsand.md#protocol-and-api). Long-lived server streams (exec and file relay) lift the write deadline of their own handler with `http.ResponseController`.
- The Railway edge also limits each HTTP request: it closes a request that transfers no data for 5 minutes, and a request that runs for more than 15 minutes. WebSocket connections are exempt. A relay stream therefore sends a keepalive event at least every 60 seconds and ends itself before 14 minutes; the caller continues with the resume rules of the [protos contract](protos-ownsand-api-contract.md#exec).
- Database: PlanetScale Postgres, according to the repository defaults.
- Development port (`planned`): `127.0.0.1:8300`. Add it to the [environment contract](repository-environment-contract.md) in the change that adds the development launcher.

## Users and Operators

- Organization owners, admins and members, through the [web console](apps-ownsand-console-foundation.md), the [CLI](cmds-ownsand-cli-foundation.md) and the [SDKs](packages-ownsand-sdk-foundation.md).
- Programs that use an organization API key.
- [Device agents](cmds-ownsand-agent-foundation.md), through the agent channel.
- Operators who deploy and monitor the service.

## Interfaces and Contracts

- Business APIs: the public services and `AgentService` of the [protos contract](protos-ownsand-api-contract.md).
- Operational endpoint: `GET /healthz` without authentication returns `200` with `{"status":"ok"}` while the process can serve HTTP requests.
- Browser access: CORS allows only `https://ownsand.delino.io` in production and the console's loopback development origin locally.

### Authentication

The server accepts three kinds of credentials in the `Authorization: Bearer` header. It selects the validator from the credential shape and never tries a second validator after the first one rejects the credential.

| Principal kind | Credential                                                        | Validation                                                                                                                                                                                                 |
| -------------- | ----------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `user`         | Logto access token (JWT)                                          | Signature, issuer, expiry, audience `https://ownsand.api.delino.io` and required scopes, according to the [authentication contract](repository-auth-contract.md#token-validation). ID tokens are rejected. |
| `api_key`      | `osk_` followed by the secret                                     | SHA-256 hash lookup with a constant-time comparison. The key must not be revoked, and its organization must not be deleted.                                                                                |
| `device`       | `osd_` device session credential from `AgentService.Authenticate` | SHA-256 hash lookup with a constant-time comparison. Accepted only to open the agent channel of its own device. The credential must not be expired, and the device must not be removed.                    |

- The Logto API resource indicator is `https://ownsand.api.delino.io`. Record the scope names and the Logto application registrations in this contract in the change that adds token validation.
- The API key exception is recorded in the [project index](project-ownsand.md#identity-and-access). API keys are not accepted on `OrganizationService`, `MemberService`, `ApiKeyService` or the admin RPCs of `DeviceService`.
- Secret formats: API keys `osk_`, enrollment tokens `ose_`, invitation secrets `osi_` and device session credentials `osd_`, each followed by 32 random bytes from a cryptographically secure source in unpadded base64url. The prefixes let secret scanners and the credential selector recognize them.

### Authorization

The server authorizes every RPC that takes an `organization_id` against the caller's membership in that organization and loads every target entity with that organization in the query. A request for an entity of another organization returns `not_found`, not `permission_denied`, so that callers cannot discover entities of other organizations.

Three RPCs take no `organization_id`. They accept only a Logto access token (`user` principal) and need no membership:

- `CreateOrganization` creates the organization and the caller's `owner` membership in one transaction.
- `ListOrganizations` returns only the organizations in which the caller has a membership.
- `AcceptInvitation` takes the organization from the validated invitation, not from the request. An expired, revoked or used invitation fails with `unauthenticated` and the same message.

| Action                                                                          | `owner` | `admin` | `member` | API key |
| ------------------------------------------------------------------------------- | ------- | ------- | -------- | ------- |
| Delete the organization, transfer ownership                                     | yes     | no      | no       | no      |
| Invitations, member roles, removal of another member                            | yes     | yes     | no       | no      |
| Remove the caller's own membership (leave the organization)                     | no      | yes     | yes      | no      |
| Enrollment tokens, device removal, device limits, agent version policy          | yes     | yes     | no       | no      |
| API keys                                                                        | yes     | yes     | no       | no      |
| Read the organization, list members                                             | yes     | yes     | yes      | no      |
| List and read devices and the agent version policy                              | yes     | yes     | yes      | yes     |
| Sandboxes, exec, files, pause, resume, snapshots, templates, data path sessions | yes     | yes     | yes      | yes     |

- An organization has exactly one `owner`. An `admin` cannot change the role of the `owner` or remove the `owner`, and cannot grant `owner`. Only `TransferOwnership` makes another existing member the `owner`.
- Sandboxes, snapshots and templates belong to the organization, not to the member or API key that created them. Every member and API key of the organization can use them. The record keeps the creating principal for audit.
- Invitations are one-time secrets that expire after 7 days. `AcceptInvitation` creates the membership for the signed-in Logto user. OwnSand does not look up users by email address.

### Agent channel

- Enrollment: an admin creates an enrollment token with an expiry of at most 24 hours. The agent generates an Ed25519 key pair locally and calls `AgentService.Enroll` with the token and the public key. The server consumes the token in the same transaction that creates the device, so a token enrolls at most one device. An expired, revoked or used token fails with `unauthenticated` and the same message, so that the response does not show which condition failed.
- Session: the agent calls `CreateChallenge`, signs the nonce with its private key and calls `Authenticate`. The nonce is single-use and expires after 60 seconds. The device session credential is needed only to open the channel and expires after 15 minutes. An open channel stays valid until it closes; the server checks the device state at each heartbeat and closes the channel when the device is removed. Before each new connection, the agent gets a new credential with `CreateChallenge` and `Authenticate`.
- Transport: open decision. The Railway edge forwards HTTP/1.1, so a Connect bidirectional stream cannot reach the container through `https://ownsand.api.delino.io`. The candidates are a WebSocket upgrade on the public HTTPS origin that carries `AgentEnvelope` messages, and a Connect bidirectional stream through the Railway TCP proxy with TLS terminated in the process. Record the decision, its endpoint and its timeouts here before the agent channel implementation.
- Heartbeat: the agent sends a heartbeat every 15 seconds. The server marks the device `offline` when the channel closes or when no heartbeat arrives for 45 seconds. At that point, the sandboxes of the device change to `unreachable` with their `last_known_state`.
- Reconnection: after `Authenticate`, the agent sends a full state report. The report lists every sandbox that the agent has a record of, including sandboxes that ended while the device was offline, with their end reasons. The server replaces the stored state of each sandbox and snapshot of the device with the reported state. A sandbox that the server knows but the report does not list changes to `failed` with `LOST_ON_DEVICE`.
- Lost VMs: when the agent has a record of a sandbox but its VM no longer exists (for example, after a host reboot, or after an agent restart on macOS), the agent reports the sandbox as `failed` with `LOST_ON_DEVICE` and releases its resources.
- Commands: the server sends create, delete, pause, resume, snapshot, export, import, clean-up and update commands with a command ID. The agent reports each result with that command ID. A command for an offline device fails immediately with `unavailable`; the server does not queue it.

### Scheduler

- A device is eligible for a create request when it is `online`, its host OS and architecture match the filters, it supports the requested guest (see [the agent capabilities](cmds-ownsand-agent-foundation.md#isolation)), and its free capacity covers the requested resources. Free capacity is the device limit minus the resources of its `creating`, `running`, `paused` and `deleting` sandboxes. A `deleting` sandbox releases its resources only when the agent confirms the deletion, or when a state report shows that the sandbox is gone.
- For a `MACOS` guest, the device must also be an Apple silicon Mac with fewer than two macOS guests in `creating`, `running`, `paused` or `deleting` state, counted from the server's own sandbox records. This is the Virtualization.framework limit; Linux guests do not count toward it.
- The sandbox architecture is the device architecture. OwnSand does not emulate another architecture.
- Among eligible devices, the scheduler selects the one with the lowest allocated share of its most constrained resource, and breaks ties by the lowest `device_id`.
- With an explicit `device_id`, the scheduler checks the same conditions and fails with `failed_precondition` instead of selecting another device.
- A sandbox from a snapshot or template runs on the device that holds the snapshot locally, or on any eligible device with the same OS and architecture when the snapshot is exported.
- The scheduler reserves the resources in the same transaction that creates the sandbox record, so two concurrent requests cannot both use the last free capacity of a device.

### TTL

- `idle_ttl` and `max_ttl` are required. The server stores both deadlines and sends them to the agent with the create command.
- The agent enforces both TTLs locally, so they apply even when the device is offline. When a TTL ends, the agent deletes the sandbox and reports `deleted` with `IDLE_TTL_EXPIRED` or `MAX_TTL_EXPIRED`.
- `max_ttl` counts from creation and includes paused time.
- The idle clock restarts at each exec start, exec end, file operation, resume and data path session request. A running exec keeps the sandbox active. The idle clock does not run while the sandbox is paused.

### Data path signaling and relay

- `OpenSession` authorizes the caller for the sandbox, forwards the caller's SDP offer to the agent on the agent channel with the session ID, the principal and an absolute expiry, and returns the agent's answer. The agent accepts the data channel only for a DTLS fingerprint that arrived in an offer from the control plane. The control plane therefore authenticates both peers of every data channel.
- Session end: the agent enforces the session limits on the peer-to-peer path, because peer-to-peer frames do not pass through the control plane. A session ends at the first of these events: 15 minutes without frames; the absolute expiry, 1 hour after `OpenSession`; a revocation message from the control plane; or 60 seconds after the agent loses its agent channel. The control plane sends a revocation message for every open session of a principal when its API key is revoked or its membership ends, and for every session of a sandbox when the sandbox is deleted. The caller calls `OpenSession` again to continue after the absolute expiry.
- Relay requests are authorized for each RPC call, so revocation takes effect at the next call. An open relay stream ends when the server receives the revocation.
- STUN: open decision. STUN needs public UDP, and Railway public networking does not expose UDP. Record the STUN deployment target and its address configuration here before the peer-to-peer implementation. The deployment becomes a documented deviation from the Railway default.
- Relay: the relay RPCs forward `DataFrame` messages between the caller's Connect stream and the agent channel through bounded in-memory buffers with back-pressure. The relay never writes frames to disk, to the database, to logs or to R2. Record the relay deployment, its per-connection buffer limit and its concurrency limit here before the relay implementation.
- The data path is selected by the client according to the [SDK contract](packages-ownsand-sdk-foundation.md#data-path). The server records the selected `DataPathTransport` in logs and metrics only.

### Device removal and organization deletion

- `RemoveDevice` marks the device `removed` and invalidates its session credentials in the same transaction. The server closes the open channel after it sends a clean-up command. An online agent deletes its local sandboxes, snapshots and locally built images, reports the result, and stops. An offline agent receives the removal notice from `Authenticate` at its next connection, deletes its local data and stops. A removed device can never get a session credential again.
- The sandboxes of a removed device change to `deleted` with `DEVICE_REMOVED`. The server deletes the records of snapshots that exist only on the removed device. An exported snapshot is organization data: its record stays, without a local copy, until it is deleted or the organization is deleted.
- Removal tombstone: the server keeps the device ID, its public key and the `removed` state after removal, also after organization deletion, until the agent confirms its local clean-up. The tombstone holds no other data. It lets `Authenticate` return the removal notice to an agent that was offline during the removal.
- `DeleteOrganization` marks the organization deleted, revokes its API keys, enrollment tokens and invitations, removes its devices as above, deletes its exported snapshots from R2 and then deletes its records, except the device removal tombstones and the audit events. A clean-up job retries R2 deletions until they succeed and logs each failure. Audit events of the organization stay for 90 days.

### Agent version policy

- The server holds one default target agent version per platform. The organization policy is `FOLLOW_DEFAULT`, `PIN` with a version, or `HOLD`.
- The server sends the target version that results from the policy on the agent channel. The [agent contract](cmds-ownsand-agent-foundation.md#agent-updates) owns verification and the conditions for installation.
- `ListDevices` reports each agent version and `AgentUpdateState`.

### Audit events

The server writes one audit event in the same transaction as each action below. Audit events record admin actions and sandbox creation and deletion. They never record exec commands, exec output, file contents, secrets or labels.

| `AuditAction`                  | Trigger                                           |
| ------------------------------ | ------------------------------------------------- |
| `ORGANIZATION_CREATED`         | `CreateOrganization`                              |
| `ORGANIZATION_DELETED`         | `DeleteOrganization`                              |
| `OWNERSHIP_TRANSFERRED`        | `TransferOwnership`                               |
| `INVITATION_CREATED`           | `CreateInvitation`                                |
| `INVITATION_REVOKED`           | `RevokeInvitation`                                |
| `MEMBER_JOINED`                | `AcceptInvitation`                                |
| `MEMBER_ROLE_CHANGED`          | `UpdateMemberRole`                                |
| `MEMBER_REMOVED`               | `RemoveMember`                                    |
| `ENROLLMENT_TOKEN_CREATED`     | `CreateEnrollmentToken`                           |
| `ENROLLMENT_TOKEN_REVOKED`     | `RevokeEnrollmentToken`                           |
| `DEVICE_ENROLLED`              | `AgentService.Enroll`                             |
| `DEVICE_LIMITS_CHANGED`        | `UpdateDeviceLimits`                              |
| `DEVICE_REMOVED`               | `RemoveDevice`                                    |
| `AGENT_VERSION_POLICY_CHANGED` | `UpdateAgentVersionPolicy`                        |
| `API_KEY_CREATED`              | `CreateApiKey`                                    |
| `API_KEY_REVOKED`              | `RevokeApiKey`                                    |
| `SANDBOX_CREATED`              | `CreateSandbox`                                   |
| `SANDBOX_DELETED`              | `DeleteSandbox` and TTL, removal or deletion ends |

- Fields: `audit_event_id`, `organization_id`, `action`, actor kind (`user`, `api_key`, `device` or `system`) and actor ID, target kind and target ID, result, request ID and time.
- Retention: 90 days. A daily job deletes older events. Organization deletion does not delete audit events early.
- The first version defines no audit read RPC. Adding one is a change to this contract and the protos contract.

## Storage

- PlanetScale Postgres tables (`planned`): organizations, memberships, invitations, devices, device limits, enrollment tokens, API keys, sandboxes, snapshots, templates, agent version policies, agent releases and audit events. Every entity uses a UUID v7 primary key, and every organization-owned table has an `organization_id` column that every query filters on.
- Secret columns hold only SHA-256 hashes of enrollment tokens, invitation secrets, API keys and device session credentials, plus a short non-secret prefix for display. Devices store only their public key.
- Exported snapshots use Cloudflare R2 with signed URLs, according to the repository default. Object keys start with `organizations/<organization_id>/snapshots/<snapshot_id>/`, so organization deletion can remove them by prefix. Signed upload and download URLs expire after 15 minutes and are issued only to the agent of the device that runs the export or import.
- Open decisions: the migration tool and the backup policy. Record them here, and add the database stage to the [deployment workflow](repository-workflow-contract.md#deployment-workflow), before the first persisted entity.

## Security

- Accept only the credentials in [Authentication](#authentication). Never accept Logto ID tokens.
- Return each secret once in the response that issues it. Never store or log it in plain text.
- Isolate organizations in every query, as [Authorization](#authorization) requires.
- Rate-limit `Enroll`, `Authenticate`, `AcceptInvitation` and API key validation failures per source address and per prefix. Record the limits here with the implementation.
- The relay and the agent channel are content-blind for storage: they forward sandbox content in memory only.
- Configuration secrets (database URL, R2 credentials, release manifest settings) are Railway service variables according to the [environment contract](repository-environment-contract.md). They never appear in `.env.example`.

## Logging

- Structured JSON logs on standard output with `log/slog`, like `accounts-api`.
- One record per RPC with the procedure, principal kind, principal ID (Logto `sub`, API key ID or device ID), organization ID, Connect code and duration.
- Agent channel events: enrollment, authentication result, connect, disconnect, offline transition, state report counts, command ID and result.
- Data path events: session ID, sandbox ID, selected transport, frame counts, byte counts and duration.
- Never log tokens, API keys, enrollment tokens, invitation secrets, device session credentials, signed URLs, SDP bodies, exec commands, environment values, exec output or file contents.

## Build and Test

Record the package scripts with the implementation, following `accounts-api`:

- `pnpm exec turbo run build --filter=ownsand-api`, `lint` (`go vet ./...`) and `test` (`go test ./...`).
- From the repository root: `gofmt` check, `go vet ./...` and `go test ./...`, according to `servers/AGENTS.md`.
- Required tests: the authorization matrix for every RPC with each principal kind, cross-organization access, enrollment with an expired, reused and revoked token, a removed device that tries to authenticate (also after organization deletion), TTL validation, scheduler eligibility (including the macOS guest limit and `deleting` sandboxes) and concurrent reservation, session revocation, relay forwarding without persistence, relay keepalive and exec resume, and log and audit redaction.

## Dependencies and Integrations

- Upstream: Logto shared tenant, PlanetScale Postgres, Cloudflare R2, the STUN service (open decision).
- Downstream: [device agent](cmds-ownsand-agent-foundation.md), [CLI](cmds-ownsand-cli-foundation.md), [web console](apps-ownsand-console-foundation.md) and [SDKs](packages-ownsand-sdk-foundation.md).
- Protocol schemas: [protos contract](protos-ownsand-api-contract.md).
- `delino-accounts`: the console links to `https://accounts.delino.io`. The control plane does not call the `delino-accounts` API in this version.

## Change Triggers

- Update [project-ownsand.md](project-ownsand.md) when the origin, the authentication exception, the deployment target or an open decision changes.
- Update the [protos contract](protos-ownsand-api-contract.md) with every RPC, message or enum change.
- Update the [agent contract](cmds-ownsand-agent-foundation.md) when the agent channel, commands, TTL enforcement or clean-up rules change.
- Update [repository-workflow-contract.md](repository-workflow-contract.md) when the service joins the deployment workflow.
- Update `servers/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-ownsand.md](project-ownsand.md)
- [servers-accounts-api-foundation.md](servers-accounts-api-foundation.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [repository-environment-contract.md](repository-environment-contract.md)
- [repository-license-contract.md](repository-license-contract.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [Railway public networking limits](https://docs.railway.com/networking/public-networking/specs-and-limits)
