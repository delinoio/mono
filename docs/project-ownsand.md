# Project: ownsand

## Goal

OwnSand turns devices that a team already owns into a private sandbox cloud. A team registers its Linux, macOS and Windows devices in an OwnSand organization. Members and programs then create, use and delete isolated sandboxes on those devices through one Connect RPC API. They do not need a third-party sandbox provider, and sandbox disks and snapshots stay on the team's own hardware by default. Exec output and file contents reach callers in two ways: directly from the device over the peer-to-peer data channel, or in transit through the relay of the control plane, which stores nothing. Sandbox disks leave the devices only in snapshots that a caller exports to R2 on purpose.

Success criterion: on one Linux, one macOS and one Windows device, an organization member runs the full sandbox lifecycle through the SDK. The lifecycle is create, exec, file read and write, pause, resume, snapshot and delete.

## Project ID

`ownsand`

## Domain Ownership Map

Paths marked `planned` do not exist yet. Create each path, its workspace membership and its validation scripts in the change that starts its runtime implementation.

- `servers/ownsand-api/`: Go control plane on the shared DigitalOcean Droplet, served at `https://ownsand.api.delino.io`. It also serves the relay fallback of the data path and STUN on UDP port 3478.
- `cmds/ownsand-agent/`: Go device agent that runs on each enrolled host.
- `cmds/ownsand-guest/`: Go helper that runs inside each sandbox guest and serves exec and file requests from the device agent.
- `cmds/ownsand/`: Go command-line interface for people and programs.
- `apps/ownsand/`: Rsbuild React web console served at `https://ownsand.delino.io` (workspace package `ownsand-console`).
- `protos/delino/ownsand/v1/`: Connect RPC schemas and the committed Go bindings.
- `packages/ownsand-sdk/`: TypeScript SDK and the generated TypeScript bindings.
- `packages/ownsand-sdk-go/`: Go SDK and the shared data path primitives.

The project has one hosted server deployable (the control plane on the shared DigitalOcean Droplet), one static site (the web console) and three distributed programs (the device agent, the guest helper and the CLI). The project has no `crates` paths.

## Domain Contract Documents

- [servers-ownsand-api-foundation.md](servers-ownsand-api-foundation.md): control plane, relay and STUN.
- [cmds-ownsand-agent-foundation.md](cmds-ownsand-agent-foundation.md): device agent, guest helper, isolation, networking and agent updates.
- [cmds-ownsand-cli-foundation.md](cmds-ownsand-cli-foundation.md): command-line interface.
- [apps-ownsand-console-foundation.md](apps-ownsand-console-foundation.md): web console.
- [protos-ownsand-api-contract.md](protos-ownsand-api-contract.md): Connect RPC services, messages and the data path framing.
- [packages-ownsand-sdk-foundation.md](packages-ownsand-sdk-foundation.md): TypeScript and Go SDKs.

## Cross-Domain Invariants

### Components

| Component ID      | Path                        | Kind                  | Production origin or distribution    |
| ----------------- | --------------------------- | --------------------- | ------------------------------------ |
| `ownsand-api`     | `servers/ownsand-api/`      | Droplet server        | `https://ownsand.api.delino.io`      |
| `ownsand-console` | `apps/ownsand/`             | Cloudflare Pages site | `https://ownsand.delino.io`          |
| `ownsand-agent`   | `cmds/ownsand-agent/`       | Host service binary   | Signed release, self-updating        |
| `ownsand-guest`   | `cmds/ownsand-guest/`       | Guest helper binary   | Installed into each guest image      |
| `ownsand-cli`     | `cmds/ownsand/`             | CLI binary            | Release artifact                     |
| `ownsand-sdk-ts`  | `packages/ownsand-sdk/`     | TypeScript package    | npm package                          |
| `ownsand-sdk-go`  | `packages/ownsand-sdk-go/`  | Go package            | Root Go module import path           |
| `ownsand-protos`  | `protos/delino/ownsand/v1/` | Schemas and bindings  | Root Go module and generated TS code |

