# servers-tide-api foundation contract

## Scope

This contract owns the `tide-api` control plane of [`tide`](project-tide.md) at `servers/tide-api/`. The control plane is the system of record for tide organizations, memberships, OwnSand links, GitHub installations, agent integrations, runner profiles, requests, runners, personal API keys and audit events. It receives demand from GitHub, from the agent vendors and from users who ask for dev sessions, keeps one queue per organization, creates and deletes runners through the OwnSand API, and starts the runner programs in them. For dev sessions, it relays the owner's commands and file transfers to the OwnSand data path.

The control plane runs no sandbox and stores no runner output, dev session command output or dev session file contents. It is licensed under `FSL-1.1-ALv2` according to the [license contract](repository-license-contract.md).

Layout, following the `accounts-api` server conventions:

- `servers/tide-api/cmd/tide-api/`: process entry point. The same binary also serves the Claude Code `spawn-runner` hook command (`planned`).
- `servers/tide-api/internal/`: `config` (configuration), `server` (the HTTP handler, `GET /healthz` and CORS), `api` (the Connect handlers, the authentication interceptor, authorization, personal API keys, audit events, rate limits and the extension hooks), `auth` (Logto access tokens and the `tide_` and `tdi_` secrets), `authz` (the authorization matrix), `secrets` (credential encryption), `ratelimit`, `lease` (the instance lease and the lease-scoped components), `jobs` (the daily jobs), `db` (the Postgres pool and the migrations, with the `dbtest` harness), `app` (the wiring and the extension registry), `layers` (the extensions in their registration order), `ownsandclient` (the OwnSand client interface over the OwnSand Go SDK, its instrumentation and the `ownsandfake` test fake), `link` (the OwnSand link), `integration` (agent integrations), `profile` (runner profiles), `work` (the queue, the dispatcher, the runner lifecycle, the workload adapter interface and `WorkService`), `devsession` (`DevSessionService`, the dev session relay and the `DEV_SESSION` adapter), `metrics` and `testenv` (in-process integration tests). The GitHub App client, the vendor adapters and the webhooks are `planned`.
- `servers/tide-api/package.json`, `turbo.json`, `.env.example` and `Dockerfile`: workspace wiring, the `deploy` field and the container image.

## Runtime and Language

