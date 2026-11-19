# Project: ownsand

## Goal

OwnSand turns devices that a team already owns into a private sandbox cloud. A team registers its Linux, macOS and Windows devices in an OwnSand organization. Members and programs then create, use and delete isolated sandboxes on those devices through one Connect RPC API. They do not need a third-party sandbox provider, and sandbox data stays on the team's own hardware.

Success criterion: on one Linux, one macOS and one Windows device, an organization member runs the full sandbox lifecycle through the SDK. The lifecycle is create, exec, file read and write, pause, resume, snapshot and delete.

## Project ID

`ownsand`

## Domain Ownership Map

All paths are `planned`. This project is in the documentation-only phase. Create each path, its workspace membership and its validation scripts in the change that starts its runtime implementation.

- `servers/ownsand-api/` (`planned`): Go control plane served at `https://ownsand.api.delino.io`.
- `cmds/ownsand-agent/` (`planned`): Go device agent that runs on each enrolled host.
- `cmds/ownsand-guest/` (`planned`): Go helper that runs inside each sandbox guest and serves exec and file requests from the device agent.
- `cmds/ownsand/` (`planned`): Go command-line interface for people and programs.
- `apps/ownsand/` (`planned`): Rsbuild React web console served at `https://ownsand.delino.io`.
- `protos/delino/ownsand/v1/` (`planned`): Connect RPC schemas and the committed Go bindings.
- `packages/ownsand-sdk/` (`planned`): TypeScript SDK.
- `packages/ownsand-sdk-go/` (`planned`): Go SDK.

The project has one hosted deployable (the control plane), one static site (the web console) and three distributed programs (the device agent, the guest helper and the CLI). The project has no `crates` paths.

## Domain Contract Documents

- [servers-ownsand-api-foundation.md](servers-ownsand-api-foundation.md): control plane.
- [cmds-ownsand-agent-foundation.md](cmds-ownsand-agent-foundation.md): device agent, guest helper, isolation, networking and agent updates.
- [cmds-ownsand-cli-foundation.md](cmds-ownsand-cli-foundation.md): command-line interface.
- [apps-ownsand-console-foundation.md](apps-ownsand-console-foundation.md): web console.
- [protos-ownsand-api-contract.md](protos-ownsand-api-contract.md): Connect RPC services, messages and the data path framing.
- [packages-ownsand-sdk-foundation.md](packages-ownsand-sdk-foundation.md): TypeScript and Go SDKs.

## Cross-Domain Invariants

### Components

