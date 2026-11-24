# Project: tide

## Goal

tide runs GitHub Actions jobs, coding-agent sessions and dev sessions on one shared pool of devices that a team already owns. The devices are the devices of an [OwnSand](project-ownsand.md) organization. tide gives every job and every session a fresh OwnSand sandbox, and deletes the sandbox when the work ends.

Problem: a small team has only a few devices. Existing products run either CI jobs or agent sessions on the team's machines, never both. When a team splits its few devices into "CI devices" and "agent devices", one group often waits while the other group is idle. On a Mac this is worse, because one Mac runs at most two macOS guests at the same time. tide keeps one queue for CI jobs and agent sessions, so every request can use every free device.

tide also lets a coding agent develop across platforms. An agent that works on one machine (for example, Claude Code or OpenAI Codex on a Linux machine) uses the `tide mcp` MCP server to create a dev session with another guest OS (for example, Windows or macOS), copy its local working tree into the session, run builds and tests there, and download the results. Dev sessions use the same queue and the same devices as the other work.

tide is a separate service on top of OwnSand. OwnSand stays a general sandbox platform and knows nothing about GitHub or agent vendors. tide holds the GitHub and vendor integrations, the queue and the priority rules.

Success criterion: a tide organization is linked to an OwnSand organization that has one Linux device, one macOS device and one Windows device. On this device pool:

- GitHub Actions jobs with Linux, macOS and Windows guests run in tide runners.
- Sessions of Claude Code, Cursor, OpenAI Codex and Devin run in tide runners on guest OSes that each vendor supports.
- When the free capacity is not sufficient, requests wait in the tide queue and start in priority order when capacity becomes free.

## Project ID

`tide`

## Domain Ownership Map

Every path below exists. A new path starts as `planned` and changes to an actual path in the change that creates it, together with its workspace membership and its validation scripts.