- Language: Go in the root module `github.com/delinoio/mono`. Import path: `github.com/delinoio/mono/servers/tide-api/...`. Logging: `log/slog`.
- Runtime: one long-running process. It serves HTTP with Connect handlers and the webhook endpoints on the `ADDR` that the deployment sets, and its metrics in the Prometheus format on a separate `METRICS_ADDR`.
- Replicas: the service runs as exactly one replica. A deployment briefly runs the new container next to the old one, so only the instance that holds a lease row in Postgres runs the dispatcher, the vendor watchers, the Claude Code orchestrator processes and the reconciliation jobs. The old instance releases the lease when it starts to drain and stops these components at that moment. The lease expires 30 seconds after its last renewal, and the holder renews it every 10 seconds. RPCs and webhooks work on both instances, because they only read and write Postgres.
- Deployment target: a Docker container on the shared DigitalOcean Droplet, deployed by the [deployment workflow](repository-workflow-contract.md#deployment-workflow) with the `deploy` target `droplet`, like `accounts-api`. Production origin: `https://tide.api.delino.io`. The Caddy reverse proxy terminates TLS and proxies to the container. tide needs no public port other than HTTPS.
- Container image: the image contains the `claude` binary at a pinned version of at least 2.1.224, because the [Claude Code adapter](#claude-code) runs the Claude Code orchestrator in the container.
- Database: PlanetScale Postgres, according to the repository defaults.
- Development ports: `127.0.0.1:18400` for HTTP (the default of `ADDR`) and `127.0.0.1:19400` for metrics (the development launcher sets `METRICS_ADDR` when it is unset). The [environment contract](repository-environment-contract.md#process-boundaries) lists them.
- Lease-scoped components: a later layer registers each background component (the dispatcher, a vendor watcher, a Claude Code orchestrator, a reconciler) with the extension registry of `servers/tide-api/internal/app`. The lease holder starts the components when it acquires the lease, restarts a component that returns early with exponential backoff (1 second to 1 minute) and stops all components before it releases or loses the lease. The daily jobs (retention deletes and re-encryption after a key rotation) are one such component: they run when the instance acquires the lease and then every 24 hours.

## Users and Operators

- Organization owners, admins and members, through the [web console](apps-tide-console-foundation.md), the [CLI](cmds-tide-cli-foundation.md) and the [SDKs](packages-tide-sdk-foundation.md).
- Coding agents that act for a member through the [`tide mcp` MCP server](cmds-tide-cli-foundation.md#mcp-server), with the member's Logto access token or personal API key.
- GitHub, which sends webhook deliveries of the tide GitHub App.
- The agent vendors: Anthropic (Claude Code orchestrator), Cursor (pending-request stream), OpenAI (webhooks) and Cognition (Devin Fleet API).
- `ownsand-api`, which tide calls with the OwnSand API key of each organization.
- Operators who deploy and monitor the service.

## Interfaces and Contracts

- Business APIs: the public services of the [protos contract](protos-tide-api-contract.md).
- Webhook endpoints: see [Webhooks](#webhooks).
- Operational endpoints: `GET /healthz` without authentication returns `200` with `{"status":"ok"}` while the process can serve HTTP requests. `GET /metrics` on `METRICS_ADDR` serves the metrics in the Prometheus format; see [Metrics](#metrics).
- Browser access: CORS allows only `https://tide.delino.io` in production and the console's loopback development origin locally.

### Authentication

- Every public RPC accepts a Logto access token for the API resource `https://tide.api.delino.io`. The server validates the signature, issuer, expiry, audience and required scopes according to the [authentication contract](repository-auth-contract.md#token-validation), and rejects ID tokens.
- The RPCs of `DevSessionService` also accept a [personal API key](#personal-api-keys) in the `Authorization: Bearer` header. Every other RPC rejects a personal API key with `permission_denied`. The server tells the two credentials apart by the `tide_` prefix. An invalid personal API key gets `unauthenticated` on every RPC.
- Logto registration (recorded decision): the API resource `https://tide.api.delino.io` defines the scope `tide:use`, and every RPC requires it in the access token. Roles in an organization come from tide's own memberships, not from token scopes. The [web console](apps-tide-console-foundation.md) and the [CLI](cmds-tide-cli-foundation.md) request access tokens for this resource with this scope. The server rejects a token whose `typ` is not `at+jwt` (ID tokens) and a machine-to-machine token whose `sub` equals its `client_id`, like `ownsand-api`. The control plane needs no Logto application for token validation.
- The webhook endpoints accept no Logto token. They accept only deliveries with a valid sender signature; see [Webhooks](#webhooks).

### Authorization

The server authorizes every RPC that takes an `organization_id` with the caller's membership in that organization. A user without a membership gets `not_found`. The server loads every target entity with that organization in the query, so a request for an entity of another organization returns `not_found`.

A personal API key authorizes as its user in the organization of the key. The server checks the user's current membership and role for each call, so a key never has more permissions than its user. A key of one organization used with another `organization_id` gets `not_found`.

`CreateOrganization`, `ListOrganizations` and `AcceptInvitation` take no `organization_id` and need no membership. They follow the same rules as in [OwnSand](servers-ownsand-api-foundation.md#authorization).

| Action                                                                                                                                            | `owner`                   | `admin`                   | `member`                  |
| ------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------- | ------------------------- | ------------------------- |
| Delete the organization, transfer ownership                                                                                                       | yes                       | no                        | no                        |
| Organization settings (the per-user dev session limit)                                                                                            | yes                       | yes                       | no                        |
| Invitations, member roles, removal of another member                                                                                              | yes                       | yes                       | no                        |
| Remove the caller's own membership (leave the organization)                                                                                       | no                        | yes                       | yes                       |
| OwnSand link, GitHub installations, agent integrations and their credentials                                                                      | yes                       | yes                       | no                        |
| Create, change and delete runner profiles                                                                                                         | yes                       | yes                       | no                        |
| Delete a runner                                                                                                                                   | yes                       | yes                       | no                        |
| Cancel a request                                                                                                                                  | yes                       | yes                       | no                        |
| Read the organization, members, the link and integration state (never credentials), runner profiles, requests, runners, capacity and audit events | yes                       | yes                       | yes                       |
| Create, list and revoke the caller's own personal API keys                                                                                        | yes                       | yes                       | yes                       |
| Create a dev session, list dev session profiles, read the metadata of every dev session of the organization                                       | yes                       | yes                       | yes                       |
| Run commands and transfer files in a dev session                                                                                                  | owner of the session only | owner of the session only | owner of the session only |
| Delete a dev session                                                                                                                              | any                       | any                       | own only                  |

- In the dev session rows, "owner of the session" is the user who created the dev session, not the organization role `owner`. No role, including `owner` and `admin`, can run commands or transfer files in another user's dev session.
- A personal API key can call only the dev session rows and nothing else; it cannot create, list or revoke API keys.
- An organization has exactly one `owner`, with the same ownership rules as OwnSand.
- Invitations are one-time secrets that expire after 7 days. The server stores only their hashes.

### OwnSand link

- An admin links the tide organization to an OwnSand organization with `SetOwnSandLink`. The request carries the OwnSand organization ID and an OwnSand organization API key.
- The server validates the link before it stores it: it calls OwnSand `ListDevices` for that organization with that key. A key of another organization fails there with `not_found`, so a stored link always matches its key.
- One OwnSand organization links to at most one tide organization. `SetOwnSandLink` fails with `already_exists` when another tide organization links the same OwnSand organization ID.
- `SetOwnSandLink` on a linked organization replaces the API key. A change of the OwnSand organization ID needs `RemoveOwnSandLink` first.
- Implementation (recorded decisions): the key must have the `osk_` prefix. The validation call counts against the `SetOwnSandLink` rate limit before it reaches OwnSand. An `unauthenticated`, `not_found` or `permission_denied` answer of the validation fails with `invalid_argument`, and `unavailable` with `unavailable`. The stored key is encrypted with the associated data of the organization ([Secrets](#secrets)). An OwnSand call that fails with `unauthenticated` marks the link `INVALID` only when the key that failed is still the stored key; a new `SetOwnSandLink` makes the link `ACTIVE` again. `RemoveOwnSandLink` first marks the link as being removed, so the dispatcher creates no new runner, then runs the clean-up steps that need OwnSand, and then deletes the link and the key in one transaction. When a clean-up step or the deletion transaction fails, the mark is cleared and the call fails; the admin calls it again.
- OwnSand client (recorded decision): `tide-api` calls OwnSand through the OwnSand Go SDK with one client per organization, built from the decrypted key and cached until the key changes. The data path uses the OwnSand relay, not a peer-to-peer session: `tide-api` relays one operation per RPC, so a session per call would add only setup time. `OWNSAND_API_ORIGIN` selects the OwnSand API ([Configuration](#configuration)).
- `RemoveOwnSandLink` stops dispatch, ends every queued request with `CANCELED`, deletes every runner of the organization as described in [Runner lifecycle](#runner-lifecycle), and then deletes the stored key.
- When OwnSand rejects the key with `unauthenticated` (for example, after an admin revoked it in OwnSand), the server marks the link `INVALID`, stops dispatch for the organization and keeps queued requests until their maximum queue wait. The console shows the state.
- Sandboxes of tide runners carry the OwnSand labels `tide.organization-id`, `tide.runner-id` and `tide.runner-profile-id`. Labels hold no secrets.

### GitHub App integration

tide uses one GitHub App that Delino owns, on github.com only.

App permissions:

- Organization permission "Self-hosted runners": read and write. tide needs it to list runner groups, to create JIT runner configurations with `POST /orgs/{org}/actions/runners/generate-jitconfig` and to remove runners.
- Repository permission "Actions": read. GitHub sends `workflow_job` events only to apps with this permission.
- Repository permission "Metadata": read, which GitHub requires for every app.
- Subscribed events: `workflow_job` and `installation`.

Installation and linking:

1. An admin calls `CreateGitHubInstallUrl`. The server returns the installation URL of the app with a `state` value. The `state` binds the tide organization and the admin's user ID, expires after 10 minutes and is single-use.
2. The admin installs the app on a GitHub organization. The app requests user authorization during installation, so GitHub redirects to the console callback with `installation_id`, `code` and `state`.
3. The console calls `LinkGitHubInstallation` with these values. The server checks the `state`, exchanges the `code` for a GitHub user access token, and accepts the link only when `GET /user/installations` with that token lists the installation and the installation's account is an organization. The server discards the user access token after this check.
4. The admin selects the runner group of the installation with `UpdateGitHubInstallation`. The default is the `Default` runner group. GitHub's runner group settings control which repositories can use the runners; tide does not change them.

- One GitHub installation links to exactly one tide organization. A tide organization can link several installations.
- Installations of personal accounts fail with `failed_precondition`, because tide registers only organization-level runners.
- When GitHub sends an `installation` event with `deleted` or `suspend`, the server marks the installation inactive, ends its queued requests with `CANCELED` and deletes its runners that wait for a job.

Job matching:

- A runner profile of kind `GITHUB_JOB` has the GitHub label `tide-<profile name>`.
- A `workflow_job` `queued` event creates a request only when the job's labels consist of exactly one label `tide-<profile name>` of an existing runner profile of the linked tide organization, optionally with `self-hosted`. The server ignores every other job, so that a tide runner never waits for a job whose other labels it does not have.
- The request key is the GitHub job ID. A repeated event for the same job ID does not create a second request.

GitHub, not tide, assigns a queued job to a runner with matching labels. A runner that tide started for one job can take another job with the same label. Therefore:

- A runner pool is one pair of a GitHub installation and a runner profile. tide registers organization-level runners, so a runner of one GitHub organization can never take a job of another GitHub organization, even with the same label. Each `GITHUB_JOB` request and each `GITHUB_JOB` runner records its GitHub installation, and every rule below counts runners and requests per runner pool.
- The dispatcher starts runners per runner pool. The oldest `QUEUED` requests of a runner pool count as covered while the pool has the same number of runners in `CREATING`, `PREPARING` or `WAITING_FOR_WORK`. The dispatcher dispatches only uncovered requests.
- Each runner registers in the GitHub organization of its installation, with the runner group of that installation and the GitHub runner name `tide-<runner_id>`. A `workflow_job` `in_progress` event names this runner. The server then binds the job's request to that runner and marks both `RUNNING` and `BUSY`.
- A `workflow_job` `completed` event ends the request. When the job never started (for example, the workflow was cancelled), the request ends with `CANCELED`. The server then compares the runner pool's runners in `CREATING`, `PREPARING` or `WAITING_FOR_WORK` with all of its remaining `QUEUED` requests, not only with its uncovered requests. When the pool has more of these runners than remaining `QUEUED` requests, the server removes the newest surplus runners: it removes the GitHub runner registration and then deletes the sandbox. When GitHub refuses the removal because the runner took a job meanwhile, the runner stays.
- GitHub does not retry failed webhook deliveries by itself. Every 5 minutes, the lease holder lists the recent deliveries of the app webhook and requests a redelivery of each failed delivery of the last 24 hours.

### Agent integrations

An agent integration holds the vendor credentials and settings of one vendor account or pool. A runner profile of kind `AGENT_SESSION` names one agent integration. An organization can have several integrations of the same vendor.

| Vendor         | Demand source in tide                                                                         | Credential that stays in tide           | Credential that enters the runner          | Runner program                                           | Guest OSes                  |
| -------------- | --------------------------------------------------------------------------------------------- | --------------------------------------- | ------------------------------------------ | -------------------------------------------------------- | --------------------------- |
| `CLAUDE_CODE`  | The `spawn-runner` hook of a Claude Code orchestrator that `tide-api` runs per integration    | Environment key                         | The single-use work order of the session   | `claude self-hosted-runner`, one session                 | `LINUX`, `MACOS`            |
| `CURSOR`       | The pending-request stream of the Cursor self-hosted pool, with claims through the Cursor API | None in addition                        | Service account API key (`CURSOR_API_KEY`) | `agent worker start --pool <pool>`, one session          | `LINUX`, `MACOS`, `WINDOWS` |
| `OPENAI_CODEX` | OpenAI `agent.session.action_required` webhooks with `environment_connection`                 | Webhook secret                          | Environment key (`CODEX_API_KEY`)          | `codex exec-server --remote <url> --environment-id <id>` | See below                   |
| `DEVIN`        | The watch stream of the Devin Fleet API, with claims through the Fleet API                    | API token with `account.outposts.write` | Outpost token (`DEVIN_OUTPOSTS_TOKEN`)     | `devin worker start --outpost=<name> --once`             | `LINUX`, `MACOS`, `WINDOWS` |

- Each runner serves one session and then its program exits. tide never gives a runner to a second session.
- tide validates the guest OS of an `AGENT_SESSION` runner profile against this table. OpenAI documents no host OS for `codex exec-server`. The Codex adapter implementation records the guest OSes that it verified, and tide accepts only those. Interim rule (recorded decision): no Codex adapter exists yet, so no guest OS is verified, and `CreateRunnerProfile` and `UpdateRunnerProfile` reject every `AGENT_SESSION` profile of an `OPENAI_CODEX` integration with `invalid_argument`. The Codex adapter change adds its verified guest OSes here and in `servers/tide-api/internal/profile`.
- Credentials (recorded decision): each credential is encrypted with the associated data of its integration and credential kind ([Secrets](#secrets)). `UpdateAgentIntegration` with the path `credentials` needs at least one non-empty credential of the integration's vendor. A daily job of the lease holder re-encrypts the credentials and the OwnSand keys of old keys after a key rotation.
- Cursor workers on Windows need Git Bash in the guest image.
- The exact command lines, flags and API endpoints come from the vendor documentation at implementation time. Record them in this contract with the adapter implementation.

#### Claude Code

- For each `CLAUDE_CODE` integration, the lease holder runs `claude self-hosted-runner orchestrator` as a child process with a hooks directory that contains the tide `spawn-runner` hook. The orchestrator uses the environment key; the key never enters a runner.
- The hook is the `tide-api` binary in hook mode. It sends the session ID, the spawn reason and the work order to the parent process over a Unix domain socket in the container, which is not reachable from the network. It exits with `0` only after tide stored the request together with its work order, and with `1` (retry) when tide could not store both.
- The work order is a secret, and the request needs it after a restart or a lease handover. tide stores it encrypted with the [secrets](#secrets) rules, in the same transaction as the request. tide deletes the stored work order when the runner of the request becomes `BUSY` or when the request ends. A runner start that fails before `BUSY` returns the request to the queue, and the next runner uses the stored work order again. A new lease holder reads the stored work order, so a deployment does not make a queued Claude Code request undispatchable.
- A runner gets the work order of its session and starts `claude self-hosted-runner` with it as the Claude Code reference documents. The runner serves that session and exits.
- The parent process restarts a crashed orchestrator with exponential backoff (1 second to 1 minute) and logs each restart.
- When tide ends a request without a runner (for example, at the maximum queue wait), the Claude Code orchestrator rules decide whether Anthropic sends a new spawn request for the session.

#### Cursor and Devin

- The lease holder keeps one watch stream per integration. When a pending request arrives, the server claims it and creates a tide request. When tide ends the request before a runner took the session, the server releases the claim through the vendor API.
- After a watch stream ends, the server reconnects with exponential backoff (1 second to 1 minute) and lists the pending requests again, so that no request is lost.

#### OpenAI Codex

- Each `OPENAI_CODEX` integration has its own webhook URL `https://tide.api.delino.io/webhooks/openai/<integration_id>` (the origin is `PUBLIC_ORIGIN`) and webhook secret. The admin registers this URL in the OpenAI project.
- An `agent.session.action_required` event with the required action type `environment_connection` creates a request. The request key is the OpenAI session ID and environment ID.
- OpenAI waits at most 5 minutes for the executor, so the maximum queue wait of an `OPENAI_CODEX` runner profile must not exceed 5 minutes.
- OpenAI documents that an idle event alone does not show that a session ended. The runner of a Codex session ends when the executor process exits, or at the runner profile's `max_ttl`.

### Runner profiles

| Field                  | Rule                                                                                                                                                                                                                                                                                                                                                                                                                |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `name`                 | Lowercase kebab-case, 1 to 40 characters, unique in the organization. For `GITHUB_JOB`, the GitHub label is `tide-<name>`.                                                                                                                                                                                                                                                                                          |
| `workload_kind`        | `GITHUB_JOB`, `AGENT_SESSION` or `DEV_SESSION`. Cannot change after creation.                                                                                                                                                                                                                                                                                                                                       |
| `agent_integration_id` | Required for `AGENT_SESSION`, absent for `GITHUB_JOB` and `DEV_SESSION`. Cannot change after creation.                                                                                                                                                                                                                                                                                                              |
| `workspace_path`       | Required for `DEV_SESSION`, absent for the other kinds. An absolute guest path in the syntax of `guest_os` (for example `/home/dev/work` or `C:\work`). Dev session file sync writes into it, and it is the default working directory of dev session commands.                                                                                                                                                      |
| `guest_os`             | `LINUX`, `MACOS` or `WINDOWS`. For `AGENT_SESSION`, one of the guest OSes of the vendor in [Agent integrations](#agent-integrations). For `DEV_SESSION`, any of the three.                                                                                                                                                                                                                                          |
| `source`               | An OwnSand base image of `guest_os`, or an OwnSand template ID whose guest OS is `guest_os`. For `DEV_SESSION`, the admin uses a template with the toolchains that the organization's projects need.                                                                                                                                                                                                                |
| `resources`            | `vcpu_count`, `memory_mib` and `disk_gib`, with the OwnSand rules.                                                                                                                                                                                                                                                                                                                                                  |
| `network_mode`         | `OUTBOUND`. Every runner program needs outbound access to GitHub or to its vendor, so tide does not offer `NONE`. Dev sessions use the same rule, so that builds and tests can download dependencies.                                                                                                                                                                                                               |
| `idle_ttl`, `max_ttl`  | Passed to OwnSand, with the OwnSand rules (`max_ttl` at most 7 days). The running runner program keeps the sandbox active, so `idle_ttl` deletes a sandbox only after its program ended. A dev session has no runner program: every command and file operation restarts the OwnSand idle clock, and a running command keeps the sandbox active, so `idle_ttl` deletes a dev session that nobody used for that time. |
| `priority`             | `HIGH`, `NORMAL` or `LOW`. Default: `HIGH` for `GITHUB_JOB`, `NORMAL` for `AGENT_SESSION` and `DEV_SESSION`.                                                                                                                                                                                                                                                                                                        |
| `max_queue_wait`       | Required. Greater than zero and at most 24 hours; at most 5 minutes for an `OPENAI_CODEX` integration.                                                                                                                                                                                                                                                                                                              |
| `warm_count`           | 0 to 10. Default `0`. The same rule applies to `DEV_SESSION`.                                                                                                                                                                                                                                                                                                                                                       |

- A change of a runner profile applies to runners that start after the change. Running runners keep their settings.
- Validation details (recorded decisions): a Windows `workspace_path` is `X:\` followed by a relative path with `\` separators; a Linux or macOS `workspace_path` starts with `/`. Neither has a `.` or `..` segment, an empty segment or a trailing separator, because sync keeps its manifest and lock next to the workspace. A template `source` is checked with OwnSand (the template exists and its snapshot has the profile's `guest_os`) when the organization has an `ACTIVE` OwnSand link; without one, OwnSand checks it when a runner starts. `idle_ttl`, `max_ttl` and `max_queue_wait` must be whole milliseconds, because tide stores them in milliseconds. `network_mode` and `priority` are required in updates; `CreateRunnerProfile` fills an unset `priority` with the default of the workload kind. `UpdateRunnerProfile` accepts the mask paths `name`, `workspace_path`, `guest_os`, `source`, `resources`, `network_mode`, `idle_ttl`, `max_ttl`, `priority`, `max_queue_wait` and `warm_count`, and validates the whole result. Overlapping updates of one profile do not undo each other: when the stored profile changed after an update read it, the update applies its mask again to the new profile. When the profile changes during three attempts in a row, the update fails with `failed_precondition`.
- Deleting a runner profile ends its queued requests with `CANCELED` and deletes its warm runners and its runners that wait for work. Busy runners finish their work.

### Personal API keys

- Every member can create personal API keys for themselves in an organization with `ApiKeyService.CreateApiKey`, with a name (1 to 64 characters) and an optional expiry time. A key without an expiry stays valid until it is revoked. Each user lists and revokes only their own keys.
- Secret format: `tide_` followed by 32 random bytes from a cryptographically secure source in unpadded base64url, like the [OwnSand API keys](servers-ownsand-api-foundation.md#authentication). `CreateApiKey` returns the secret once.
- The server stores only the SHA-256 hash of the secret and a short non-secret prefix for display. A lookup hashes the presented value, loads the row by that hash and compares the stored hash in constant time. A value with the `tide_` prefix but a wrong shape gets `unauthenticated`.
- A revoked or expired key gets `unauthenticated`. The server records the last use time of each key with at most one write per key per minute. Only an accepted use counts: a valid key that an RPC other than `DevSessionService` rejects does not change the last use time.
- The display prefix is `tide_` and the first 4 characters after it, like the invitation display prefix (`tdi_` and 4 characters). The rate limit of authentication failures keys on it.
- When a membership ends (removal by an admin or leaving), the server revokes every key of that user in that organization in the same transaction. It then ends the user's dev sessions of the organization as described in [Dev sessions](#dev-sessions). The dev session layer does this through the membership end hook of the core: one step in the same transaction and one step after the commit.
- Organization deletion revokes every key of the organization and records an `API_KEY_REVOKED` audit event for each, then deletes the keys with the organization.

### Dev sessions

A dev session gives one user a sandbox of a chosen guest OS for builds and tests. The user, or a coding agent for the user, picks a `DEV_SESSION` runner profile by name. The admin decides the guest OS, source, resources, workspace path, TTLs, priority, maximum queue wait and warm count in the profile.

Creation and queueing:

- `CreateDevSession` takes the runner profile ID and an optional label (at most 100 characters, for display). It creates a request of kind `DEV_SESSION` with the caller as the session owner and returns at once with the dev session ID (the request ID) and the request state. It never waits for capacity.
- The request uses the [queue](#queue-and-dispatch) with the profile's priority and maximum queue wait, like every other request. When no capacity is free, it stays `QUEUED`; when its maximum queue wait ends, it ends with `EXPIRED`.
- `GetDevSession` and `ListDevSessions` return the state. The SDKs and the MCP server wait for readiness by repeated `GetDevSession` calls.
- Per-user limit: the organization setting `max_dev_sessions_per_user` is unset by default, which means no limit. An admin can set it to a positive integer. It counts the user's dev sessions in `QUEUED`, `STARTING` and `RUNNING`. `CreateDevSession` fails with `resource_exhausted` when the user already has that many. Lowering the setting ends no existing dev session.

Lifecycle:

- Dispatch is the same as for `AGENT_SESSION` requests: the dispatcher assigns a warm runner of the profile or creates a sandbox. In `PREPARING`, tide creates the profile's `workspace_path` in the guest when it is missing. tide installs and starts no runner program.
- When the runner is ready, the runner changes to `BUSY` and the request to `RUNNING`. The request is `STARTING` from runner assignment until then. Only a `RUNNING` dev session accepts commands and file transfers; other states get `failed_precondition`.
- `DeleteDevSession` by the session owner, or by an `owner` or `admin` of the organization, ends a queued dev session with `CANCELED` and the end reason `CANCELED_BY_USER`, and ends a starting or running dev session with `ENDED` and the end reason `SESSION_DELETED` after tide deletes its sandbox. Running commands end with the sandbox.
- When OwnSand deletes the sandbox at its `idle_ttl` or `max_ttl`, the runner changes to `DELETED` with `SANDBOX_ENDED`, and the request ends with `ENDED` and the end reason `SESSION_TTL_EXPIRED`. Other OwnSand end reasons follow step 6 of the [runner lifecycle](#runner-lifecycle): the runner changes to `FAILED` and the request ends with `FAILED` and `RUNNER_LOST`.
- When the owner's membership ends, the server ends the owner's queued dev sessions with `CANCELED` and the end reason `MEMBER_REMOVED`, and deletes the sandboxes of the others, which end with `ENDED` and `MEMBER_REMOVED`.
- A dev session does not depend on the client that created it. When the MCP server or the CLI exits, the dev session stays until it is deleted or its TTL ends.
- Implementation (recorded decisions): `CreateDevSession` serializes the caller's creations in an organization with a lock of the caller's membership, so two parallel calls cannot pass the per-user limit together. It also holds a shared lock of the runner profile row until it commits, so a concurrent `DeleteRunnerProfile` either cancels the new request or makes the call fail with `not_found`. `DeleteDevSession` ends the request and marks the runner `DELETING` in one transaction, then deletes the sandbox before it answers. It locks the session's open runner and then the request (the lock order of the engine) and decides on the locked state, so a session that the dispatcher started meanwhile has its runner deleted and is not only canceled; when OwnSand fails, the lease holder retries the sandbox deletion. When the owner's membership ends, the same transaction that revokes the API keys ends the dev sessions, and the sandboxes are deleted after the commit, with the same retry.

### Dev session commands

- `ExecDevSession` (server-streaming) runs one command in the guest. The request carries the dev session ID, a client-generated `exec_id` (UUID v7), the argument array, an environment map, an optional working directory (an absolute guest path, or a path relative to `workspace_path`; the default is `workspace_path`) and an optional timeout. The server checks the caller and the session state, then calls the OwnSand exec through the OwnSand Go SDK with the organization's OwnSand API key and forwards the event stream.
- `AttachDevSessionExec` (server-streaming) continues the event stream of an exec from given stdout and stderr offsets, with OwnSand `AttachExec`.
- The tide exec event messages carry the same information as the OwnSand exec events (`started`, `output_truncated`, `stdout` and `stderr` chunks with offsets, `exited` with an exit code or signal and `timed_out`, `failed` with a reason, `sandbox_ended`), in tide's own schema. The OwnSand rules apply without change: the command runs independently of the stream, the agent keeps the last 8 MiB of each output stream and the exit result for 10 minutes after the command ends, and stdin is not supported.
- tide ends each forwarded stream before it has run for 14 minutes, with an OK status and no final event, and sends a keepalive event at least every 60 seconds while the command produces no output. The client continues with `AttachDevSessionExec`.
- Environment values in exec requests are not stored, not logged and not added to audit events. They can carry secrets that a local `tide mcp` process passes from its own environment.
- Implementation (recorded decisions): a forwarded stream ends after 13 minutes and 30 seconds, and a keepalive goes out when no event went out for 30 seconds. A relative `working_directory` or file `path` is joined to the runner's `workspace_path` with the separator of the guest OS; an absolute path is used unchanged. The `exec_id` is sent in lowercase. `dev_session_exec_ended` log records carry the result, exit code or signal, the failure reason and the duration, never the command or the environment.

### Dev session file transfer

- `WriteDevSessionFile` (unary) writes one chunk of at most 1 MiB to an absolute guest path at an offset, and `ReadDevSessionFile` (server-streaming) streams a guest file from an offset. The server forwards them to OwnSand `WriteFile` and `ReadFile` with the OwnSand operation ID rules: the caller generates the `write_id` or `read_id`.
- The server applies no file size limit of its own. The clients enforce the transfer limits of [sync](#dev-session-sync) and [download](#dev-session-download) before they start a transfer, because the limits protect the bandwidth of the shared Droplet.
- Relay state (recorded decision): the OwnSand binding salt of a relayed read or write is derived from its `read_id` or `write_id` with the primary key of `TIDE_SECRET_KEYS` (HMAC-SHA256), so every call of one operation uses the same salt on every instance. A `ReadDevSessionFile` call that ends at the stream limit sends no end message; the client continues with the same `read_id` from its next offset. `tide-api` keeps the OwnSand write of each `write_id` in memory between `WriteDevSessionFile` calls and drops it 20 minutes after its last chunk or after the final chunk. After a deployment handover, the next chunk opens the write again on the new instance with the same salt, so the write continues; a write that OwnSand already dropped (15 minutes without a chunk) fails with `not_found`, and the client starts the file again with a new `write_id`. A rotation of the primary key during a transfer changes the salt; the client then starts the transfer again.

#### Dev session sync

The Go SDK implements sync for the CLI and the MCP server. Sync copies a local working tree into the profile's `workspace_path`:

1. The local root is a directory in a Git working tree. The file list is the output of `git ls-files --cached --others --exclude-standard` for that directory: tracked files and untracked files that are not ignored. Ignored files are never sent. A local directory that is not in a Git working tree fails before any transfer. Submodule entries are not entered; sync reports them as skipped.
2. The client reads the guest manifest `<workspace_path>.tide-sync.json` (next to the workspace, not in it). The manifest lists each synced relative path with its size, SHA-256 and mode. A missing or unreadable manifest counts as empty.
3. The client hashes the local files and computes the changed and new files, and the deleted files (in the manifest, but not in the local list). Files that the guest created and that are not in the manifest, such as build output, stay.
4. The client rejects the sync before any upload when a changed file is larger than 100 MiB or the changed files together are larger than 2 GiB. The error lists the paths and sizes that break the limit.
5. The client rejects the sync before any upload when two local paths differ only in case and the guest OS is `WINDOWS` or `MACOS`, or when a path is not valid on a `WINDOWS` guest (reserved names or characters). The error lists the paths.
6. The client takes the sync lock: it creates the directory `<workspace_path>.tide-sync.lock` with one exec, which fails when the directory exists. A sync that finds the lock fails with `failed_precondition` and tells the caller that another sync runs. `--force` (CLI) or `force` (MCP) removes a lock that a stopped sync left.
7. The client packs the changed files into one tar archive, uploads it with `WriteDevSessionFile` to a temporary file next to the workspace, extracts it into `workspace_path` with the guest's `tar` (part of Linux, macOS and Windows 10 and later), deletes the deleted files, writes the new manifest, removes the archive and releases the lock.

- Contents are copied byte for byte. Sync converts no line endings.
- Linux and macOS guests keep the executable bit. Windows guests ignore modes.
- Symbolic links are created as links. When a Windows guest cannot create a link, sync reports the path as a warning and continues.
- A sync that stops before step 7 ends leaves the old manifest, so the next sync sends the same files again. A sync is idempotent.

#### Dev session download

The Go SDK implements download for the CLI and the MCP server. Download copies a guest file or directory to a local path:

- A directory is packed in the guest with `tar` into a temporary archive next to the workspace, downloaded with `ReadDevSessionFile` and extracted locally. The client removes the guest archive after the download.
- The client rejects the download when one file is larger than 500 MiB or the transfer is larger than 2 GiB in total. It checks the size of a single file or of the archive before it reads it, and checks each archive entry during extraction.
- Local extraction rejects entries with absolute paths, entries that resolve outside the local destination and symbolic links that point outside it. It writes to a temporary location and moves the result into place only after the download succeeds.
- Download never changes the local working tree unless the caller names a destination in it.

### Queue and dispatch

- The queue holds the requests in `QUEUED` state of all runner profiles of one organization. The order is: priority (`HIGH` first), then the time when tide created the request (oldest first), then the request ID.
- The dispatcher runs a pass for an organization when a request arrives, a runner ends, a sandbox of the organization ends, a runner profile changes, and at least every 15 seconds.
- In each pass, the dispatcher goes through the uncovered requests in queue order:
  1. When a warm runner of the request's profile exists, it assigns that runner to the request.
  2. Otherwise, it calls OwnSand `CreateSandbox` without `device_id`, with the profile's source, resources, network, TTLs and the tide labels. The OwnSand scheduler selects the device and applies the device limits, including the [vCPU overcommit](servers-ownsand-api-foundation.md#vcpu-overcommit).
  3. When OwnSand returns `unavailable`, the request stays in the queue. When warm runners of other profiles exist, the dispatcher deletes one of them (from the profile with the lowest priority first, then the oldest warm runner), so that the next pass may find capacity.
  4. When OwnSand returns `failed_precondition`, no device of the OwnSand organization can ever run the profile. The request ends with `FAILED` and the end reason `NO_ELIGIBLE_DEVICE`.
  5. The dispatcher continues with the next request. A request that does not fit does not block smaller requests behind it.
- The dispatcher creates warm runners only when the queue of the organization has no uncovered request, so that warm runners never take capacity from waiting requests.
- tide never preempts a runner that serves a request.
- Each `CreateSandbox` call uses an `idempotency_key` that tide generates and stores with the runner before the call, so a retry after a lost response does not create a second sandbox.
- A request that is still `QUEUED` when its `max_queue_wait` ends ends with `EXPIRED`. tide releases its vendor claim when one exists.
- Implementation (recorded decisions):
  - The dispatcher, one driver goroutine per open runner (the runner reconciler) and the gauge collector run as lease components. A change on the lease holder (a new request, a profile or link change, an ended runner) starts a pass at once; a change on another instance waits for the next periodic pass.
  - A request of a binding workload kind (`DEV_SESSION`, `AGENT_SESSION`) stays `QUEUED` until OwnSand created its sandbox: the dispatcher stores the runner with the request and the idempotency key first, so a request that finds no capacity does not change state, and the stored runner keeps the next pass from dispatching it twice. The request is `STARTING` from the successful `CreateSandbox`, which also counts the start attempt.
  - `unavailable` removes the stored runner (no sandbox exists) and deletes at most one warm runner of another profile per pass. `invalid_argument`, `not_found` and `permission_denied` from `CreateSandbox` count as a start failure. These rules, and the `failed_precondition` rule, apply only to an error that OwnSand sent (a Connect wire error): OwnSand answers an error only before it commits a sandbox. An error that the client made from a transport failure or from the HTTP status of an intermediary (for example a proxy's 503, which the client reads as `unavailable`) is an uncertain result, because the sandbox may exist. Any other error or a lost response keeps the runner `CREATING`, and its driver repeats `CreateSandbox` with the same key. Each `CreateSandbox` call ends after 15 seconds without an answer; that end is an uncertain result, and it also ends the organization's dispatcher pass, so an OwnSand call that stalls cannot stop the dispatch and the maximum queue wait checks of other organizations. The runner stores the `CreateSandbox` request of its first call, and every retry sends that request unchanged: OwnSand binds the key to the request parameters, so a request rebuilt from a profile that changed or was deleted meanwhile would fail with `already_exists`.
  - A request's `priority` is the profile's priority when the request arrives.
  - Workload adapters: the `work.Adapter` interface holds what the contract leaves to a workload kind: whether the dispatcher binds a runner to one request (`BindsAtDispatch`) or starts runners for a runner pool (`PoolKey`, `GITHUB_JOB`), preparation of the guest (`Prepare`), the start of the runner program with its credential (`Start`), the request outcome of a program exit, a sandbox end and a runner deletion, and the release of a vendor claim. A layer that receives demand creates requests with `Engine.CreateRequest` (unique per organization, source and source key) and reports progress with `Engine.MarkBusy` and `Engine.EndRequest`. Only the `DEV_SESSION` adapter exists; a request of a kind without an adapter waits until its maximum queue wait.

### Runner lifecycle

1. `CREATING`: tide created the OwnSand sandbox and waits until it is `RUNNING`.
2. `PREPARING`: tide checks for the runner program in the guest and installs it when it is missing (see [Runner installation](#runner-installation)). For `DEV_SESSION`, tide creates the `workspace_path` instead and installs nothing. A warm runner then changes to `WARM` and waits for a request.
3. For `DEV_SESSION`, a runner that has a request changes to `BUSY` at once; see [Dev sessions](#dev-sessions). For the other kinds, when the runner has a request (or, for `GITHUB_JOB`, when the dispatcher starts it for its profile), tide writes the runner credential to a guest file that only the runner process reads and starts the runner program with an OwnSand exec. The program writes its output to a log file in the guest, not to the exec stream, so that the exec stream carries only the exit result. The runner is then `WAITING_FOR_WORK`.
4. `BUSY`: the runner program took its job or session.
5. When the runner program exits, or when a dev session is deleted, tide deletes the sandbox (`DELETING`, then `DELETED`). tide follows each runner exec with OwnSand `AttachExec` and resumes it after each relay stream rotation. When tide misses the exit, the profile's `idle_ttl` deletes the sandbox.
6. When OwnSand ends the sandbox for another reason (TTL, device removal, `LOST_ON_DEVICE`), the runner changes to `FAILED` with the OwnSand end reason. An `AGENT_SESSION` request of that runner ends with `FAILED`; the vendor decides whether the session runs again. A `GITHUB_JOB` request ends when GitHub sends the `completed` event.
7. A failure in steps 1 to 3 changes the runner to `FAILED`, deletes the sandbox and returns its request to `QUEUED` with its original queue time. After 3 failed attempts for the same request, the request ends with `FAILED` and the end reason `RUNNER_START_FAILED`.

- Deleting a `GITHUB_JOB` runner that waits for a job first removes its GitHub runner registration in the GitHub organization of its installation.
- An admin can delete any runner with `DeleteRunner`. tide deletes the sandbox; the work in it ends.
- Implementation (recorded decisions): a driver checks the sandbox state every 10 seconds and waits at most 30 minutes for a `CREATING` sandbox to be `RUNNING`. A runner that was created for a request that ended before the work started (for example, a dev session that was deleted meanwhile) is deleted with `CLEAN_UP`. `RemoveOwnSandLink`, `DeleteRunnerProfile` and `DeleteOrganization` delete their runners with `CLEAN_UP` before the transaction that ends the queued requests: the link removal ends them with `CANCELED` and `INTEGRATION_REMOVED`, the profile deletion with `PROFILE_DELETED` and the organization deletion with `CANCELED_BY_USER`. A sandbox that OwnSand no longer accepts the key for is left to its `idle_ttl`. A deleted runner without a stored sandbox ID (its `CreateSandbox` response was lost or is still in flight) stays `DELETING` until tide repeats `CreateSandbox` with the stored request and key: tide deletes the sandbox that this call returns, and an error answer of OwnSand means that the key created nothing. The repeated call can create a sandbox when no earlier call did; tide then deletes it at once. A runner that ends while it still has a sandbox to delete (after a start failure, or when a `CreateSandbox` response arrives after the runner ended) is marked for clean-up in the same transaction, and the reconciler retries the sandbox deletion until OwnSand deletes the sandbox, the link is removed or OwnSand rejects the key.
- `WatchQueue` (recorded decision): a trigger gives every write of a request or runner a new change number; the stream reads the changes every second on any instance, sends a keepalive after 25 seconds without an event and ends after 13 minutes and 30 seconds. `ListRequests` uses an offset page token, because its order is the queue order.

### Runner installation

- When a runner program is missing in the guest, tide installs it with an OwnSand exec before it starts the runner:
  - GitHub Actions runner: the official release archive from `github.com/actions/runner` at the version that tide pins.
  - `claude`: the official Claude Code installer, at least version 2.1.224.
  - Cursor `agent`: the official Cursor CLI installer.
  - `codex`: the `@openai/codex` npm package at the version that tide pins. The guest needs Node.js. When the guest has no Node.js, tide first installs the official Node.js 24 distribution at the version that tide pins.
  - `devin`: the official Devin CLI installer.
- tide installs the runner programs on every guest OS that the runner profile allows for its workload kind and vendor: Linux, macOS and Windows.
- Version policy (recorded decision):
  - Pinned programs: the GitHub Actions runner, `codex` and Node.js. tide-api pins an exact version of each in its source. Renovate updates the pins through a custom manager that the runner implementation adds to `renovate.json`.
  - Unpinned programs: `claude`, Cursor `agent` and `devin`. tide installs the version that the official installer gives at installation time. tide checks the minimum version only for `claude` (2.1.224). Cursor `agent` and `devin` have no minimum version.
  - tide uses a program that the guest already has (for example, from an OwnSand template) at its installed version, also when that version differs from the pin. tide installs the program again only when its version is lower than the minimum version. Only `claude` has a minimum version.
- An admin can make starts faster: create an OwnSand sandbox from the base image, let tide or the admin install the runner programs, save it as an OwnSand template and set it as the runner profile's `source`. tide then finds the program and skips the installation.
- The exact install commands per guest OS come from the vendor documentation. The runner implementation records them here, together with the guest OSes on which it verified them.

### Webhooks

| Endpoint                                 | Sender | Verification                                                                                                      |
| ---------------------------------------- | ------ | ----------------------------------------------------------------------------------------------------------------- |
| `POST /webhooks/github`                  | GitHub | HMAC-SHA256 of the raw body with the app webhook secret, from `X-Hub-Signature-256`, constant-time.               |
| `POST /webhooks/openai/{integration_id}` | OpenAI | The OpenAI webhook signature of the raw body with the integration's webhook secret, with its timestamp tolerance. |

- The server verifies the signature before it parses the payload. A delivery with a missing or invalid signature gets `401` and creates nothing.
- The server records the delivery ID and ignores a repeated delivery.
- The server stores the event in the same transaction that changes the request, and then answers `2xx`. It does not wait for the dispatcher.
- Webhook bodies are limited to 1 MiB.

### Secrets

- Organization credentials are the OwnSand API key and the credentials of each agent integration. Claude Code work orders of queued requests use the same encryption. The server encrypts each one with AES-256-GCM before it stores it. The encryption key comes from the service secret `TIDE_SECRET_KEYS`, which holds one or more keys with IDs. Each stored value records the ID of its key, so that the operator can add a new key and re-encrypt stored values.
- `TIDE_SECRET_KEYS` format (recorded decision): a comma-separated list of `<key ID>:<key>` entries. A key ID is 1 to 32 ASCII letters, digits, `-` or `_`, and a key is 32 random bytes in base64 (standard or URL alphabet, with or without padding). The first entry is the primary key, which encrypts every new value; the server decrypts with every listed key. Each value is bound to its owner with GCM associated data (for example the kind of credential and the organization ID), so a value copied to another row does not decrypt.
- Key rotation: the operator adds the new key as the first entry and keeps the old keys after it, and deploys. The daily re-encryption jobs of the lease holder, which each layer that stores encrypted values registers, re-encrypt the values of the old keys with the primary key and log how many they changed. When no value uses an old key, the operator removes it and deploys again. A missing or invalid `TIDE_SECRET_KEYS` stops the server at startup.
- The API accepts credentials only in set operations and never returns them. Read operations return only whether a credential is set and when it was set.
- The GitHub App ID, private key, client ID, client secret and webhook secret are service secrets of the deployment, according to the [environment contract](repository-environment-contract.md).
- The server decrypts a credential only for the call that uses it, and passes a runner credential only to the runner that needs it, as the [agent integrations table](#agent-integrations) shows. The OwnSand API key, the GitHub App private key, the Claude Code environment key, the Devin API token and the OpenAI webhook secret never enter a runner.
- GitHub JIT runner configurations are secrets. tide creates one right before it starts the runner and never stores it.

### Capacity view

`GetCapacity` returns, for each device of the linked OwnSand organization, the data that OwnSand `ListDevices` returns about state, limits, the effective vCPU limit and allocation, together with the number of tide runners on the device by state. The console uses it to show how the device pool is used.

### Metrics

The server records its metrics with the OpenTelemetry SDK through the shared `servers/internal/telemetry` package, like `accounts-api` ([accounts-api metrics](servers-accounts-api-foundation.md#metrics)). `GET /metrics` on `METRICS_ADDR` serves them in the Prometheus format through the OpenTelemetry Prometheus exporter. Caddy does not route `METRICS_ADDR`, so the endpoint is not public. Metric labels never contain organization IDs, user IDs, repository names or other user data.

| Metric                                    | Type      | Labels                        |
| ----------------------------------------- | --------- | ----------------------------- |
| `tide_requests`                           | gauge     | `kind`, `priority`, `state`   |
| `tide_request_queue_wait_seconds`         | histogram | `kind`, `priority`            |
| `tide_requests_ended_total`               | counter   | `kind`, `state`, `end_reason` |
| `tide_runners`                            | gauge     | `kind`, `guest_os`, `state`   |
| `tide_runner_start_seconds`               | histogram | `kind`, `guest_os`            |
| `tide_ownsand_calls_total`                | counter   | `rpc`, `code`                 |
| `tide_webhook_deliveries_total`           | counter   | `sender`, `result`            |
| `tide_vendor_watch_reconnects_total`      | counter   | `vendor`                      |
| `tide_claude_orchestrator_restarts_total` | counter   | none                          |
| `tide_dev_session_execs_total`            | counter   | `guest_os`, `result`          |
| `tide_dev_session_transfer_bytes_total`   | counter   | `direction`, `guest_os`       |

- Implementation status: `tide_ownsand_calls_total`, `tide_requests`, `tide_request_queue_wait_seconds` (recorded when a request leaves `QUEUED`), `tide_requests_ended_total`, `tide_runners`, `tide_runner_start_seconds` (creation until `WARM`, `WAITING_FOR_WORK` or `BUSY`), `tide_dev_session_execs_total` and `tide_dev_session_transfer_bytes_total` are recorded; the lease holder records the two gauges every 15 seconds. The webhook, vendor and orchestrator metrics are `planned` with their layers.
- The `kind` label includes `DEV_SESSION`. `tide_dev_session_execs_total` counts ended dev session execs by `result` (`exited`, `failed`, `sandbox_ended`). `tide_dev_session_transfer_bytes_total` counts the file bytes that `tide-api` relays, with `direction` `upload` or `download`, so that operators can see the Droplet bandwidth that dev sessions use.

- A webhook delivery that the server rejects for a missing or invalid signature counts with `result` `invalid_signature`.

When the OTLP endpoint is configured, the shared telemetry package sends the metrics over OTLP to Grafana Cloud, the default observability backend ([repository defaults](repository-defaults.md#default-technology-choices)), as for `accounts-api`.

- Retention (recorded decision): the default metrics retention of the Grafana Cloud plan. tide sets no other retention.
- Dashboard (recorded decision): one tide dashboard, committed as Grafana dashboard JSON under `servers/tide-api/` (`planned`). It shows the queue, the runners, the OwnSand calls, the webhook deliveries and the dev session transfer bytes. The implementation records the file path and how the dashboard gets to Grafana Cloud.
- Alerts (recorded decision): Grafana Cloud alert rules send notifications to the Grafana Cloud contact point that operators manage. The repository does not record the notification channel.

| Alert                       | Condition                                                                                                                          |
| --------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| Runner start failures       | The increase of `tide_requests_ended_total` with `end_reason` `RUNNER_START_FAILED` in 15 minutes is 1 or more.                    |
| OwnSand call errors         | In 10 minutes, more than 5% of `tide_ownsand_calls_total` has a `code` other than `ok`.                                            |
| Webhook signature failures  | The increase of `tide_webhook_deliveries_total` with `result` `invalid_signature` in 5 minutes is more than 10.                    |
| Vendor or orchestrator loss | The increase of `tide_vendor_watch_reconnects_total` or of `tide_claude_orchestrator_restarts_total` in 15 minutes is more than 5. |

### Audit events

The server records audit events for: organization creation and deletion, ownership transfer, invitations, member role changes and removals, OwnSand link changes, GitHub installation links and changes, agent integration creation, credential replacement and deletion, runner profile changes, request cancellation by a user, runner deletion by a user, organization setting changes, personal API key creation and revocation (including the revocation when a membership ends), dev session creation and dev session deletion by a user. A dev session event names the session owner and, for a deletion by another user, the deleting user. Each event has the actor, the action, the target entity and the time. Audit events never contain credentials, command arguments, environment values, command output or file contents.

### Organization deletion

`DeleteOrganization` revokes every personal API key of the organization, ends every queued request with `CANCELED`, deletes every runner as described in [Runner lifecycle](#runner-lifecycle), stops the Claude Code orchestrators and vendor watchers of the organization, removes the GitHub installation links, and then deletes the stored credentials. tide cannot uninstall the GitHub App; the console tells the owner to uninstall it in GitHub.

- Each layer does its part through an organization deletion hook of the core, registered in the order above: a step before the deletion transaction that may call OwnSand or GitHub (a failure stops the deletion and keeps the organization, so the step is idempotent), a step in the transaction under the organization row lock, and a step after the commit. The core then revokes the personal API keys, records the audit events and deletes the organization row with its memberships, invitations and keys.

## Storage

- PlanetScale Postgres tables: organizations (with the organization settings), memberships, invitations, OwnSand links, GitHub installations, agent integrations, runner profiles, requests (with the dev session owner and label), runners, personal API keys (hash, display prefix, owner, name, expiry, last use, revocation), webhook deliveries, the lease and audit events. The core migration `00001_core.sql` creates the organizations, memberships, invitations, personal API keys, audit events and the lease; `00002_configuration.sql` creates the OwnSand links, agent integrations with their encrypted credentials and runner profiles; `00003_work.sql` creates the requests and runners; GitHub installations and webhook deliveries are `planned`. Each later layer adds its own migration with the next free five-digit version, and its organization tables reference the organization with `ON DELETE CASCADE`, so organization deletion removes them.
- Requests store the vendor or GitHub key (GitHub job ID, Claude Code session ID, Cursor or Devin request ID, OpenAI session and environment ID) with a unique constraint per organization and source, so that a repeated event or claim cannot create a second request.
- Webhook delivery IDs are kept for 7 days.
- Retention of ended records (recorded decision): an ended request (including an ended dev session) and an ended runner are kept for 30 days after they end, and then a daily job of the lease holder deletes them. This is the same period as for ended OwnSand sandboxes.
- Retention of audit events (recorded decision): 90 days, the same as OwnSand. A daily job of the lease holder deletes older events. Organization deletion does not delete audit events early.
- Migration tool (recorded decision): [goose](https://github.com/pressly/goose) (`github.com/pressly/goose/v3`), like `accounts-api`, with SQL migrations embedded from `servers/tide-api/internal/db/migrations/`. Migrations are forward-only, as in `accounts-api`.
- Backup policy (recorded decision): the default automatic backups of PlanetScale Postgres, with no custom backup schedule. A backup is made every 12 hours and kept for 2 days, and point-in-time recovery is available in that period ([PlanetScale Postgres backups](https://planetscale.com/docs/postgres/backups)).
- tide stores no runner output, no dev session command, environment, output or file content, and no JIT configuration. It stores a Claude Code work order only encrypted and only until the runner of the request is `BUSY` or the request ends; see [Claude Code](#claude-code).

## Configuration

The server reads these environment variables without a service prefix. Empty values count as unset. A value that fails validation stops the server at startup, and the error never contains a secret value.

| Variable                      | Required | Default                         | Rule                                                                                                           |
| ----------------------------- | -------- | ------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| `ADDR`                        | no       | `127.0.0.1:18400`               | `host:port` of the HTTP/1.1 and h2c listener. The deployment sets it.                                          |
| `LOG_LEVEL`                   | no       | `info`                          | `debug`, `info`, `warn` or `error`.                                                                            |
| `DATABASE_URL`                | yes      |                                 | Postgres connection URL.                                                                                       |
| `RUN_MIGRATIONS`              | no       | `true`                          | Apply the embedded migrations at startup.                                                                      |
| `LOGTO_ISSUER`                | yes      |                                 | Logto issuer, `<tenant endpoint>/oidc`.                                                                        |
| `LOGTO_JWKS_URL`              | no       | `LOGTO_ISSUER` + `/jwks`        | Key set URL.                                                                                                   |
| `LOGTO_JWKS_FILE`             | no       | empty                           | Local key set file instead of the URL, for end-to-end test issuers.                                            |
| `LOGTO_AUDIENCE`              | no       | `https://tide.api.delino.io`    | Expected `aud`.                                                                                                |
| `CORS_ORIGINS`                | no       | `https://tide.delino.io`        | Comma-separated browser origins.                                                                               |
| `TRUST_PROXY_CLIENT_ADDRESS`  | no       | `false`                         | Client address from the last `X-Forwarded-For` entry, as in OwnSand.                                           |
| `ALLOW_DIRECT_CLIENT_ADDRESS` | no       | `false`                         | Client address from the TCP peer when the proxy setting is off. The development launcher sets `true`.          |
| `TIDE_SECRET_KEYS`            | yes      |                                 | Service secret; see [Secrets](#secrets).                                                                       |
| `METRICS_ADDR`                | no       | empty (listener off)            | `host:port` of the internal metrics listener. The development launcher sets `127.0.0.1:19400`.                 |
| `OTEL_*`                      | no       | empty (no OTLP export)          | Standard OpenTelemetry variables, read by the SDK through the shared telemetry package, as for `accounts-api`. |
| `OWNSAND_API_ORIGIN`          | no       | `https://ownsand.api.delino.io` | Origin of the OwnSand API, for example `http://127.0.0.1:18300` for a local `ownsand-api`.                     |
| `PUBLIC_ORIGIN`               | no       | `https://tide.api.delino.io`    | Public origin of `tide-api` in the webhook URLs that admins register.                                          |

- Later layers add the GitHub App and vendor settings to this table with their implementation.
- Member display names: `ListMembers` reads them through an optional display name source. The `delino-accounts` API has no display name RPC yet, as the [OwnSand contract](servers-ownsand-api-foundation.md#dependencies-and-integrations) records, so tide-api has no client or configuration for it and returns empty display names until that RPC exists.

## Security

- tide runs no customer code. Runner code runs only in OwnSand sandboxes on the organization's own devices.
- A runner can read the credential that tide gives to it. For Cursor, OpenAI Codex and Devin, that credential can connect workers or executors of that vendor account or pool. Admins should use credentials that are limited to the pool or environment that tide serves.
- The GitHub App never gets repository contents permissions. Jobs get their repository access from the `GITHUB_TOKEN` of the workflow, as with any self-hosted runner.
- Public repositories can use tide runners only when the admin allows them in the GitHub runner group settings.
- The `spawn-runner` hook socket accepts connections only from processes in the container.
- Dev sessions run code of their owner on the organization's devices, like the other runners. Only the session owner can run commands or read files in it, so code and secrets that a user copies into a dev session are not visible to other members through tide. Members who can reach the device or the OwnSand organization directly are outside this rule.
- A leaked personal API key gives access only to the dev sessions of its user and to dev session creation. The user or an admin who removes the membership revokes it.
- Rate limits (recorded decision, the same values as [OwnSand](servers-ownsand-api-foundation.md#security)): in-memory token buckets, which are service-wide because the service runs as one replica. A rejected call fails with `resource_exhausted`.
  - `AcceptInvitation`: 10 calls per invitation display prefix per minute and 10 calls per user per minute, each with a burst of 10.
  - The OwnSand link validation of `SetOwnSandLink`: 10 calls per user per minute with a burst of 10.
  - Personal API key authentication failures: 10 failures per key display prefix per minute with a burst of 10. Only failures take tokens, and a prefix without tokens is rejected before validation.
  - When the request has a client address: 120 calls per client address per minute with a burst of 60, in one bucket per address that these limits share. The client address comes from the same rule and the same configuration settings as in OwnSand (`TRUST_PROXY_CLIENT_ADDRESS` and `ALLOW_DIRECT_CLIENT_ADDRESS`); without a client address, this bucket does not apply.

## Logging

- Structured `log/slog` records with `organization_id`, `request_id`, `runner_id`, `runner_profile_id`, the OwnSand `sandbox_id`, the GitHub job ID and delivery ID, the vendor request ID, the dev session `exec_id`, `read_id` or `write_id`, and the personal API key ID where they apply.
- Log every dev session exec end with its result, exit code or failure reason and duration, and every relayed file operation with its size and duration.
- Log every request and runner state change with the previous state, the new state and the end reason.
- Log every OwnSand call result with the RPC and the Connect code, every GitHub API error with its status, every vendor watch reconnect and every orchestrator restart.
- Never log credentials, tokens, API key secrets, JIT configurations, work orders, webhook bodies, exec commands that carry credentials, runner output, or dev session command arguments, environment values, output and file contents.
- CLI-style output of the development launcher uses ANSI color by default, with the `NO_COLOR` opt-out.

## Build and Test

- Package scripts wrap the Go toolchain like `accounts-api`. Before finishing a change, run `pnpm exec turbo run test --filter=tide-api`, `go vet ./...` and the `gofmt` check from `servers/AGENTS.md`.
- Postgres-backed tests (the `db`, `lease`, `jobs` and `api` integration tests) use `TIDE_TEST_DATABASE_URL`, a Postgres URL whose user may create databases. Each test creates and drops its own database; without the variable, these tests skip. The CI `go-test` job sets it. A local run: `TIDE_TEST_DATABASE_URL=postgres://postgres:<password>@127.0.0.1:<port>/postgres?sslmode=disable pnpm exec turbo run test --filter=tide-api`, for example against a `postgres:17` container.
- Unit tests: queue order, coverage counting for GitHub profiles, warm runner rules, maximum queue wait, retry after runner start failure, runner profile validation (including `workspace_path`), webhook signature checks and duplicate deliveries, credential encryption with key rotation, the authorization matrix (including personal API keys on every service and dev session owner checks), personal API key format, hashing, expiry and revocation, the per-user dev session limit, and dev session end reasons.
- Integration tests with fakes of `ownsand-api`, the GitHub API and each vendor API: full request lifecycles for each workload kind and vendor, `unavailable` and `failed_precondition` from OwnSand, lost webhook deliveries with redelivery, cancelled jobs with surplus runner removal, vendor watch reconnects, a dev session that waits in the queue and starts, exec stream rotation and attach, relayed file reads and writes, dev session deletion by the owner and by an admin, TTL expiry, and the end of a membership with running dev sessions and keys.
- Acceptance (manual, recorded in the pull request or Linear issue): the [project success criterion](project-tide.md#goal) with a real GitHub organization, real vendor accounts and real Linux, macOS and Windows devices. Keep fake-based results separate from real acceptance.

## Dependencies and Integrations

- Upstream: [`ownsand-api`](servers-ownsand-api-foundation.md) through the [OwnSand Go SDK](packages-ownsand-sdk-foundation.md); the GitHub REST API and webhooks; the Claude Code CLI; the Cursor, OpenAI and Devin APIs; Logto; PlanetScale Postgres.
- Downstream: the [web console](apps-tide-console-foundation.md), the [CLI](cmds-tide-cli-foundation.md) and the [SDKs](packages-tide-sdk-foundation.md).
- tide depends on a working OwnSand runtime. tide runtime implementation starts after the OwnSand sandbox lifecycle (create, exec, delete) works on real devices.
- The migration tool goose is a recorded owner decision (see [Storage](#storage)); its version follows the root `go.mod`.
- New dependencies (Connect, protobuf, a Postgres driver and any GitHub or vendor client library) need owner approval and a [dependency security](repository-dependency-security-contract.md) review in the change that adds them.

## Change Triggers

- Update the [project index](project-tide.md) when components, origins, the layering on OwnSand or the authentication exception change.
- Update the [protos contract](protos-tide-api-contract.md) with every RPC, message or enum change.
- Update the console, CLI and SDK contracts when their surface changes.
- Update the OwnSand contracts first when tide needs a new OwnSand capability.
- Update the [authentication contract](repository-auth-contract.md) when the webhook endpoints or their verification change.
- Update `servers/AGENTS.md` only when development procedures, directory ownership or development rules change; follow the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-tide.md](project-tide.md)
- [servers-ownsand-api-foundation.md](servers-ownsand-api-foundation.md)
- [protos-ownsand-api-contract.md](protos-ownsand-api-contract.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [repository-environment-contract.md](repository-environment-contract.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [GitHub REST API: self-hosted runners](https://docs.github.com/en/rest/actions/self-hosted-runners)
- [GitHub webhook events: `workflow_job`](https://docs.github.com/en/webhooks/webhook-events-and-payloads#workflow_job)
- [Claude Code self-hosted environments reference](https://code.claude.com/docs/en/self-hosted-environments-reference)
- [Cursor self-hosted pool](https://cursor.com/docs/cloud-agent/self-hosted/pool)
- [OpenAI self-hosted sandboxes](https://developers.openai.com/api/docs/guides/agents-api/environments/self-hosted)
- [OpenAI environment lifecycle](https://developers.openai.com/api/docs/guides/agents-api/environments/lifecycle)
- [Devin Outposts reference](https://docs.devin.ai/cloud/outposts/reference)
