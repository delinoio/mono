# Project: ownsand

## Goal

OwnSand turns devices that a team already owns into a private sandbox cloud. A team registers its Linux, macOS and Windows devices in an OwnSand organization. Members and programs then create, use and delete isolated sandboxes on those devices through one Connect RPC API. They do not need a third-party sandbox provider, and sandbox disks and snapshots stay on the team's own hardware by default. Exec output and file contents reach callers in two ways: directly from the device over the peer-to-peer data channel, or in transit through the relay server, which stores nothing. Sandbox disks leave the devices only in snapshots that a caller exports to R2 on purpose.

Success criterion: on one Linux, one macOS and one Windows device, an organization member runs the full sandbox lifecycle through the SDK. The lifecycle is create, exec, file read and write, pause, resume, snapshot and delete.

## Project ID

`ownsand`

## Domain Ownership Map

All paths are `planned`. This project is in the documentation-only phase. Create each path, its workspace membership and its validation scripts in the change that starts its runtime implementation.

- `servers/ownsand-api/` (`planned`): Go control plane served at `https://ownsand.api.delino.io`.
- `servers/ownsand-relay/` (`planned`): Go relay server on a DigitalOcean Droplet, served at `https://ownsand-relay.api.delino.io` with STUN on UDP port 3478.
- `cmds/ownsand-agent/` (`planned`): Go device agent that runs on each enrolled host.
- `cmds/ownsand-guest/` (`planned`): Go helper that runs inside each sandbox guest and serves exec and file requests from the device agent.
- `cmds/ownsand/` (`planned`): Go command-line interface for people and programs.
- `apps/ownsand/` (`planned`): Rsbuild React web console served at `https://ownsand.delino.io`.
- `protos/delino/ownsand/v1/` (`planned`): Connect RPC schemas and the committed Go bindings.
- `packages/ownsand-sdk/` (`planned`): TypeScript SDK.
- `packages/ownsand-sdk-go/` (`planned`): Go SDK.

The project has two hosted deployables (the control plane on Railway and the relay server on DigitalOcean), one static site (the web console) and three distributed programs (the device agent, the guest helper and the CLI). The project has no `crates` paths.

## Domain Contract Documents

- [servers-ownsand-api-foundation.md](servers-ownsand-api-foundation.md): control plane.
- [servers-ownsand-relay-foundation.md](servers-ownsand-relay-foundation.md): relay server and STUN.
- [cmds-ownsand-agent-foundation.md](cmds-ownsand-agent-foundation.md): device agent, guest helper, isolation, networking and agent updates.
- [cmds-ownsand-cli-foundation.md](cmds-ownsand-cli-foundation.md): command-line interface.
- [apps-ownsand-console-foundation.md](apps-ownsand-console-foundation.md): web console.
- [protos-ownsand-api-contract.md](protos-ownsand-api-contract.md): Connect RPC services, messages and the data path framing.
- [packages-ownsand-sdk-foundation.md](packages-ownsand-sdk-foundation.md): TypeScript and Go SDKs.

## Cross-Domain Invariants

### Components

| Component ID      | Path                        | Kind                  | Production origin or distribution     |
| ----------------- | --------------------------- | --------------------- | ------------------------------------- |
| `ownsand-api`     | `servers/ownsand-api/`      | Railway server        | `https://ownsand.api.delino.io`       |
| `ownsand-relay`   | `servers/ownsand-relay/`    | DigitalOcean Droplet  | `https://ownsand-relay.api.delino.io` |
| `ownsand-console` | `apps/ownsand/`             | Cloudflare Pages site | `https://ownsand.delino.io`           |
| `ownsand-agent`   | `cmds/ownsand-agent/`       | Host service binary   | Signed release, self-updating         |
| `ownsand-guest`   | `cmds/ownsand-guest/`       | Guest helper binary   | Installed into each guest image       |
| `ownsand-cli`     | `cmds/ownsand/`             | CLI binary            | Release artifact                      |
| `ownsand-sdk-ts`  | `packages/ownsand-sdk/`     | TypeScript package    | npm package                           |
| `ownsand-sdk-go`  | `packages/ownsand-sdk-go/`  | Go package            | Root Go module import path            |
| `ownsand-protos`  | `protos/delino/ownsand/v1/` | Schemas and bindings  | Root Go module and generated TS code  |

### Protocol and API

