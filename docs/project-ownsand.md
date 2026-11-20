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
- `apps/ownsand/`: Rsbuild React web console (workspace package `ownsand-console`) served at `https://ownsand.delino.io`.
- `protos/delino/ownsand/v1/`: Connect RPC schemas and the committed Go bindings.
- `packages/ownsand-sdk/`: TypeScript SDK (workspace package `ownsand-sdk`, not published) and the generated TypeScript bindings of the schemas.
- `packages/ownsand-sdk-go/`: Go SDK: the client in the package root and the shared data path packages in its subpackages.

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
- Public RPCs that callers reach through `https://ownsand.api.delino.io`, including the relay RPCs, must be unary or server-streaming, so that clients without client or bidirectional streams can use them. Client-streaming RPCs are not used, because browser clients cannot send them. Only the device-facing `RelayAgentService.Connect` (and, depending on its open transport decision, the agent channel) is bidirectional and needs HTTP/2 from end to end. Relay streams send keepalive events and end before 14 minutes, and callers resume them according to the [protos contract](protos-ownsand-api-contract.md#exec). The [control plane contract](servers-ownsand-api-foundation.md#agent-channel) records the transport of the long-lived agent channel.
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

Record each decision in the owning domain contract before the implementation that depends on it. The domain contracts also own narrower open decisions (for example, the macOS userspace network stack, the Windows network enforcement, the Linux base image, the snapshot content per hypervisor, the CLI keychain storage, the verification of the client address header and the stalled relay reader timeout (the header rule and the relay buffer and concurrency limits are recorded), and the WebSocket transport for devices behind HTTP/1.1-only proxies); each is recorded where it applies. The cross-domain ones are:

- The release signing tool and the key custody process ([agent contract](cmds-ownsand-agent-foundation.md#agent-updates)).
- The microVM implementation for Linux hosts (Firecracker class) ([agent contract](cmds-ownsand-agent-foundation.md#isolation)): recorded as Firecracker `v1.17.0` without the jailer; the jailer, the production Linux base image and its signed publication remain open.
- The public UDP port for STUN in the [deployment workflow](repository-workflow-contract.md#deployment-workflow), and the verification of the h2c and long-lived stream behavior of the Caddy route of `ownsand-api` ([control plane contract](servers-ownsand-api-foundation.md#runtime-and-language)).
- The WebSocket fallback of the agent channel and the relay connection for devices behind HTTP/1.1-only proxies. The primary transport is recorded: a Connect bidirectional stream over HTTP/2 ([control plane contract](servers-ownsand-api-foundation.md#agent-channel)).
- The database backup policy ([control plane contract](servers-ownsand-api-foundation.md#storage)). The migration tool is recorded there (an embedded forward-only runner).
- The npm package name and publication workflow of the TypeScript SDK. The workspace package `ownsand-sdk` is private and unpublished; the repository publishes no packages to a registry today, so the [workflow contract](repository-workflow-contract.md) must change before the first publication.
- Every new third-party dependency. The protobuf code generation tool (buf with the Go and Connect plugins) and the Go protobuf and Connect runtimes are recorded in the [protos contract](protos-ownsand-api-contract.md#runtime-and-language). The control plane records its Postgres driver (`github.com/jackc/pgx/v5`), JWT library (`github.com/go-jose/go-jose/v4`) and STUN library (`github.com/pion/stun/v3`) in its [contract](servers-ownsand-api-foundation.md#dependencies-and-integrations). The device agent records `golang.org/x/sys`, its WebRTC library (`github.com/pion/webrtc/v4`) and Firecracker in the [agent contract](cmds-ownsand-agent-foundation.md#dependencies-and-integrations) and its data path section, and the Go SDK records the same WebRTC library in its [contract](packages-ownsand-sdk-foundation.md#dependencies-and-integrations). The TypeScript SDK bundles no WebRTC library ([SDK contract](packages-ownsand-sdk-foundation.md#runtime-and-language)). OwnSand also needs hypervisor bindings for Virtualization.framework and Hyper-V. Each dependency needs owner approval and a [dependency security](repository-dependency-security-contract.md) review in the change that adds it.

## End-to-end test

The cross-component suite in `servers/ownsand-api/e2e/` (package `e2e`, licensed like the control plane) runs the real components together on one Linux host: it builds the control plane, the device agent, the guest helper and the CLI, starts the control plane on a fresh Postgres database with a test issuer key set and loopback STUN, enrolls agents with the `process` runtime (the guest helper runs as a host process without isolation; guest paths are host paths below the sandbox root), and drives the lifecycle through the Go SDK and the CLI binary on the forced peer-to-peer and relay transports: organization creation with a user access token, enrollment, API key, create, exec (stdout, stderr and exit code), a multi-chunk file write and read, exec detach and `AttachExec`, pause, resume, snapshot, a sandbox created from the snapshot, snapshot deletion (including the device's local copy) and delete. It also exports a multi-part snapshot to an in-process fake S3 store and imports it on a second device, and runs the TypeScript SDK test `packages/ownsand-sdk/src/e2e.test.ts` over the relay.

- Run it with `OWNSAND_E2E=1 OWNSAND_TEST_DATABASE_URL=<postgres URL whose user may create databases> go test ./servers/ownsand-api/e2e/` from the repository root after `pnpm install` (the TypeScript part needs `pnpm` on `PATH` and is skipped without it). `OWNSAND_E2E_LOGS=1` prints the logs of the child processes also when the tests pass. Without `OWNSAND_E2E=1` or the database URL, or with `-short`, the suite is skipped.
- `TestLifecycleFirecracker` runs the Go SDK lifecycle with the Firecracker runtime and network `NONE` (including a memory-state restore) when `OWNSAND_FC_TEST_BINARY`, `OWNSAND_FC_TEST_KERNEL` and `OWNSAND_FC_TEST_ROOTFS` name the Firecracker binary, the guest kernel and a root file system from `cmds/ownsand-agent/images/linux/build-rootfs.sh`, like the agent's [Firecracker integration test](cmds-ownsand-agent-foundation.md#build-and-test); it needs `/dev/kvm` access but no root, and CI does not run it.
- CI runs it in the `go` job with a Postgres service container, according to the [workflow contract](repository-workflow-contract.md#validation).
- It does not cover the `OUTBOUND` network policy (which needs root), the macOS and Windows hypervisor runtimes, signed updates, the WebSocket fallback, real R2, TLS or real Logto, macOS or Windows hosts; those stay part of device acceptance.

## Change Policy

- Update this index and every affected domain contract together when component IDs, origins, ownership paths, the authentication exception, the data path or the licensing boundary change.
- Change a `planned` path to an actual path in the same change that creates it. That change also adds the workspace membership, the package scripts and the validation commands that the owning domain contract defines.
- Update the [protos contract](protos-ownsand-api-contract.md) before or with every RPC, message or enum change. Check the [tide project](project-tide.md), which consumes the public API, when a public RPC changes. Update the SDK, CLI and console contracts when a change affects their surface.
- Update [repository-auth-contract.md](repository-auth-contract.md) when the API key, enrollment token or device credential exception changes.
- Update the [documentation catalog](README.md) when contracts are added, renamed or removed.
- Update the [control plane contract](servers-ownsand-api-foundation.md) and this index together when the traffic placement or the deployment target changes.
- Update `apps/AGENTS.md`, `cmds/AGENTS.md`, `servers/AGENTS.md`, `packages/AGENTS.md` and `protos/AGENTS.md` only when development procedures, directory ownership or development rules change; follow the [instruction-update policy](README.md#instruction-update-policy).
- Record implementation status, validation results and unresolved limits in pull requests, Linear issues and CI logs/artifacts. Distinguish fixture, build and packaging checks from acceptance on real Linux, macOS and Windows devices.

## References

- [servers-ownsand-api-foundation.md](servers-ownsand-api-foundation.md)
- [project-tide.md](project-tide.md): a consumer of the public API that runs GitHub Actions jobs and coding-agent sessions on OwnSand devices.
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
