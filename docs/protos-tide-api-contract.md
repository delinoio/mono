# protos-tide API contract

## Scope

This contract owns the Connect RPC schemas of [`tide`](project-tide.md) in the protobuf package `delino.tide.v1` at `protos/delino/tide/v1/`, with the committed Go bindings (Go package `tidev1`, Connect package `tidev1connect`) and the committed TypeScript bindings of the workspace package `@delino/tide-protos` (`protos/delino/tide/`). It defines every public service, message and enum of `tide-api`.

## Runtime and Language

- Protobuf schemas, proto3 syntax.
- Go bindings are committed in the root Go module, like the other packages under `protos/`. TypeScript code is generated for the [console](apps-tide-console-foundation.md) and the [TypeScript SDK](packages-tide-sdk-foundation.md).
- The code generation tool is the tool that the [OwnSand protos contract](protos-ownsand-api-contract.md#runtime-and-language) records: the buf CLI and the Go protobuf and Connect plugins, run by `scripts/protos.sh` with the module configuration in `protos/buf.yaml` and `protos/buf.gen.yaml`.

## Users and Operators

- [`tide-api`](servers-tide-api-foundation.md), which serves the services.
- The [console](apps-tide-console-foundation.md), the [CLI](cmds-tide-cli-foundation.md) and the [SDKs](packages-tide-sdk-foundation.md), which call them.

## Interfaces and Contracts

### General rules

- Package: `delino.tide.v1`. A breaking change needs a new package version (`v2`). Within `v1`, do not reuse or renumber fields, do not change field types, and reserve the numbers and names of removed fields.
- Identifiers are UUID v7 values in canonical text form in `string` fields named `<entity>_id`. The tide link of a GitHub installation uses `installation_id`, because `github_installation_id` is the GitHub numeric installation ID. User ID fields (`user_id`) hold the Logto `sub` unchanged. OwnSand identifiers use the field names `ownsand_organization_id`, `ownsand_sandbox_id`, `ownsand_device_id` and `ownsand_template_id`. GitHub numeric identifiers use `int64` fields named `github_<entity>_id`.
- Times use `google.protobuf.Timestamp`. Durations use `google.protobuf.Duration`. Sizes use `int64` fields with a unit suffix (`memory_mib`, `disk_gib`).
- Every enum has a zero value `<ENUM>_UNSPECIFIED`. The server rejects `UNSPECIFIED` in a required enum field with `invalid_argument`.
- If a list filter contains an undefined enum number, the server returns `invalid_argument` and names the field.
- List RPCs use `page_size` (default 50, maximum 200) and an opaque `page_token`.
- Every public RPC is unary or server-streaming. tide has no client-streaming or bidirectional RPC.
- Errors use Connect error codes. Detail messages never contain credentials.
- Stored credential fields exist only in request messages of set operations. Response messages carry `credential_set` (bool) and `credential_set_at` instead. The only exceptions are the two one-time secrets: the personal API key secret in `CreateApiKeyResponse` and the invitation secret in `CreateInvitationResponse`.
- `DevSessionService` RPCs accept a Logto access token or a personal API key. Every other service accepts only a Logto access token.
- Enum value names follow the buf `STANDARD` lint rule: each value has the upper-snake-case enum name as its prefix (for example `REQUEST_STATE_QUEUED`, `OWN_SAND_LINK_STATE_ACTIVE` and `GIT_HUB_INSTALLATION_STATE_ACTIVE`). The tables below list the values without the prefix.
- A server-streaming RPC returns a `<Rpc>Response` message that wraps its event, so that every RPC has its own response type.

### Public services

The control plane serves every public service at `https://tide.api.delino.io`. Every RPC except `OrganizationService.CreateOrganization`, `OrganizationService.ListOrganizations` and `MemberService.AcceptInvitation` takes an `organization_id`. The [control plane contract](servers-tide-api-foundation.md#authorization) owns the role that each RPC requires.

| Service                   | RPC                          | Kind             | Purpose                                                                                                                                                              |
| ------------------------- | ---------------------------- | ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `OrganizationService`     | `CreateOrganization`         | unary            | Create an organization. The caller becomes its `owner`.                                                                                                              |
|                           | `GetOrganization`            | unary            | Read one organization.                                                                                                                                               |
|                           | `ListOrganizations`          | unary            | List the organizations of the signed-in user.                                                                                                                        |
|                           | `DeleteOrganization`         | unary            | Delete the organization and start its clean-up.                                                                                                                      |
|                           | `TransferOwnership`          | unary            | Make another member the `owner`.                                                                                                                                     |
|                           | `UpdateOrganizationSettings` | unary            | Change the organization settings with a field mask. Today the only setting is `max_dev_sessions_per_user` (unset means no limit). `GetOrganization` returns it.      |
| `MemberService`           | `CreateInvitation`           | unary            | Create a one-time invitation with a role. Returns the secret once.                                                                                                   |
|                           | `ListInvitations`            | unary            | List pending invitations without secrets.                                                                                                                            |
|                           | `RevokeInvitation`           | unary            | Revoke a pending invitation.                                                                                                                                         |
|                           | `AcceptInvitation`           | unary            | Join the organization with an invitation secret.                                                                                                                     |
|                           | `ListMembers`                | unary            | List members, roles and display names.                                                                                                                               |
|                           | `UpdateMemberRole`           | unary            | Change a member role between `admin` and `member`.                                                                                                                   |
|                           | `RemoveMember`               | unary            | Remove a member, or leave the organization.                                                                                                                          |
| `OwnSandLinkService`      | `SetOwnSandLink`             | unary            | Link an OwnSand organization with an API key, or replace the key.                                                                                                    |
|                           | `GetOwnSandLink`             | unary            | Read the link and its `OwnSandLinkState`, without the key.                                                                                                           |
|                           | `RemoveOwnSandLink`          | unary            | Remove the link after tide deletes its runners.                                                                                                                      |
| `GitHubService`           | `CreateGitHubInstallUrl`     | unary            | Return the app installation URL with a single-use `state`.                                                                                                           |
|                           | `LinkGitHubInstallation`     | unary            | Link an installation with `installation_id`, `code` and `state`.                                                                                                     |
|                           | `ListGitHubInstallations`    | unary            | List linked installations with their account, state and runner group.                                                                                                |
|                           | `ListGitHubRunnerGroups`     | unary            | List the runner groups of an installation's GitHub organization.                                                                                                     |
|                           | `UpdateGitHubInstallation`   | unary            | Set the runner group of an installation.                                                                                                                             |
|                           | `UnlinkGitHubInstallation`   | unary            | Remove the link. Cancels its queued requests and deletes its waiting runners.                                                                                        |
| `AgentIntegrationService` | `CreateAgentIntegration`     | unary            | Create an integration with a vendor, name, settings and credentials.                                                                                                 |
|                           | `GetAgentIntegration`        | unary            | Read one integration without credentials. For `OPENAI_CODEX`, includes its webhook URL.                                                                              |
|                           | `ListAgentIntegrations`      | unary            | List integrations without credentials.                                                                                                                               |
|                           | `UpdateAgentIntegration`     | unary            | Change the name or settings, or replace credentials.                                                                                                                 |
|                           | `DeleteAgentIntegration`     | unary            | Delete an integration. Fails with `failed_precondition` while a runner profile names it or while tide holds a vendor claim of it.                                    |
| `RunnerProfileService`    | `CreateRunnerProfile`        | unary            | Create a runner profile.                                                                                                                                             |
|                           | `GetRunnerProfile`           | unary            | Read one runner profile.                                                                                                                                             |
|                           | `ListRunnerProfiles`         | unary            | List runner profiles.                                                                                                                                                |
|                           | `UpdateRunnerProfile`        | unary            | Change the mutable fields of a runner profile.                                                                                                                       |
|                           | `DeleteRunnerProfile`        | unary            | Delete a runner profile.                                                                                                                                             |
| `WorkService`             | `ListRequests`               | unary            | List requests, with optional profile, kind and state filters; ended requests only on request.                                                                        |
|                           | `GetRequest`                 | unary            | Read one request.                                                                                                                                                    |
|                           | `CancelRequest`              | unary            | End a `QUEUED` request with `CANCELED`.                                                                                                                              |
|                           | `ListRunners`                | unary            | List runners, with optional profile and state filters; ended runners only on request.                                                                                |
|                           | `GetRunner`                  | unary            | Read one runner.                                                                                                                                                     |
|                           | `DeleteRunner`               | unary            | Delete a runner and its sandbox.                                                                                                                                     |
|                           | `GetCapacity`                | unary            | Return the device pool view of the [control plane](servers-tide-api-foundation.md#capacity-view).                                                                    |
|                           | `WatchQueue`                 | server-streaming | Stream request and runner changes of the organization for live views. Sends a keepalive at least every 30 seconds and ends before 14 minutes; the client reconnects. |
| `ApiKeyService`           | `CreateApiKey`               | unary            | Create a personal API key of the caller with a name and an optional expiry. Returns the secret once.                                                                 |
|                           | `ListApiKeys`                | unary            | List the caller's own keys with name, display prefix, expiry, last use and state, without secrets.                                                                   |
|                           | `RevokeApiKey`               | unary            | Revoke one of the caller's own keys.                                                                                                                                 |
| `DevSessionService`       | `ListDevSessionProfiles`     | unary            | List the `DEV_SESSION` runner profiles with name, guest OS, resources, workspace path, TTLs and maximum queue wait.                                                  |
|                           | `CreateDevSession`           | unary            | Create a dev session request for a profile, with an optional label. Returns at once with the dev session and its state.                                              |
|                           | `GetDevSession`              | unary            | Read one dev session.                                                                                                                                                |
|                           | `ListDevSessions`            | unary            | List dev sessions of the organization, with optional owner (including "the caller"), profile and state filters; ended sessions only on request.                      |
|                           | `DeleteDevSession`           | unary            | Cancel a queued dev session or delete a starting or running one.                                                                                                     |
|                           | `ExecDevSession`             | server-streaming | Run a command in a running dev session and stream `DevSessionExecEvent` messages.                                                                                    |
|                           | `AttachDevSessionExec`       | server-streaming | Continue the event stream of an exec from given stdout and stderr offsets.                                                                                           |
|                           | `ReadDevSessionFile`         | server-streaming | Stream a guest file from an offset in chunks of at most 1 MiB.                                                                                                       |
|                           | `WriteDevSessionFile`        | unary            | Write one chunk of at most 1 MiB to a guest file at an offset.                                                                                                       |
| `AuditService`            | `ListAuditEvents`            | unary            | List audit events of the organization.                                                                                                                               |

### Organization and member messages

- `Organization` carries `settings` (`OrganizationSettings`) with the optional `max_dev_sessions_per_user`. `UpdateOrganizationSettings` takes the settings and a field mask; a masked path with no value clears the setting.
- Invitation secrets are `tdi_` followed by 32 random bytes in unpadded base64url. `CreateInvitationResponse` returns the secret once.

### OwnSand link and GitHub messages

- `SetOwnSandLinkRequest` carries `ownsand_organization_id` and the credential `ownsand_api_key`. `OwnSandLink` carries the state, `credential_set` and `credential_set_at`. `GetOwnSandLinkResponse` has no `link` when the organization has no link.
- `GitHubInstallation` carries `installation_id`, `github_installation_id`, the GitHub account login and ID, the state, and the runner group (`github_runner_group_id` and its name). `LinkGitHubInstallation` takes `github_installation_id`, `code` and `state` from the GitHub callback.

### Runner profile messages

`RunnerProfile` carries the fields of the [control plane runner profile table](servers-tide-api-foundation.md#runner-profiles). `source` is a `oneof` of `base_image` (`GuestOs`) and `ownsand_template_id`. `CreateRunnerProfile` sets every field. `UpdateRunnerProfile` uses a field mask and rejects a change of `workload_kind` or `agent_integration_id` with `invalid_argument`.

Use `resources` to replace all resource values. Use `resources.vcpu_count`, `resources.memory_mib` or `resources.disk_gib` to change individual values. The server keeps the current value of each omitted resource and validates the resulting profile.

### Agent integration messages

`AgentIntegration` has `agent_integration_id`, `vendor`, `name`, a vendor settings `oneof` and the credential state: one `AgentCredentialState` (`AgentCredential`, `credential_set` and `credential_set_at`) for each credential of the vendor. Create and update requests carry the credentials in a vendor credentials `oneof`; `UpdateAgentIntegration` uses a field mask with the paths `name`, `settings` and `credentials`, and replaces only the non-empty credential fields. The settings and credentials per vendor:

| Vendor         | Settings       | Credentials                                    |
| -------------- | -------------- | ---------------------------------------------- |
| `CLAUDE_CODE`  | none           | `environment_key`                              |
| `CURSOR`       | `pool_name`    | `service_account_api_key`                      |
| `OPENAI_CODEX` | none           | `environment_key`, `webhook_secret`, `api_key` |
| `DEVIN`        | `outpost_name` | `api_token`, `outpost_token`                   |

- `OPENAI_CODEX` `api_key` (`AgentCredential` `API_KEY`, field 3 of `OpenAiCodexCredentials`) is an OpenAI API key of the project with the `api.agents.read` permission. The OpenAI `agent.session.action_required` webhook carries only the session ID and the action type, so `tide-api` reads the session's environment ID and remote URL with this key; the key stays in tide. The field and the enum value are additive changes of `v1`.

### Personal API key and dev session messages

- `ApiKey`: `api_key_id`, `name`, `display_prefix`, `state`, `created_at`, `expires_at` (absent when the key does not expire), `last_used_at` and `revoked_at`. `CreateApiKeyResponse` adds the `secret` field once.
- `DevSession`: `dev_session_id` (the request ID), `runner_profile_id`, `owner_user_id`, `label`, `guest_os`, `workspace_path`, `state` (`RequestState`), `end_reason`, `queued_at`, `started_at`, `ended_at`, `runner_id` and `ownsand_device_id` when a runner serves it.
- `ExecDevSessionRequest`: `organization_id`, `dev_session_id`, `exec_id`, `command` (argument array), `env` (string map), optional `working_directory` and optional `timeout`. `AttachDevSessionExecRequest`: `organization_id`, `dev_session_id`, `exec_id`, `stdout_offset` and `stderr_offset`.
- `ExecDevSessionResponse` and `AttachDevSessionExecResponse` wrap one `DevSessionExecEvent`. `DevSessionExecEvent` is a `oneof` of `keepalive`, `started`, `output_truncated` (first available offsets), `stdout` and `stderr` (`offset` and `data`), `exited` (`oneof` of `exit_code` or `signal`, and `timed_out`), `failed` (`ExecFailureReason` and a message) and `sandbox_ended` (the OwnSand end reason as text). It copies the meaning of the OwnSand exec events and does not import OwnSand schemas.
- `ReadDevSessionFileRequest`: `organization_id`, `dev_session_id`, `read_id`, `path` and `offset`; the stream carries chunks with `offset` and `data`, and a final message with `size_bytes`. `WriteDevSessionFileRequest`: `organization_id`, `dev_session_id`, `write_id`, `path`, `offset`, `data`, `mode` and `final`. Every chunk of a file uses the same `write_id`; the file appears at the path only after the `final` chunk, and the written chunks must then cover the whole file. The response returns the written range and, after the final chunk, `finished` and `size_bytes`.

### Audit messages

- `AuditEvent`: `audit_event_id`, `action` (`AuditAction`), `actor_user_id`, `actor_api_key_id` when the actor used a personal API key, `target_kind` (`AuditTargetKind`), `target_id`, `subject_user_id` (the dev session owner for dev session events and the affected member for membership events) and `create_time`. `ListAuditEvents` returns the newest events first.

### Request and runner messages

- `Request`: `request_id`, `runner_profile_id`, `kind`, `source_key` (the GitHub job ID or vendor request key as text, for display), `state`, `end_reason`, `priority`, `queued_at`, `started_at`, `ended_at`, `runner_id` when a runner serves it, and for `GITHUB_JOB` the `github_installation_id`, the repository full name, the workflow run ID and the job name.
- `WatchQueueResponse` is a `oneof` of `keepalive`, `request`, `runner` and `snapshot_complete`. Each stream first sends every non-terminal request and runner and then `snapshot_complete`, and then sends each changed request or runner.
- `GetCapacityResponse` lists one `DeviceCapacity` per device: the OwnSand device ID and name, `DeviceState`, the host OS and architecture as text, the limits, the effective vCPU limit, the allocation and the tide runner counts by `RunnerState`.
- `Runner`: `runner_id`, `runner_profile_id`, `kind`, `state`, `end_reason`, `ownsand_sandbox_id`, `ownsand_device_id`, `request_id` when it serves one, `github_installation_id` for `GITHUB_JOB`, `created_at`, `ended_at` and, when it ended with an OwnSand end reason, that reason as text.

### Enums

| Enum                      | Values                                                                                                                                                                                                                                    |
| ------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `WorkloadKind`            | `GITHUB_JOB`, `AGENT_SESSION`, `DEV_SESSION`                                                                                                                                                                                              |
| `AgentVendor`             | `CLAUDE_CODE`, `CURSOR`, `OPENAI_CODEX`, `DEVIN`                                                                                                                                                                                          |
| `GuestOs`                 | `LINUX`, `MACOS`, `WINDOWS`                                                                                                                                                                                                               |
| `Priority`                | `HIGH`, `NORMAL`, `LOW`                                                                                                                                                                                                                   |
| `RequestState`            | `QUEUED`, `STARTING`, `RUNNING`, `ENDED`, `CANCELED`, `EXPIRED`, `FAILED`                                                                                                                                                                 |
| `RequestEndReason`        | `COMPLETED`, `CANCELED_BY_SOURCE`, `CANCELED_BY_USER`, `MAX_QUEUE_WAIT`, `NO_ELIGIBLE_DEVICE`, `RUNNER_START_FAILED`, `RUNNER_LOST`, `PROFILE_DELETED`, `INTEGRATION_REMOVED`, `SESSION_DELETED`, `SESSION_TTL_EXPIRED`, `MEMBER_REMOVED` |
| `RunnerState`             | `CREATING`, `PREPARING`, `WARM`, `WAITING_FOR_WORK`, `BUSY`, `DELETING`, `DELETED`, `FAILED`                                                                                                                                              |
| `RunnerEndReason`         | `WORK_DONE`, `SURPLUS`, `WARM_RECLAIMED`, `DELETED_BY_USER`, `START_FAILED`, `SANDBOX_ENDED`, `CLEAN_UP`                                                                                                                                  |
| `OwnSandLinkState`        | `ACTIVE`, `INVALID`                                                                                                                                                                                                                       |
| `GitHubInstallationState` | `ACTIVE`, `SUSPENDED`, `DELETED`                                                                                                                                                                                                          |
| `ApiKeyState`             | `ACTIVE`, `EXPIRED`, `REVOKED`                                                                                                                                                                                                            |
| `AgentCredential`         | `ENVIRONMENT_KEY`, `SERVICE_ACCOUNT_API_KEY`, `WEBHOOK_SECRET`, `API_TOKEN`, `OUTPOST_TOKEN`, `API_KEY`                                                                                                                                   |
| `NetworkMode`             | `OUTBOUND`                                                                                                                                                                                                                                |
| `InvitationState`         | `PENDING`, `ACCEPTED`, `REVOKED`, `EXPIRED`                                                                                                                                                                                               |
| `DeviceState`             | `ONLINE`, `OFFLINE`                                                                                                                                                                                                                       |
| `ExecFailureReason`       | `PROGRAM_NOT_FOUND`, `PERMISSION_DENIED`, `WORKING_DIRECTORY_INVALID`, `RESOURCE_EXHAUSTED`, `OTHER` (the values of the OwnSand enum)                                                                                                     |

- `ENDED`, `CANCELED`, `EXPIRED` and `FAILED` are terminal request states. `DELETED` and `FAILED` are terminal runner states.
- `STARTING` means that a runner was assigned to an agent session or dev session request and is not yet `BUSY`. `GITHUB_JOB` requests go from `QUEUED` to `RUNNING` directly, because GitHub assigns the job.

## Storage

The schemas define no storage. The [control plane contract](servers-tide-api-foundation.md#storage) owns persistence.

## Security

- No response message carries a credential, a JIT configuration or a work order. The only secrets in responses are the personal API key secret (`CreateApiKeyResponse.secret`) and the invitation secret (`CreateInvitationResponse.invitation_secret`). Each is returned once, and `ListApiKeys` and `ListInvitations` omit them.
- The invitation secret is an input of `AcceptInvitation`: the request field `AcceptInvitationRequest.invitation_secret`.
- Exec environment values are request fields that the server neither stores nor logs.

## Logging

Generated code does not log. Components never log credential fields of request messages.

## Build and Test

- After a schema change, run `pnpm install` and `scripts/protos.sh generate` from the repository root, and commit the schemas with the generated Go and TypeScript bindings. `scripts/protos.sh check` (lint, format and binding drift) and `scripts/protos.sh breaking <base>` cover `delino.tide.v1` together with the other packages under `protos/`, in the same CI job.
- `protos/delino/tide/v1/schema_test.go` checks the general rules: `UNSPECIFIED` zero values, no client-streaming RPC, no credential in a response except the two one-time secrets, and `organization_id` on every RPC except the three documented ones.
- The workspace package `@delino/tide-protos` has the scripts `build` and `typecheck`.

## Dependencies and Integrations

- Upstream: none. The schemas do not import OwnSand schemas, so tide clients do not depend on OwnSand bindings.
- Downstream: `tide-api`, the console, the CLI and the SDKs.

## Change Triggers

- Update the [project index](project-tide.md) and the [control plane contract](servers-tide-api-foundation.md) when a service, message or enum changes behavior.
- Update the console, CLI and SDK contracts when their surface changes.
- Update `protos/AGENTS.md` only when development procedures, directory ownership or development rules change; follow the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-tide.md](project-tide.md)
- [servers-tide-api-foundation.md](servers-tide-api-foundation.md)
- [protos-ownsand-api-contract.md](protos-ownsand-api-contract.md)
- [repository-defaults.md](repository-defaults.md)