- The public API is OwnSand's own Connect RPC API in the protobuf package `delino.ownsand.v1`. It is not compatible with the E2B API. The [protos contract](protos-ownsand-api-contract.md) owns every service, message and enum.
- Every business flow between components uses Connect RPC or the protobuf framing that the protos contract defines. Plain HTTP endpoints are limited to operational endpoints such as health checks.
- Railway platform facts: Railway's published limits state the 5-minute and 15-minute request limits and the WebSocket exemption. They do not state the HTTP version between the edge and the container or the absence of public UDP. Railway community reports and templates say that the edge forwards HTTP/1.1 to the container and that public networking exposes no UDP ports. The contracts treat both as constraints; verify them with Railway in the change that records the agent channel transport. STUN runs on the relay server on DigitalOcean, so it does not depend on Railway UDP support. Sources: [specs and limits](https://docs.railway.com/networking/public-networking/specs-and-limits), [HTTP/2 edge feedback](https://station.railway.com/feedback/http-2-support-on-edge-proxy-50adedfe) and [a WebRTC template that uses TCP because of the missing UDP](https://railway.com/deploy/livekit-voice-agent-self-hosted-webrtc-ai-on-railway--livekit-voice-agent).
- The Railway public edge forwards HTTP/1.1 to the container (see the platform facts above). Public RPCs that callers reach through `https://ownsand.api.delino.io` must therefore be unary or server-streaming. Bidirectional RPCs need HTTP/2 from end to end and must not be used on that route. Client-streaming RPCs are not used either, because browser clients cannot send them. The edge also closes a request that transfers no data for 5 minutes or that runs for more than 15 minutes. The relay streams run on the relay server, not behind this edge; they still send keepalive events and end before 14 minutes, and callers resume them according to the [protos contract](protos-ownsand-api-contract.md#exec). The [control plane contract](servers-ownsand-api-foundation.md#interfaces-and-contracts) records the transport of the long-lived agent channel.
- Exec streams and file transfers use a peer-to-peer WebRTC data channel between the caller (SDK or CLI) and the device agent when ICE succeeds. The control plane does the signaling. When the peer-to-peer connection fails, the caller gets a relay ticket from the control plane and uses the relay RPCs of the relay server, which forward the same frames to the agent. The relay server does not store payloads.
- Traffic placement: traffic whose volume grows with sandbox content (relay exec output and file contents) and STUN run on the relay server on DigitalOcean, not on Railway. Railway bills network egress at $0.05 per GB; DigitalOcean bills Droplet outbound transfer beyond the team's pooled Droplet transfer allowance at $0.01 per GiB and does not bill inbound transfer ([Railway pricing](https://docs.railway.com/pricing/plans), [DigitalOcean bandwidth billing](https://docs.digitalocean.com/platform/billing/bandwidth/)). The control plane on Railway carries only the API, signaling, relay authorization and agent channel traffic. A new data flow whose volume grows with sandbox content must use the peer-to-peer path, the relay server or R2 signed URLs, never the control plane or the agent channel.
- The relay server holds no database connection. The control plane authorizes each relay use with signed relay tickets and sends a matching relay grant to the agent on the agent channel; the agent admits new relay streams only for live grants and ends admitted streams under the [agent contract](cmds-ownsand-agent-foundation.md#data-path) rules, so revocation does not depend on the relay server. The [relay server contract](servers-ownsand-relay-foundation.md#relay-tickets) owns ticket verification.

### Identity and access

- People authenticate with Logto access tokens according to the [authentication contract](repository-auth-contract.md). The Logto user ID (`sub`) is the user ID in every OwnSand table.
- OwnSand stores organizations, memberships and invitations in its own tables. It does not use Logto Organizations.
- Roles use the enum identifiers `owner`, `admin` and `member`. `admin` includes every permission of `member`, and `owner` every permission of `admin`, except that the `owner` cannot leave the organization without first transferring ownership. The [control plane contract](servers-ownsand-api-foundation.md#authorization) owns the authorization matrix.
- **Authentication contract exception.** Programs (SDK, CI and AI agents) authenticate with organization API keys that admins issue and revoke. Device agents enroll with one-time enrollment tokens that admins issue: a token expires within 24 hours and creates exactly one device in one organization. An API key is a credential of the organization, not a user identity. It is scoped to exactly one organization and has a subset of the `member` permissions: it can use devices, sandboxes, snapshots and templates, but it cannot read the organization or its members. The [authorization matrix](servers-ownsand-api-foundation.md#authorization) defines the exact operations. This is a project-level exception to the rule that backend services accept only Logto access tokens. It does not add an identity provider, a password store or a sign-in flow for people. The [authentication contract](repository-auth-contract.md#documented-exceptions) lists this exception.
- **Relay tickets.** The relay server accepts only relay tickets, which the control plane signs with an Ed25519 key after it authorizes the caller with a Logto access token or an API key. A `CALLER` ticket is scoped to one organization, device, sandbox, operation (one exec, exec attach, file read or file write; exec, read and write tickets carry the digest of the command or guest path) and relay origin; an `AGENT` ticket is scoped to one device and relay origin, and the relay connection that it opens takes no new requests 30 seconds after its expiry unless the agent refreshes it with a newer `AGENT` ticket. Both expire with their grant, at most 15 minutes after issue. They are never persisted; the lease holder keeps a pending grant and its tickets in memory until the grant's expiry, so that a retried `AuthorizeRelay` returns the same ticket. A relay ticket is not accepted by the control plane and adds no identity: the device agent admits a relay stream only while the matching relay grant, which the control plane sent on the agent channel, is live, and ends admitted streams on revocation, agent channel loss or the grant's stream deadline. The [authentication contract](repository-auth-contract.md#documented-exceptions) lists this credential with the API key exception, and the [relay server contract](servers-ownsand-relay-foundation.md#relay-tickets) owns its verification.
- Device agents authenticate with a device key pair that the agent generates locally during enrollment. Device credentials are not user credentials and grant access only to the agent channel of their own device.
- The server stores only hashes of enrollment tokens, invitation secrets, API keys and device session credentials. It shows each secret once, at issue time.
- A user, an API key or a device can never see or change the devices, sandboxes, snapshots, templates, members or API keys of another organization.

### Identifiers and states

- New persisted entities use UUID v7 identifiers: organizations, memberships, invitations, devices, enrollment tokens, API keys, sandboxes, snapshots, templates and audit events. Callers generate the `exec_id`, `read_id` and `write_id` values as UUID v7; the control plane does not persist them and does not trust them as server identifiers. The create `idempotency_key` is also caller-generated; the control plane stores it with the reservation for 24 hours.
- Sandbox, device, snapshot and agent-version-policy states use the enums in the [protos contract](protos-ownsand-api-contract.md#enums). Components must not compare free-form state strings.
- A sandbox stays on the device that created it for its whole life. OwnSand never moves a sandbox to another device. When a device goes offline, its non-terminal sandboxes change to `unreachable`; `deleted` and `failed` sandboxes keep their state, according to the [control plane contract](servers-ownsand-api-foundation.md#agent-channel). When the agent reconnects, the agent reports the actual state and the control plane reconciles the stored state with it according to the [control plane contract](servers-ownsand-api-foundation.md#agent-channel).

### Licensing and code sharing

- `servers/ownsand-api/` and `servers/ownsand-relay/` use `FSL-1.1-ALv2` according to the [license contract](repository-license-contract.md). Every other OwnSand path uses Apache-2.0.
- The agent, the guest helper, the CLI and the SDKs must not import code from `servers/`. The control plane and the relay server do not import each other's `internal` packages; they share only the protos. Code that the control plane shares with them lives in `protos/delino/ownsand/v1/` or `packages/ownsand-sdk-go/`.

### Repository rules that apply without change

- Account management links to `https://accounts.delino.io`. OwnSand builds no profile or account settings pages.
- OwnSand does not charge users in this version. Billing and usage reporting to `delino-accounts` are out of scope. A later change that adds charges must use `delino-accounts` according to the [billing contract](repository-billing-contract.md).
- Logs use `log/slog` in Go. No log record or audit event contains secrets, tokens, API keys, exec commands, exec output or file contents.

### Deviations from repository defaults

- Program authentication with organization API keys, as described above.
- Sandbox disks, memory state, local snapshots and the locally built macOS and Windows base images stay on the device file system, not in Cloudflare R2. They are sandbox content that belongs to the device owner. Only an explicit snapshot export goes to R2, with signed URLs, according to the default.
- The relay server (`ownsand-relay`) runs on a DigitalOcean Droplet, not on Railway, for two reasons. Relay traffic is the largest egress of the project, and DigitalOcean outbound transfer costs less than Railway egress (see [traffic placement](#protocol-and-api)). STUN needs public UDP, and Railway public networking does not expose UDP ports (see the [platform facts](#protocol-and-api)). The [relay server contract](servers-ownsand-relay-foundation.md#runtime-and-language) owns the Droplet, its firewall and its TLS termination, and the [workflow contract](repository-workflow-contract.md#deployment-workflow) must record its deployment before the first relay deployment.

### Platform and license constraints

- Linux hosts need KVM access for the microVM runtime.
- macOS guests need an Apple silicon host. Virtualization.framework runs at most two macOS guests at the same time on one Mac. Linux guests do not count toward this limit.
- Windows hosts need Hyper-V, which Windows 11 Pro, Enterprise and Education and Windows Server include. Windows 11 Home does not include Hyper-V.
- Delino does not distribute macOS or Windows images. Each device builds them locally from an Apple IPSW or from a Windows ISO that an admin supplies. The organization is responsible for the operating system license terms of its guests.

### Open decisions

Record each decision in the owning domain contract before the implementation that depends on it. The domain contracts also own narrower open decisions (for example, the macOS userspace network stack, the Windows network enforcement, the Linux base image, the snapshot content per hypervisor, the CLI keychain storage and the rate-limit client address header); each is recorded where it applies. The cross-domain ones are:

- The release signing tool and the key custody process ([agent contract](cmds-ownsand-agent-foundation.md#agent-updates)).
- The microVM implementation for Linux hosts (Firecracker class) ([agent contract](cmds-ownsand-agent-foundation.md#isolation)).
- The relay server deployment job ([relay server contract](servers-ownsand-relay-foundation.md#deployment)), its TLS certificate source ([runtime](servers-ownsand-relay-foundation.md#runtime-and-language)) and its buffer and concurrency limits ([relay streams](servers-ownsand-relay-foundation.md#relay-streams)).
- The transport of the long-lived agent channel through the Railway edge ([control plane contract](servers-ownsand-api-foundation.md#agent-channel)).
- The per-device admin settings UI ([console contract](apps-ownsand-console-foundation.md#interfaces-and-contracts)).
- The database migration tool and the backup policy ([control plane contract](servers-ownsand-api-foundation.md#storage)).
- The npm package name and publication workflow of the TypeScript SDK. The repository publishes no packages to a registry today, so the [workflow contract](repository-workflow-contract.md) must change before the first publication.
- The protobuf code generation tool and every new third-party dependency. The root Go module has no third-party dependencies today. OwnSand needs at least Connect, protobuf, a Postgres driver, a WebRTC library (pion), a STUN server library, and hypervisor bindings for Virtualization.framework and Hyper-V. Each dependency needs owner approval and a [dependency security](repository-dependency-security-contract.md) review in the change that adds it.

## Change Policy

- Update this index and every affected domain contract together when component IDs, origins, ownership paths, the authentication exception, the data path or the licensing boundary change.
- Change a `planned` path to an actual path in the same change that creates it. That change also adds the workspace membership, the package scripts and the validation commands that the owning domain contract defines.
- Update the [protos contract](protos-ownsand-api-contract.md) before or with every RPC, message or enum change. Update the SDK, CLI and console contracts when a change affects their surface.
- Update [repository-auth-contract.md](repository-auth-contract.md) when the API key or relay ticket exception changes.
- Update the [documentation catalog](README.md) when contracts are added, renamed or removed.
- Update the [relay server contract](servers-ownsand-relay-foundation.md) and this index together when the traffic placement or the relay server deployment target changes.
- Update `apps/AGENTS.md`, `cmds/AGENTS.md`, `servers/AGENTS.md`, `packages/AGENTS.md` and `protos/AGENTS.md` only when development procedures, directory ownership or development rules change; follow the [instruction-update policy](README.md#instruction-update-policy).
- Record implementation status, validation results and unresolved limits in pull requests, Linear issues and CI logs/artifacts. Distinguish fixture, build and packaging checks from acceptance on real Linux, macOS and Windows devices.

## References

- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [repository-billing-contract.md](repository-billing-contract.md)
- [repository-dependency-security-contract.md](repository-dependency-security-contract.md)
- [repository-environment-contract.md](repository-environment-contract.md)
- [repository-license-contract.md](repository-license-contract.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [project-delino-accounts.md](project-delino-accounts.md)
- [project-template.md](project-template.md)