### Protocol and API

- The public API is OwnSand's own Connect RPC API in the protobuf package `delino.ownsand.v1`. It is not compatible with the E2B API. The [protos contract](protos-ownsand-api-contract.md) owns every service, message and enum.
- Every business flow between components uses Connect RPC or the protobuf framing that the protos contract defines. Plain HTTP endpoints are limited to operational endpoints such as health checks.
- Hosting facts: `ownsand-api` runs as a Docker container on the shared DigitalOcean Droplet. The Caddy reverse proxy on the Droplet terminates TLS and proxies to the container; the [control plane contract](servers-ownsand-api-foundation.md#runtime-and-language) requires that route to proxy HTTP/2 (h2c) and to keep long-lived streams, and requires its verification before the relay implementation. The [deployment workflow](repository-workflow-contract.md#deployment-workflow) publishes only HTTP through Caddy today, so it must add the public UDP port for STUN before the first deployment of `ownsand-api`.
- Public RPCs that callers reach through `https://ownsand.api.delino.io`, including the relay RPCs, must be unary or server-streaming, so that clients without client or bidirectional streams can use them. Client-streaming RPCs are not used, because browser clients cannot send them. Only the device-facing `RelayAgentService.Connect` and the agent channel (`AgentService.Channel`, a Connect bidirectional stream by owner decision) are bidirectional and need HTTP/2 from end to end. Relay streams send keepalive events and end before 14 minutes, and callers resume them according to the [protos contract](protos-ownsand-api-contract.md#exec). The [control plane contract](servers-ownsand-api-foundation.md#agent-channel) records the transport of the long-lived agent channel.
- Exec streams and file transfers use a peer-to-peer WebRTC data channel between the caller (SDK or CLI) and the device agent when ICE succeeds. The control plane does the signaling. When the peer-to-peer connection fails, the caller gets a relay grant from the control plane and uses the relay RPCs of the control plane, which forward the same frames to the agent over the agent's relay connection. The relay does not store payloads.
- Traffic placement: traffic whose volume grows with sandbox content (exec output and file contents) uses the peer-to-peer path first. The relay carries it only as a fallback, on a separate relay connection, never on the agent channel, so that content does not delay commands, heartbeats and revocations. Relay traffic leaves the Droplet once, toward the caller for exec output and file reads, or toward the agent for file writes. DigitalOcean bills Droplet outbound transfer beyond the pooled allowance at $0.01 per GiB and does not bill inbound transfer ([DigitalOcean bandwidth billing](https://docs.digitalocean.com/platform/billing/bandwidth/)), and the relay shares the Droplet with the other servers, so the [relay limits](servers-ownsand-api-foundation.md#relay-streams) bound its share. A new data flow whose volume grows with sandbox content must use the peer-to-peer path, the relay or R2 signed URLs, never the API RPCs or the agent channel.
- The control plane authorizes each relay use with a relay grant: it authorizes the caller with a Logto access token or an API key, and sends the grant to the agent on the agent channel. Each relay RPC carries the caller's own credential and the grant ID, and the agent's relay connection uses the device session credential, so the relay adds no credential. The agent admits new relay streams only for live grants and ends admitted streams under the [agent contract](cmds-ownsand-agent-foundation.md#data-path) rules, so revocation does not depend only on the control plane's relay check. The [control plane contract](servers-ownsand-api-foundation.md#relay-authentication) owns relay authentication.

### Identity and access

- People authenticate with Logto access tokens according to the [authentication contract](repository-auth-contract.md). The Logto user ID (`sub`) is the user ID in every OwnSand table.
- OwnSand stores organizations, memberships and invitations in its own tables. It does not use Logto Organizations.
- Roles use the enum identifiers `owner`, `admin` and `member`. `admin` includes every permission of `member`, and `owner` every permission of `admin`, except that the `owner` cannot leave the organization without first transferring ownership. The [control plane contract](servers-ownsand-api-foundation.md#authorization) owns the authorization matrix.
- **Authentication contract exception.** Programs (SDK, CI and AI agents) authenticate with organization API keys that admins issue and revoke. Device agents enroll with one-time enrollment tokens that admins issue: a token expires within 24 hours and creates exactly one device in one organization. An API key is a credential of the organization, not a user identity. It is scoped to exactly one organization and has a subset of the `member` permissions: it can use devices, sandboxes, snapshots and templates, but it cannot read the organization or its members. The [authorization matrix](servers-ownsand-api-foundation.md#authorization) defines the exact operations. This is a project-level exception to the rule that backend services accept only Logto access tokens. It does not add an identity provider, a password store or a sign-in flow for people. The [authentication contract](repository-auth-contract.md#documented-exceptions) lists this exception.
- **Relay grants.** The relay accepts no credential of its own. A relay grant, which the control plane creates after it authorizes the caller, is scoped to one organization, device, sandbox and operation (one exec, exec attach, file read or file write; exec, read and write grants carry the digest of the command or guest path), expires at most 15 minutes after issue and is never persisted. Each relay RPC needs both the grant ID and the credential of the grant's principal, and the agent's relay connection needs the device session credential. The device agent admits a relay stream only while the matching relay grant, which the control plane sent on the agent channel, is live, and ends admitted streams on revocation, agent channel loss or the grant's stream deadline.
- Device agents authenticate with a device key pair that the agent generates locally during enrollment. Device credentials are not user credentials and grant access only to the agent channel and the relay connection of their own device.
- The server stores only hashes of enrollment tokens, invitation secrets, API keys and device session credentials. It shows each secret once, at issue time.
- A user, an API key or a device can never see or change the devices, sandboxes, snapshots, templates, members or API keys of another organization.

### Identifiers and states

- New persisted entities use UUID v7 identifiers: organizations, memberships, invitations, devices, enrollment tokens, API keys, sandboxes, snapshots, templates and audit events. Callers generate the `exec_id`, `read_id` and `write_id` values as UUID v7; the control plane does not persist them and does not trust them as server identifiers. The create `idempotency_key` is also caller-generated; the control plane stores it with the reservation for 24 hours.
- Sandbox, device, snapshot and agent-version-policy states use the enums in the [protos contract](protos-ownsand-api-contract.md#enums). Components must not compare free-form state strings.
- A sandbox stays on the device that created it for its whole life. OwnSand never moves a sandbox to another device. When a device goes offline, its non-terminal sandboxes change to `unreachable`; `deleted` and `failed` sandboxes keep their state, according to the [control plane contract](servers-ownsand-api-foundation.md#agent-channel). When the agent reconnects, the agent reports the actual state and the control plane reconciles the stored state with it according to the [control plane contract](servers-ownsand-api-foundation.md#agent-channel).

### Licensing and code sharing

- `servers/ownsand-api/` uses `FSL-1.1-ALv2` according to the [license contract](repository-license-contract.md). Every other OwnSand path uses Apache-2.0.
- The agent, the guest helper, the CLI and the SDKs must not import code from `servers/`. Code that the control plane shares with them lives in `protos/delino/ownsand/v1/` or `packages/ownsand-sdk-go/`.

### Repository rules that apply without change

- Account management links to `https://accounts.delino.io`. OwnSand builds no profile or account settings pages.
- OwnSand does not charge users in this version. Billing and usage reporting to `delino-accounts` are out of scope. A later change that adds charges must use `delino-accounts` according to the [billing contract](repository-billing-contract.md).
- Logs use `log/slog` in Go. No log record or audit event contains secrets, tokens, API keys, exec commands, exec output or file contents.

### Deviations from repository defaults

- Program authentication with organization API keys, as described above.
- Sandbox disks, memory state, local snapshots and the locally built macOS and Windows base images stay on the device file system, not in Cloudflare R2. They are sandbox content that belongs to the device owner. Only an explicit snapshot export goes to R2, with signed URLs, according to the default.

### Platform and license constraints

- Linux hosts need KVM access for the microVM runtime.
- macOS guests need an Apple silicon host. Virtualization.framework runs at most two macOS guests at the same time on one Mac. Linux guests do not count toward this limit.
- Windows hosts need Hyper-V, which Windows 11 Pro, Enterprise and Education and Windows Server include. Windows 11 Home does not include Hyper-V.
- Delino does not distribute macOS or Windows images. Each device builds them locally from an Apple IPSW or from a Windows ISO that an admin supplies. The organization is responsible for the operating system license terms of its guests.

### Open decisions

Record each decision in the owning domain contract before the implementation that depends on it. The domain contracts also own narrower open decisions (for example, the macOS userspace network stack, the Windows network enforcement, the Linux base image, the snapshot content per hypervisor, the CLI keychain storage, the rate-limit client address header and the WebSocket transport for devices behind HTTP/1.1-only proxies); each is recorded where it applies. The cross-domain ones are:

- The release signing tool and the key custody process ([agent contract](cmds-ownsand-agent-foundation.md#agent-updates)).
- Decided: the microVM implementation for Linux hosts is Firecracker, pinned in the [agent contract](cmds-ownsand-agent-foundation.md#isolation); the jailer setup with one unprivileged user per sandbox is still open there.
- The public UDP port for STUN in the [deployment workflow](repository-workflow-contract.md#deployment-workflow), and the verification of the h2c and long-lived stream behavior of the Caddy route of `ownsand-api` ([control plane contract](servers-ownsand-api-foundation.md#runtime-and-language)).
- The database backup policy and the database stage of the deployment workflow ([control plane contract](servers-ownsand-api-foundation.md#storage)). The migration tool is decided: an in-house runner over embedded SQL files.
- The registry name and publication workflow of the TypeScript SDK (the workspace package `@delino/ownsand-sdk` is private). The repository publishes no packages to a registry today, so the [workflow contract](repository-workflow-contract.md) must change before the first publication.
- Every new third-party dependency. The [protos contract](protos-ownsand-api-contract.md#runtime-and-language) records the code generation tool and the approved Connect and protobuf runtimes. The owner also approved a Postgres driver (pgx), pion (WebRTC and STUN) and the TypeScript Connect and protobuf runtimes; each is recorded in the change that adds it, with a [dependency security](repository-dependency-security-contract.md) review. Hypervisor bindings for Virtualization.framework and Hyper-V still need owner approval.

## End-to-end acceptance

The opt-in Go test `packages/ownsand-sdk-go/e2e/` runs the components together on one Linux host. It builds `ownsand-api`, `ownsand-agent` and the `ownsand` CLI, starts the control plane against a new Postgres database and a local JWKS issuer that stands in for Logto, enrolls and runs a real agent with Firecracker microVMs, and drives the Go SDK and the CLI. It imports only public packages; the control plane is used only as a built binary. Without `OWNSAND_E2E=1` it skips, so CI stays green.

Steps, each forced once onto the relay and once onto the peer-to-peer data channel (loopback host candidates and the control plane STUN server): create an organization, create an enrollment token, enroll and connect the agent, set the device limits, and then through the Go SDK create a sandbox, exec (stdout, stderr and the exit code), write a multi-chunk file and an empty file, read them, pause, resume, snapshot, create a sandbox from the snapshot and read its disk, and delete. The SDK steps also cover a command that cannot start, an exec timeout, `AttachExec` of an ended exec, the `not_found` and `failed_precondition` error frames, and an API key revocation during an exec. The CLI runs the same lifecycle with an API key. Then an idle TTL expiry (`IDLE_TTL_EXPIRED`), an agent killed and restarted (`FAILED` with `LOST_ON_DEVICE`) and device removal (`DEVICE_REMOVED`, the agent confirms the removal, deletes its identity and exits).

Requirements: Linux with read and write access to `/dev/kvm`, Go, `mkfs.ext4` (e2fsprogs, used without root through `mkfs.ext4 -d`) and a Postgres server where the test may create and drop a database. Sandboxes use `NETWORK_MODE_NONE`, so the agent needs no `CAP_NET_ADMIN`; its network policy errors in the log are expected. The artifact directory holds:

- `firecracker`: `firecracker-v1.17.0-<arch>` from the official release, checked against its `.sha256.txt`, according to the [agent contract](cmds-ownsand-agent-foundation.md#isolation).
- `vmlinux`: an uncompressed guest kernel with virtio-mmio block, virtio vsock and kernel IP autoconfiguration, according to the [agent contract](cmds-ownsand-agent-foundation.md#images).
- `busybox`: a static busybox binary. The test builds `rootfs.ext4` from it, a minimal init and a static `ownsand-guest` built from the same revision, so the guest helper always matches the agent under test.

Command (from the repository root; `OWNSAND_TEST_DATABASE_URL` also works for the database):

```sh
OWNSAND_E2E=1 \
OWNSAND_E2E_DATABASE_URL='postgres://<user>:<password>@127.0.0.1:5432/postgres?sslmode=disable' \
OWNSAND_E2E_FC_DIR=<artifact directory> \
go test -count=1 -v -timeout 30m ./packages/ownsand-sdk-go/e2e/
```

`OWNSAND_E2E_LOG_DIR=<dir>` keeps the control plane, agent and enrollment logs; the test prints their tails when it fails. `OWNSAND_E2E_LONG=1` adds a silent 15-minute relay exec that must survive the control plane's 14-minute stream rotation (raise `-timeout` to 45m). The test does not cover `OUTBOUND` networking, snapshot export, macOS and Windows hosts, NAT traversal across networks or the Logto tenant itself; those remain device acceptance items. Record the results in pull requests and Linear issues according to the change policy below.

## Change Policy

- Update this index and every affected domain contract together when component IDs, origins, ownership paths, the authentication exception, the data path or the licensing boundary change.
- Change a `planned` path to an actual path in the same change that creates it. That change also adds the workspace membership, the package scripts and the validation commands that the owning domain contract defines.
- Update the [protos contract](protos-ownsand-api-contract.md) before or with every RPC, message or enum change. Update the SDK, CLI and console contracts when a change affects their surface.
- Update [repository-auth-contract.md](repository-auth-contract.md) when the API key, enrollment token or device credential exception changes.
- Update the [documentation catalog](README.md) when contracts are added, renamed or removed.
- Update the [control plane contract](servers-ownsand-api-foundation.md) and this index together when the traffic placement or the deployment target changes.
- Update `apps/AGENTS.md`, `cmds/AGENTS.md`, `servers/AGENTS.md`, `packages/AGENTS.md` and `protos/AGENTS.md` only when development procedures, directory ownership or development rules change; follow the [instruction-update policy](README.md#instruction-update-policy).
- Record implementation status, validation results and unresolved limits in pull requests, Linear issues and CI logs/artifacts. Distinguish fixture, build and packaging checks from acceptance on real Linux, macOS and Windows devices.

## References

- [servers-ownsand-api-foundation.md](servers-ownsand-api-foundation.md)
- [DigitalOcean bandwidth billing](https://docs.digitalocean.com/platform/billing/bandwidth/)
- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [repository-billing-contract.md](repository-billing-contract.md)
- [repository-dependency-security-contract.md](repository-dependency-security-contract.md)
- [repository-environment-contract.md](repository-environment-contract.md)
- [repository-license-contract.md](repository-license-contract.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [project-delino-accounts.md](project-delino-accounts.md)
- [project-template.md](project-template.md)