| Component ID      | Path                        | Kind                  | Production origin or distribution    |
| ----------------- | --------------------------- | --------------------- | ------------------------------------ |
| `ownsand-api`     | `servers/ownsand-api/`      | Railway server        | `https://ownsand.api.delino.io`      |
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
- Railway platform facts: Railway's published limits state the 5-minute and 15-minute request limits and the WebSocket exemption. They do not state the HTTP version between the edge and the container or the absence of public UDP. Railway community reports and templates say that the edge forwards HTTP/1.1 to the container and that public networking exposes no UDP ports. The contracts treat both as constraints; verify them with Railway in the change that records the agent channel transport and the STUN deployment. Sources: [specs and limits](https://docs.railway.com/networking/public-networking/specs-and-limits), [HTTP/2 edge feedback](https://station.railway.com/feedback/http-2-support-on-edge-proxy-50adedfe) and [a WebRTC template that uses TCP because of the missing UDP](https://railway.com/deploy/livekit-voice-agent-self-hosted-webrtc-ai-on-railway--livekit-voice-agent).
- The Railway public edge forwards HTTP/1.1 to the container (see the platform facts above). Public RPCs that callers reach through `https://ownsand.api.delino.io` must therefore be unary or server-streaming. Bidirectional RPCs need HTTP/2 from end to end and must not be used on that route. Client-streaming RPCs are not used either, because browser clients cannot send them. The edge also closes a request that transfers no data for 5 minutes or that runs for more than 15 minutes, so long-lived relay streams send keepalive events and callers resume them, according to the [protos contract](protos-ownsand-api-contract.md#exec). The [control plane contract](servers-ownsand-api-foundation.md#interfaces-and-contracts) records the transport of the long-lived agent channel.
- Exec streams and file transfers use a peer-to-peer WebRTC data channel between the caller (SDK or CLI) and the device agent when ICE succeeds. The control plane does the signaling. When the peer-to-peer connection fails, the caller uses the relay RPCs of the control plane, which forward the same frames to the agent. The relay does not store payloads.

### Identity and access

- People authenticate with Logto access tokens according to the [authentication contract](repository-auth-contract.md). The Logto user ID (`sub`) is the user ID in every OwnSand table.
- OwnSand stores organizations, memberships and invitations in its own tables. It does not use Logto Organizations.
- Roles use the enum identifiers `owner`, `admin` and `member`. `admin` includes every permission of `member`, and `owner` every permission of `admin`, except that the `owner` cannot leave the organization without first transferring ownership. The [control plane contract](servers-ownsand-api-foundation.md#authorization) owns the authorization matrix.
- **Authentication contract exception.** Programs (SDK, CI and AI agents) authenticate with organization API keys that admins issue and revoke. Device agents enroll with one-time enrollment tokens that admins issue: a token expires within 24 hours and creates exactly one device in one organization. An API key is a credential of the organization, not a user identity. It is scoped to exactly one organization and has a subset of the `member` permissions: it can use devices, sandboxes, snapshots and templates, but it cannot read the organization or its members. The [authorization matrix](servers-ownsand-api-foundation.md#authorization) defines the exact operations. This is a project-level exception to the rule that backend services accept only Logto access tokens. It does not add an identity provider, a password store or a sign-in flow for people. The [authentication contract](repository-auth-contract.md#documented-exceptions) lists this exception.
- Device agents authenticate with a device key pair that the agent generates locally during enrollment. Device credentials are not user credentials and grant access only to the agent channel of their own device.
- The server stores only hashes of enrollment tokens, invitation secrets, API keys and device session credentials. It shows each secret once, at issue time.
- A user, an API key or a device can never see or change the devices, sandboxes, snapshots, templates, members or API keys of another organization.

### Identifiers and states

- New persisted entities use UUID v7 identifiers: organizations, memberships, invitations, devices, enrollment tokens, API keys, sandboxes, snapshots, templates and audit events. Callers generate the `exec_id`, `read_id` and `write_id` values as UUID v7; the control plane does not persist them and does not trust them as server identifiers.
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
- STUN needs public UDP. Railway public networking does not expose UDP ports (see the [platform facts](#protocol-and-api)), so the STUN service cannot run on Railway. Its deployment target is an open decision and becomes a documented deviation from the Railway default when it is selected.

### Platform and license constraints

- Linux hosts need KVM access for the microVM runtime.
- macOS guests need an Apple silicon host. Virtualization.framework runs at most two macOS guests at the same time on one Mac. Linux guests do not count toward this limit.
- Windows hosts need Hyper-V, which Windows 11 Pro, Enterprise and Education and Windows Server include. Windows 11 Home does not include Hyper-V.
- Delino does not distribute macOS or Windows images. Each device builds them locally from an Apple IPSW or from a Windows ISO that an admin supplies. The organization is responsible for the operating system license terms of its guests.

### Open decisions

Record each decision in the owning domain contract before the implementation that depends on it:

- The release signing tool and the key custody process ([agent contract](cmds-ownsand-agent-foundation.md#agent-updates)).
- The microVM implementation for Linux hosts (Firecracker class) ([agent contract](cmds-ownsand-agent-foundation.md#isolation)).
- The STUN deployment and the relay deployment ([control plane contract](servers-ownsand-api-foundation.md#data-path-signaling-and-relay)).
- The transport of the long-lived agent channel through the Railway edge ([control plane contract](servers-ownsand-api-foundation.md#agent-channel)).
- The per-device admin settings UI ([console contract](apps-ownsand-console-foundation.md#interfaces-and-contracts)).
- The database migration tool and the backup policy ([control plane contract](servers-ownsand-api-foundation.md#storage)).
- The protobuf code generation tool and every new third-party dependency. The root Go module has no third-party dependencies today. OwnSand needs at least Connect, protobuf, a Postgres driver, a WebRTC library (pion), and hypervisor bindings for Virtualization.framework and Hyper-V. Each dependency needs owner approval and a [dependency security](repository-dependency-security-contract.md) review in the change that adds it.

## Change Policy

- Update this index and every affected domain contract together when component IDs, origins, ownership paths, the authentication exception, the data path or the licensing boundary change.
- Change a `planned` path to an actual path in the same change that creates it. That change also adds the workspace membership, the package scripts and the validation commands that the owning domain contract defines.
- Update the [protos contract](protos-ownsand-api-contract.md) before or with every RPC, message or enum change. Update the SDK, CLI and console contracts when a change affects their surface.
- Update [repository-auth-contract.md](repository-auth-contract.md) when the API key exception changes.
- Update the [documentation catalog](README.md) when contracts are added, renamed or removed.
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