- `servers/tide-api/`: Go control plane on the shared DigitalOcean Droplet, served at `https://tide.api.delino.io`. It owns the queue, the dispatcher, the GitHub App integration, the agent vendor adapters and the Claude Code orchestrator processes.
- `apps/tide/`: Rsbuild React web console (workspace package `tide-console`) served at `https://tide.delino.io`.
- `cmds/tide/`: Go command-line interface. The same binary serves the `tide mcp` MCP server for coding agents.
- `protos/delino/tide/`: the workspace package `@delino/tide-protos` with the Connect RPC schemas in `v1/`, the committed Go bindings and the committed TypeScript bindings.
- `packages/tide-sdk/`: TypeScript SDK (workspace package `tide-sdk`).
- `go/tide/`: Go SDK, with the test fake `go/tide/tidetest`. It is under `go/` because external users import it (see the [workflow contract](repository-workflow-contract.md#public-go-packages)).

The project has one hosted server deployable (the control plane on the shared DigitalOcean Droplet), one static site (the web console) and one distributed program (the CLI). tide distributes no device program: OwnSand owns every program that runs on a device. The project has no `crates` paths.

## Domain Contract Documents

- [servers-tide-api-foundation.md](servers-tide-api-foundation.md): control plane, queue, dispatcher, GitHub App integration and agent vendor adapters.
- [protos-tide-api-contract.md](protos-tide-api-contract.md): Connect RPC services, messages and enums.
- [apps-tide-console-foundation.md](apps-tide-console-foundation.md): web console.
- [cmds-tide-cli-foundation.md](cmds-tide-cli-foundation.md): command-line interface and the `tide mcp` MCP server.
- [packages-tide-sdk-foundation.md](packages-tide-sdk-foundation.md): TypeScript and Go SDKs.

## Cross-Domain Invariants

### Components

| Component ID   | Path                     | Kind                  | Production origin or distribution    |
| -------------- | ------------------------ | --------------------- | ------------------------------------ |
| `tide-api`     | `servers/tide-api/`      | Droplet server        | `https://tide.api.delino.io`         |
| `tide-console` | `apps/tide/`             | Cloudflare Pages site | `https://tide.delino.io`             |
| `tide-cli`     | `cmds/tide/`             | CLI binary            | Release artifact                     |
| `tide-sdk-ts`  | `packages/tide-sdk/`     | TypeScript package    | npm package                          |
| `tide-sdk-go`  | `go/tide/`               | Go package            | Root Go module import path           |
| `tide-protos`  | `protos/delino/tide/v1/` | Schemas and bindings  | Root Go module and generated TS code |

### Layering on OwnSand

- tide uses OwnSand only through the public Connect RPC API of `ownsand-api`, with the [OwnSand Go SDK](packages-ownsand-sdk-foundation.md) and one OwnSand organization API key per tide organization. tide never connects to a device agent and never imports code from `servers/ownsand-api/`.
- OwnSand has no tide-specific behavior. The only OwnSand change that tide needs is the general [vCPU overcommit rule](servers-ownsand-api-foundation.md#vcpu-overcommit). A later tide feature that needs another OwnSand capability must add it to the OwnSand contracts as a general sandbox capability.
- One tide organization links to exactly one OwnSand organization, and one OwnSand organization links to at most one tide organization. Two tide organizations on the same devices would split the queue again, which is the problem that tide solves.
- OwnSand stays the owner of devices, device limits and sandbox placement. tide decides only which request asks OwnSand for a sandbox next. The OwnSand scheduler selects the device.
- Other callers of the same OwnSand organization (people, the OwnSand CLI or other programs) can still create sandboxes. tide counts their resources as used capacity and never deletes their sandboxes.

### Workloads

- Workload kinds use the enum `WorkloadKind`: `GITHUB_JOB`, `AGENT_SESSION` and `DEV_SESSION`.
- Agent vendors use the enum `AgentVendor`: `CLAUDE_CODE`, `CURSOR`, `OPENAI_CODEX` and `DEVIN`.
- A **runner profile** is an admin-defined description of one kind of runner: its name, workload kind, agent vendor (for `AGENT_SESSION`), guest OS, OwnSand source (base image or template), resources, network mode, TTLs, priority, maximum queue wait and warm count. The [control plane contract](servers-tide-api-foundation.md#runner-profiles) owns the fields and their validation.
- A **request** is one unit of demand: one queued GitHub Actions job, one agent session that a vendor asks to run, or one dev session that a tide user asks for.
- A **dev session** is a runner of kind `DEV_SESSION` that one tide user owns. The user, or a coding agent with the user's credential, runs commands in it and copies files into it and out of it through `tide-api`. Unlike `AGENT_SESSION`, no vendor sends the demand and tide starts no runner program in it. The [control plane contract](servers-tide-api-foundation.md#dev-sessions) owns its rules.
- A **runner** is one OwnSand sandbox that tide created for a runner profile. A runner serves at most one request and is never reused. tide deletes the sandbox when the work ends.
- A **warm runner** is a running sandbox of a runner profile that waits for a request without a request assigned. Each runner profile has a warm count, which is `0` by default.

### Scheduling

- tide keeps one queue per tide organization for all three workload kinds (`GITHUB_JOB`, `AGENT_SESSION` and `DEV_SESSION`). Each request waits in it with the priority and maximum queue wait of its runner profile.
- Priorities use the enum `Priority`: `HIGH`, `NORMAL` and `LOW`. A new `GITHUB_JOB` runner profile has `HIGH`, and a new `AGENT_SESSION` or `DEV_SESSION` runner profile has `NORMAL`. An admin can change the priority of each runner profile.
- tide does not preempt work in this version. It never stops, pauses or deletes a runner that serves a request in order to free capacity. tide frees capacity only by deleting warm runners.
- tide uses the vCPU overcommit of OwnSand devices and does not overcommit on its own.
- Each runner profile has a maximum queue wait. A request that waits longer ends with `EXPIRED`. External limits can end a request earlier: GitHub cancels a queued job after 24 hours, and OpenAI waits at most 5 minutes for an executor.

### Protocol and API

- The public API is tide's own Connect RPC API in the protobuf package `delino.tide.v1`. The [protos contract](protos-tide-api-contract.md) owns every service, message and enum.
- List RPCs follow the [protos contract's enum filter rules](protos-tide-api-contract.md#general-rules).
- Public RPCs are unary or server-streaming, like the [OwnSand transport rule](project-ownsand.md#protocol-and-api), so that browser clients can call them.
- Dev session commands and file transfers go through `tide-api`: the caller calls tide RPCs, and `tide-api` calls the OwnSand data path with the organization's OwnSand API key. Callers of tide never get an OwnSand credential and never connect to OwnSand.
- The `tide mcp` MCP server is a local stdio process of the [CLI](cmds-tide-cli-foundation.md#mcp-server). It is a client of the public RPCs and adds no server endpoint. tide has no remote (HTTP) MCP endpoint in this version.
- Inbound webhooks from GitHub and from OpenAI are plain HTTP `POST` endpoints, because those third parties do not call Connect RPC. They are the only plain HTTP endpoints besides operational endpoints. Each webhook endpoint verifies the sender's signature before it reads the payload. The [control plane contract](servers-tide-api-foundation.md#webhooks) owns them.

### Identity and access

- People authenticate with Logto access tokens according to the [authentication contract](repository-auth-contract.md). The Logto user ID (`sub`) is the user ID in every tide table.
- tide stores its own organizations, memberships and invitations, with the same roles and rules as OwnSand: `owner`, `admin` and `member`. tide memberships are separate from OwnSand memberships. The [control plane contract](servers-tide-api-foundation.md#authorization) owns the authorization matrix.
- The CLI, the `tide mcp` MCP server and the SDKs use Logto access tokens or personal API keys.
- **Personal API keys.** A personal API key belongs to one user in one tide organization, for coding agents and other programs that cannot sign in with a browser. It authenticates as that user, but only on the RPCs of `DevSessionService`; every other RPC rejects it. It has an optional expiry, and tide revokes it when the user leaves or is removed from the organization. The [control plane contract](servers-tide-api-foundation.md#personal-api-keys) owns its format, storage and rules.
- **Authentication contract exceptions.** `tide-api` accepts personal API keys on `DevSessionService` RPCs, as described above. `tide-api` also accepts GitHub webhook deliveries signed with the webhook secret of the tide GitHub App, and OpenAI webhook deliveries signed with the webhook secret of an OpenAI Codex integration. A webhook signature authenticates only the delivery of an event from that sender. It gives no access to any RPC. The [authentication contract](repository-auth-contract.md#documented-exceptions) lists these exceptions.
- tide holds credentials that it uses toward other services: one OwnSand API key per tide organization, the GitHub App private key and the agent vendor credentials of each agent integration. tide encrypts organization credentials at rest and never returns them through the API. The [control plane contract](servers-tide-api-foundation.md#secrets) owns their handling.
- Code in a runner can read every credential that tide gives to that runner. tide gives a runner only the credential that its runner process needs, and never the OwnSand API key or the GitHub App private key. tide gives a dev session no credential; it contains only what its owner copies into it or passes to its commands.

### Identifiers and states

- New persisted entities use UUID v7 identifiers: organizations, memberships, invitations, OwnSand links, GitHub installations, agent integrations, runner profiles, requests, runners, personal API keys and audit events. A dev session is identified by the ID of its request.
- Request and runner states use the enums of the [protos contract](protos-tide-api-contract.md#enums). Components must not compare free-form state strings.

### Licensing and code sharing

- `servers/tide-api/` uses `FSL-1.1-ALv2` according to the [license contract](repository-license-contract.md). Every other tide path uses Apache-2.0.
- The CLI and the SDKs must not import code from `servers/`. Code that the control plane shares with them lives in `protos/delino/tide/v1/` or `go/tide/`.

### Repository rules that apply without change

- Account management links to `https://accounts.delino.io`. tide builds no profile or account settings pages.
- tide does not charge users in this version. Billing and usage reporting to `delino-accounts` are out of scope. A later change that adds charges must use `delino-accounts` according to the [billing contract](repository-billing-contract.md).
- Logs use `log/slog` in Go. No log record or audit event contains secrets, tokens, credentials, JIT runner configurations, vendor work orders, commands that carry them, or runner output. For dev sessions, no log record or audit event contains command arguments, environment values, command output or file contents.
- tide needs no feature flag in this version. It is a new project without existing users or behavior to protect, and the OwnSand vCPU overcommit starts at `100` (no overcommit) on every device. Dev sessions are part of the first version and need no flag for the same reason.

### Deviations from repository defaults

- Inbound webhooks from GitHub and OpenAI use plain HTTP endpoints, as described in [Protocol and API](#protocol-and-api).
- The personal API key and webhook signature exceptions, as described in [Identity and access](#identity-and-access).
- Dev session file transfers pass through `tide-api` on the shared Droplet and then through the OwnSand data path, so they use Droplet bandwidth. This is accepted in this version, because tide callers must not get OwnSand credentials. The transfer limits in the [control plane contract](servers-tide-api-foundation.md#dev-session-file-transfer) bound one transfer.

### Platform and vendor constraints

- GitHub: only github.com and GitHub Enterprise Cloud organizations. tide registers organization-level runners only, so repositories of personal accounts and GitHub Enterprise Server are out of scope.
- Claude Code self-hosted runners run on Linux and macOS, not on Windows. Self-hosted environments are a beta feature of the Claude Team and Enterprise plans. A Claude Code work order is single-use and expires with its spawn request, so tide hands it to at most one runner and does not keep a request in the queue after the work order expired; Anthropic then sends a new spawn request ([control plane contract](servers-tide-api-foundation.md#claude-code)).
- Cursor Self-Hosted Pools need a Cursor Enterprise plan and a service account API key.
- Devin Outposts need a Devin plan that includes Outposts.
- OpenAI Codex self-hosted sandboxes need an OpenAI environment key, an OpenAI webhook for the project and an OpenAI API key of the project with `api.agents.read`: the webhook carries only the session ID, so tide reads the session's environment with that key. The API key and the webhook secret stay in tide; only the environment key enters a runner.
- macOS guests need an Apple silicon device, and one Mac runs at most two macOS guests at the same time. The OwnSand scheduler enforces this limit for tide too.
- Delino distributes no macOS or Windows image. Runner profiles use OwnSand base images or OwnSand templates that the organization builds.

### Recorded decisions

The owning domain contracts hold the details:

- Metrics: the default retention of the Grafana Cloud plan, one tide dashboard committed as Grafana dashboard JSON, and Grafana Cloud alerts to the operators' contact point ([control plane contract](servers-tide-api-foundation.md#metrics)).
- Runner programs: tide installs them on the guest OSes that each workload kind and vendor allows (Linux, macOS and Windows; Claude Code and OpenAI Codex on Linux and macOS only). It pins the GitHub Actions runner, `codex` and Node.js, and Renovate updates the pins. It installs `claude`, Cursor `agent` and `devin` at the version of the official installer, with a minimum version only for `claude` ([control plane contract](servers-tide-api-foundation.md#runner-installation)).
- Storage: ended requests and runners are kept for 30 days and audit events for 90 days, the migration tool is goose, and the backups are the PlanetScale Postgres defaults ([control plane contract](servers-tide-api-foundation.md#storage)).
- Rate limits: the same values as OwnSand ([control plane contract](servers-tide-api-foundation.md#security)).
- TypeScript SDK package: the workspace package `tide-sdk` is private and unpublished, like `ownsand-sdk` ([SDK contract](packages-tide-sdk-foundation.md#dependencies-and-integrations)).

### Open decisions

Record each decision in the owning domain contract before the implementation that depends on it. The cross-domain ones are:

- The exact install commands per guest OS of each runner program. The runner implementation records them with the guest OSes on which it verified them ([control plane contract](servers-tide-api-foundation.md#runner-installation)).
- The npm package name and publication workflow of the TypeScript SDK, together with the same open decision of OwnSand.
- Every new third-party dependency. Each dependency needs owner approval and a [dependency security](repository-dependency-security-contract.md) review in the change that adds it. Recorded owner approval: the official MCP Go SDK `github.com/modelcontextprotocol/go-sdk` for the `tide mcp` server. Recorded decision: the root `go.mod` pins v1.8.0, the latest stable release when it was added. The change that adds it records the dependency security review (the Go vulnerability database and the GitHub advisories of the module). The module is licensed under MIT and Apache-2.0, and its new indirect modules are licensed under MIT, MIT-0 or BSD-3-Clause.

## Change Policy

- Update this index and every affected domain contract together when component IDs, origins, ownership paths, the authentication exception, the layering on OwnSand or the licensing boundary change.
- Change a `planned` path to an actual path in the same change that creates it. That change also adds the workspace membership, the package scripts and the validation commands that the owning domain contract defines.
- Update the [protos contract](protos-tide-api-contract.md) before or with every RPC, message or enum change. Update the SDK, CLI and console contracts when a change affects their surface.
- Update the OwnSand contracts first when tide needs a new OwnSand capability, and keep that capability general.
- Update [repository-auth-contract.md](repository-auth-contract.md) when the personal API key or webhook signature exceptions change.
- Update the [documentation catalog](README.md) when contracts are added, renamed or removed.
- Update `apps/AGENTS.md`, `cmds/AGENTS.md`, `servers/AGENTS.md`, `packages/AGENTS.md` and `protos/AGENTS.md` only when development procedures, directory ownership or development rules change; follow the [instruction-update policy](README.md#instruction-update-policy).
- Record implementation status, validation results and unresolved limits in pull requests, Linear issues and CI logs/artifacts. Distinguish fixture, build and packaging checks from acceptance with real GitHub organizations, real vendor accounts and real Linux, macOS and Windows devices.

## References

- [project-ownsand.md](project-ownsand.md)
- [servers-ownsand-api-foundation.md](servers-ownsand-api-foundation.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [repository-billing-contract.md](repository-billing-contract.md)
- [repository-dependency-security-contract.md](repository-dependency-security-contract.md)
- [repository-environment-contract.md](repository-environment-contract.md)
- [repository-license-contract.md](repository-license-contract.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [project-template.md](project-template.md)
- [GitHub REST API: self-hosted runners](https://docs.github.com/en/rest/actions/self-hosted-runners)
- [GitHub webhook events: `workflow_job`](https://docs.github.com/en/webhooks/webhook-events-and-payloads#workflow_job)
- [Claude Code self-hosted environments](https://code.claude.com/docs/en/self-hosted-environments)
- [Cursor self-hosted machines](https://cursor.com/docs/cloud-agent/self-hosted)
- [OpenAI self-hosted sandboxes](https://developers.openai.com/api/docs/guides/agents-api/environments/self-hosted)
- [Devin Outposts](https://docs.devin.ai/cloud/outposts/overview)
- [Model Context Protocol](https://modelcontextprotocol.io/)
- [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
