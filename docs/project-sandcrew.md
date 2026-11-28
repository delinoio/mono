# Project: sandcrew

## Goal

SandCrew is a coding-agent session product. A member of a [SandRun](project-sandrun.md) organization starts an agent session in the SandCrew console, talks to the agent in a chat view, and the agent works in a sandbox. A session is only a session: SandCrew requires no result such as a branch or a pull request.

SandCrew does not implement an agent harness (agent loop, tools, context compaction or model calls). It runs the agents of the vendors through the [Agent Client Protocol](https://agentclientprotocol.com/) (ACP), and it uses SandRun dev sessions as its executor. A session needs only a vendor credential of the user or the organization (an API key or a subscription token), not an Enterprise, Team or Outposts plan of the vendor.

Problem: the SandRun `AGENT_SESSION` workload is vendor-initiated. The vendor sends the demand, and the session UI and the harness stay at the vendor. Each of these integrations needs an upper-tier vendor plan. SandCrew is user-initiated: the user starts the session in SandCrew, and SandCrew shows the transcript.

The first version (MVP) supports Claude Code and OpenAI Codex on Linux, macOS and Windows guests. Cursor and Devin come after the MVP. Other ACP agents can be added with a new vendor value, a pinned agent installation in the bridge and no server change.

Success criterion (feature completion): an organization member creates a Claude Code session and an OpenAI Codex session from the console, talks to the agent, pauses and resumes the session and deletes it, verified with automated tests against fakes. Acceptance on real devices and with real vendor accounts is not part of the MVP; pull requests and Linear issues record it as not performed.

## Project ID

`sandcrew`

## Domain Ownership Map

A `planned` path changes to an actual path in the change that creates it, together with its workspace membership and its validation scripts.

- `servers/sandcrew-api/` (`planned`): Go control plane on the shared DigitalOcean Droplet, served at `https://sandcrew.api.delino.io`. It owns sessions, transcripts, vendor credentials, session secrets, the organization settings of SandCrew, the event collector and Web Push notifications.
- `apps/sandcrew/`: Rsbuild React web console (workspace package `sandcrew-console`) served at `https://sandcrew.delino.io`.
- `cmds/sandcrew-bridge/`: Go guest program (the bridge) that SandCrew copies into each session sandbox. It starts the vendor agent, speaks ACP with it over local stdio, reads input from an inbox directory and writes normalized events.
- `protos/delino/sandcrew/`: the workspace package `@delino/sandcrew-protos` with the Connect RPC schemas and the bridge event schema in `v1/`, the committed Go bindings and the committed TypeScript bindings.

The project has one hosted server deployable (the control plane), one static site (the console) and one distributed program (the bridge, which SandCrew distributes itself into session sandboxes). The project has no `crates` or `packages` paths.

## Domain Contract Documents

- [servers-sandcrew-api-foundation.md](servers-sandcrew-api-foundation.md): control plane, storage, event collector, transcripts, retention, membership reconciliation, credentials, notifications and observability.
- [apps-sandcrew-console-foundation.md](apps-sandcrew-console-foundation.md): web console.
- [cmds-sandcrew-bridge-foundation.md](cmds-sandcrew-bridge-foundation.md): guest bridge, inbox, event log, auto-approval, pinned agent installation and protocol versions.
- [protos-sandcrew-api-contract.md](protos-sandcrew-api-contract.md): Connect RPC services, the bridge event and input schema, and the enums.

## Cross-Domain Invariants

### Components

| Component ID       | Path                         | Kind                  | Production origin or distribution                        |
| ------------------ | ---------------------------- | --------------------- | -------------------------------------------------------- |
| `sandcrew-api`     | `servers/sandcrew-api/`      | Droplet server        | `https://sandcrew.api.delino.io`                         |
| `sandcrew-console` | `apps/sandcrew/`             | Cloudflare Pages site | `https://sandcrew.delino.io`                             |
| `sandcrew-bridge`  | `cmds/sandcrew-bridge/`      | Guest program binary  | Embedded in `sandcrew-api`, copied into each session     |
| `sandcrew-protos`  | `protos/delino/sandcrew/v1/` | Schemas and bindings  | Root Go module and the `@delino/sandcrew-protos` package |

### Layering

```
OwnSand  : sandboxes (knows nothing about GitHub or agent vendors)
SandRun  : queue, scheduling, CI jobs, vendor agent sessions, dev sessions (knows nothing about ACP or transcripts)
SandCrew : ACP sessions, transcripts, chat UI
```

- SandCrew uses SandRun only through the public SandRun Connect RPC API, in the same way that SandRun uses OwnSand only through the public OwnSand API. SandCrew never calls OwnSand, never gets an OwnSand credential and never imports code from `servers/sandrun-api/` or `servers/ownsand-api/`.
- SandCrew runs each agent session in one SandRun `DEV_SESSION`. SandCrew sessions use the same SandRun queue and the same devices as CI jobs and other sessions. SandRun gets no ACP, transcript or chat behavior.
- A SandRun capability that SandCrew needs is added to the SandRun contracts as a general dev session capability. The capabilities that SandCrew needs are delegated authentication with the scope `sandrun:act-for-user`, [dev session pause and resume](servers-sandrun-api-foundation.md#dev-session-pause-and-resume) and [dev sessions without a maximum lifetime](servers-sandrun-api-foundation.md#runner-profiles).
- SandRun in turn needs these general OwnSand capabilities: a [memory-state pause that releases vCPU and memory](servers-ownsand-api-foundation.md#memory-state-pause), [paused sandboxes that survive agent restarts](cmds-ownsand-agent-foundation.md#memory-state-pause) and an [optional `max_ttl`](servers-ownsand-api-foundation.md#ttl). Windows guests also need the `OUTBOUND` network mode, because every agent calls its model API.
- SandRun's rule that it distributes no device program stays unchanged. SandCrew owns, builds, distributes and copies the bridge.

### Executor mapping

| Need                                              | SandRun RPC                                                                              |
| ------------------------------------------------- | ---------------------------------------------------------------------------------------- |
| Queue a sandbox with a priority                   | `CreateDevSession`, `GetDevSession`                                                      |
| Install the bridge                                | `WriteDevSessionFile`, then `ExecDevSession`                                             |
| Pass the model credential and the session secrets | `ExecDevSession` environment and `WriteDevSessionFile` (SandRun stores and logs neither) |
| Read events                                       | `AttachDevSessionExec`, with `ReadDevSessionFile` of the guest event log to recover      |
| Send input                                        | `WriteDevSessionFile` into the bridge inbox                                              |
| Pause and resume                                  | `PauseDevSession`, `ResumeDevSession`                                                    |
| End the session                                   | `DeleteDevSession`                                                                       |

### Sessions

- A session starts with an empty workspace (the `workspace_path` of its runner profile). The agent clones repositories and does other work only when the prompt tells it to. SandCrew has no pull request flow and no GitHub integration in the MVP.
- A session has no maximum lifetime. It continues until its owner deletes it, or until it ends with one of the reasons below.
- Session states use the enum `SessionState`: `QUEUED`, `STARTING`, `RUNNING`, `WAITING_FOR_INPUT`, `PAUSING`, `PAUSED`, `RESUME_QUEUED`, `RESUMING`, `DISCONNECTED` and `ENDED`. Only `ENDED` is terminal. A deleted session is removed, so it has no state.
- End reasons use the enum `SessionEndReason`: `START_FAILED`, `QUEUE_EXPIRED`, `AGENT_EXITED`, `DEVICE_LOST`, `RESTORE_FAILED`, `PAUSED_RETENTION_EXPIRED`, `BRIDGE_UNSUPPORTED` and `EXECUTOR_ENDED`. The [control plane contract](servers-sandcrew-api-foundation.md#session-lifecycle) owns the mapping from SandRun states and end reasons.
- The transcript of an ended session stays readable. Its owner can start a new session from it ("Start new session from transcript"): SandCrew writes the previous transcript as a readable file `.sandcrew/previous-session.md` in the new workspace, and the first prompt says where the file is and that the workspace is empty, followed by the user's message. SandCrew has no summarization logic.

### Pause and resume

- When the agent waits for user input, SandCrew pauses the session after the organization's pause delay (default 10 minutes). A paused session holds no vCPU or memory on its device, only disk, and its agent process continues with its state after the resume.
- A message or a permission answer to a paused session starts the resume. The resume waits in the SandRun queue with the priority of the session's runner profile, for capacity on the device that holds the sandbox.
- A paused session survives an agent restart, an agent update and a host reboot of its device.
- Device temporarily offline: the session shows `DISCONNECTED` and waits for the device. Device lost or restore failed: the session ends with `DEVICE_LOST` or `RESTORE_FAILED`.
- Paused-session retention: an organization setting, default 30 days, with "unlimited" as an allowed value. When it ends, SandCrew deletes the sandbox and ends the session with `PAUSED_RETENTION_EXPIRED`.

### Protocol and API

- The public API is SandCrew's own Connect RPC API in the protobuf package `delino.sandcrew.v1`. The [protos contract](protos-sandcrew-api-contract.md) owns every service, message and enum, including the bridge event and input schema.
- Public RPCs are unary or server-streaming, like the [SandRun rule](project-sandrun.md#protocol-and-api), so that browser clients can call them. The console uses React Query with `@connectrpc/connect-query`.
- The bridge and the control plane exchange only the bridge event and input schema of the protos contract: events as JSON lines on the stdout of one long-running dev session exec and in an append-only guest log, and input as files in a guest inbox directory. The bridge never connects to the control plane and holds no SandCrew credential.
- The bridge sends its protocol version in its first event. Only an incompatible change of the event or input schema gets a new protocol major version. The control plane supports every protocol major version that a live session uses. SandCrew never replaces the bridge or the agent of an existing session.
- Vendors without ACP are wrapped by the bridge (headless CLI mode), so the control plane knows only one protocol.

### Identity and access

- People authenticate with Logto access tokens for the API resource `https://sandcrew.api.delino.io` according to the [authentication contract](repository-auth-contract.md). The Logto user ID (`sub`) is the user ID in every SandCrew table.
- SandCrew works inside SandRun organizations. It stores no organizations, memberships, roles or invitations. Members are managed only in SandRun, and the console links to the SandRun console for member management. SandCrew stores its own settings per SandRun organization ID.
- `sandcrew-api` calls `sandrun-api` with a Logto machine-to-machine access token with the scope `sandrun:act-for-user` and names the acting user in the request header `Delino-Acting-User`. SandRun authorizes that user as if the user called, records the user as the dev session owner, applies the per-user limits and records the user and the calling service in its audit events. The [authentication contract](repository-auth-contract.md#service-to-service-calls) records the scope and the on-behalf-of rule.
- Every SandCrew request checks the caller's SandRun membership and role (with a cache of at most 60 seconds). A reconciliation job deletes the data of users who left an organization and of deleted organizations. The [control plane contract](servers-sandcrew-api-foundation.md#membership) owns the cache and the job.
- Only the session owner reads a session's transcript and sends input to it. Other members of the organization see only session metadata (state, vendor, label, owner and times), like the SandRun dev session rule. `owner` and `admin` members of the SandRun organization change the SandCrew organization settings and the organization vendor credentials. Every member registers personal vendor credentials and session secrets and runs their own sessions.
- SandCrew adds no exception to the authentication contract.

### Permissions and the security boundary

- New sessions auto-approve agent permission requests. The bridge starts the vendor's auto-approve mode where one exists and answers every remaining ACP permission request with an allow option. The transcript records each auto-approved request.
- The user can switch the session to an ask mode. Permission requests then appear in the chat, and an unanswered request counts as waiting for input.
- Permission prompts are advisory. The bridge and the agent run in the same sandbox, so an agent with shell access can bypass them. The sandbox is the security boundary.

### Credentials and secrets

- Vendor credentials exist per organization (registered by `owner` and `admin`) and per user. A user credential replaces the organization credential of the same vendor for that user's sessions. API keys and subscription tokens are allowed for every vendor: the Claude Code token of `claude setup-token`, and the OpenAI Codex ChatGPT sign-in file `auth.json`.
- Session secrets are secret environment variables that a user stores per organization. The new-session form selects which secrets the session gets; none is preselected.
- `sandcrew-api` encrypts every credential and secret with AES-256-GCM with keys from `SANDCREW_SECRET_KEYS` (the format and rotation rules of `SANDRUN_SECRET_KEYS`). The API is write-only. Values never appear in logs, events, transcripts or analytics.
- Code in a session sandbox can read every credential and secret that SandCrew gives to that session. SandCrew gives a session only its model credential and its selected secrets, and never a SandCrew, SandRun or OwnSand credential.

### Identifiers and states

- New persisted entities use UUID v7 identifiers: sessions, vendor credentials, session secrets, transcript chunks, push subscriptions and inbox messages. The SandRun organization ID and the SandRun dev session ID are stored as references.
- States, end reasons, vendors, credential kinds and every event kind use the enums of the [protos contract](protos-sandcrew-api-contract.md#enums). Components must not compare free-form state strings.

### Licensing and code sharing

- `servers/sandcrew-api/` uses `FSL-1.1-ALv2` according to the [license contract](repository-license-contract.md). Every other SandCrew path uses Apache-2.0, including the bridge (`cmds/sandcrew-bridge/` is not an FSL-1.1-ALv2 directory).
- The bridge and the console must not import code from `servers/`. Code that the control plane shares with the bridge lives in `protos/delino/sandcrew/v1/`, `go/uuidv7/` or a root [shared internal Go package](repository-workflow-contract.md#shared-internal-go-packages).

### Repository rules that apply without change

- Account management links to `https://accounts.delino.io`. SandCrew builds no profile or account settings pages.
- SandCrew does not charge users in the MVP and reports no usage to `delino-accounts`. OwnSand already charges the organization for sandbox usage, and vendors charge model usage to the user's credential. A later change that adds charges uses `delino-accounts`, USD and `_usd_micros` amounts according to the [billing contract](repository-billing-contract.md).
- Logs use `log/slog` in Go with OpenTelemetry to Grafana Cloud. No log record, metric label, audit event or analytics event contains tokens, credentials, secrets, prompts, agent output, tool output or file contents.
- Console UX follows the Toss Design Guidelines, uses `delino-ui` components with light and dark themes, closes dialogs with `Esc` and focuses the single critical input of a form. Browser end-to-end tests follow the [e2e contract](repository-e2e-contract.md).
- Product analytics use PostHog Cloud, server-side only ([control plane contract](servers-sandcrew-api-foundation.md#product-analytics)).
- SandCrew has no public user documentation in `apps/delino-docs` in the MVP.
- SandCrew needs no feature flag. It is a new product with a new console, so no existing behavior needs protection. The SandRun and OwnSand changes are additive and backward compatible, and only the new scope and new runner profiles use them.

### Deviations from repository defaults

- Transcript chunks in R2 are read and written only by `sandcrew-api`, without signed URLs. Transcripts are small, and every read needs the owner check of the session.
- Data path: events go guest → OwnSand → `sandrun-api` → `sandcrew-api`. Transcripts are small, so this is accepted. Large data such as repository checkouts must use `git clone` (or another download) in the guest.

### Platform and vendor constraints

- A session is bound to one device. If that device is removed, or the saved state cannot be restored, the workspace is lost and only the transcript remains. SandCrew does not move a session to another device and does not restore memory state on another device.
- SandRun does not preempt work. A running session holds device capacity until it pauses or ends; a paused session holds only disk.
- Windows guests need the OwnSand `OUTBOUND` network mode. Until OwnSand supports it, SandCrew cannot use Windows runner profiles; the settings page shows them as not selectable.
- Claude Code: the token of `claude setup-token` is valid for one year and has no refresh. OpenAI Codex: the ChatGPT sign-in `auth.json` is refreshed by Codex during use; OpenAI recommends API keys for automation.

### Open decisions

Record each decision in the owning domain contract before the implementation that depends on it. The cross-domain ones are:

- Per-hypervisor verification of the memory-state save and restore after an agent restart and a host reboot (Firecracker, Virtualization.framework and Hyper-V). The [agent contract](cmds-ownsand-agent-foundation.md#memory-state-pause) records the result.
- Whether a Codex `auth.json` refresh in one session invalidates the token in other sessions that use the same credential. The [bridge contract](cmds-sandcrew-bridge-foundation.md#credential-updates) records the result.
- The exact headless fallback commands of a vendor without ACP. The [bridge contract](cmds-sandcrew-bridge-foundation.md#vendors) records them when such a vendor is added; it already records the ACP adapter of each MVP vendor, its guest OSes, its auto-approve and asking modes and its `session/load` and `session/resume` support.
- Alert thresholds. The [control plane contract](servers-sandcrew-api-foundation.md#metrics-and-alerts) records them after internal usage data exists.
- Every new third-party dependency (for example, a Web Push library or an R2 client). Each needs owner approval and a [dependency security](repository-dependency-security-contract.md) review in the change that adds it.

## Change Policy

- Update this index and every affected domain contract together when component IDs, origins, ownership paths, the layering, the executor mapping or the licensing boundary change.
- Change a `planned` path to an actual path in the same change that creates it. That change also adds the workspace membership, the package scripts, the validation commands that the owning domain contract defines, and the development ports in the [environment contract](repository-environment-contract.md#process-boundaries).
- Update the [protos contract](protos-sandcrew-api-contract.md) before or with every RPC, message, event or enum change, and decide whether the change needs a new bridge protocol major version.
- Update the SandRun contracts first when SandCrew needs a new SandRun capability, and keep that capability general. Update the OwnSand contracts first when that capability needs OwnSand.
- Update [repository-auth-contract.md](repository-auth-contract.md) when the delegated scope, its operations or its consuming services change.
- Update the [documentation catalog](README.md) when contracts are added, renamed or removed.
- Update `apps/AGENTS.md`, `cmds/AGENTS.md`, `servers/AGENTS.md` and `protos/AGENTS.md` only when development procedures, directory ownership or development rules change; follow the [instruction-update policy](README.md#instruction-update-policy).
- Record implementation status, validation results and unresolved limits in pull requests, Linear issues and CI logs/artifacts. Distinguish fake-based tests from acceptance with real devices and real vendor accounts.

## References

- [project-sandrun.md](project-sandrun.md)
- [project-ownsand.md](project-ownsand.md)
- [servers-sandrun-api-foundation.md](servers-sandrun-api-foundation.md)
- [protos-sandrun-api-contract.md](protos-sandrun-api-contract.md)
- [servers-ownsand-api-foundation.md](servers-ownsand-api-foundation.md)
- [cmds-ownsand-agent-foundation.md](cmds-ownsand-agent-foundation.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [repository-billing-contract.md](repository-billing-contract.md)
- [repository-dependency-security-contract.md](repository-dependency-security-contract.md)
- [repository-e2e-contract.md](repository-e2e-contract.md)
- [repository-environment-contract.md](repository-environment-contract.md)
- [repository-license-contract.md](repository-license-contract.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [project-template.md](project-template.md)
- [ACP agents directory](https://agentclientprotocol.com/get-started/agents)
- [ACP registry](https://agentclientprotocol.com/get-started/registry)
- [ACP session setup](https://agentclientprotocol.com/protocol/session-setup)
- [ACP schema](https://agentclientprotocol.com/protocol/schema)
