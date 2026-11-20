# protos-tide API contract

## Scope

This contract owns the Connect RPC schemas of [`tide`](project-tide.md) in the protobuf package `delino.tide.v1` at `protos/delino/tide/v1/` (`planned`), with the committed Go bindings and the generated TypeScript code. It defines every public service, message and enum of `tide-api`.

## Runtime and Language

- Protobuf schemas, proto3 syntax.
- Go bindings are committed in the root Go module, like the other packages under `protos/`. TypeScript code is generated for the [console](apps-tide-console-foundation.md) and the [TypeScript SDK](packages-tide-sdk-foundation.md).
- The code generation tool is the same tool that OwnSand selects. It is an open decision in the [OwnSand project index](project-ownsand.md#open-decisions).

## Users and Operators

- [`tide-api`](servers-tide-api-foundation.md), which serves the services.
- The [console](apps-tide-console-foundation.md), the [CLI](cmds-tide-cli-foundation.md) and the [SDKs](packages-tide-sdk-foundation.md), which call them.

## Interfaces and Contracts

### General rules

- Package: `delino.tide.v1`. A breaking change needs a new package version (`v2`). Within `v1`, do not reuse or renumber fields, do not change field types, and reserve the numbers and names of removed fields.
- Identifiers are UUID v7 values in canonical text form in `string` fields named `<entity>_id`. User ID fields (`user_id`) hold the Logto `sub` unchanged. OwnSand identifiers use the field names `ownsand_organization_id`, `ownsand_sandbox_id`, `ownsand_device_id` and `ownsand_template_id`. GitHub numeric identifiers use `int64` fields named `github_<entity>_id`.
- Times use `google.protobuf.Timestamp`. Durations use `google.protobuf.Duration`. Sizes use `int64` fields with a unit suffix (`memory_mib`, `disk_gib`).
- Every enum has a zero value `<ENUM>_UNSPECIFIED`. The server rejects `UNSPECIFIED` in a required enum field with `invalid_argument`.
- List RPCs use `page_size` (default 50, maximum 200) and an opaque `page_token`.
- Every public RPC is unary or server-streaming. tide has no client-streaming or bidirectional RPC.
- Errors use Connect error codes. Detail messages never contain credentials.
- Credential fields exist only in request messages of set operations. Response messages carry `credential_set` (bool) and `credential_set_at` instead.

### Public services

The control plane serves every public service at `https://tide.api.delino.io`. Every RPC except `OrganizationService.CreateOrganization`, `OrganizationService.ListOrganizations` and `MemberService.AcceptInvitation` takes an `organization_id`. The [control plane contract](servers-tide-api-foundation.md#authorization) owns the role that each RPC requires.

| Service                   | RPC                        | Kind             | Purpose                                                                                                                                                              |
| ------------------------- | -------------------------- | ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `OrganizationService`     | `CreateOrganization`       | unary            | Create an organization. The caller becomes its `owner`.                                                                                                              |
|                           | `GetOrganization`          | unary            | Read one organization.                                                                                                                                               |
|                           | `ListOrganizations`        | unary            | List the organizations of the signed-in user.                                                                                                                        |
|                           | `DeleteOrganization`       | unary            | Delete the organization and start its clean-up.                                                                                                                      |
|                           | `TransferOwnership`        | unary            | Make another member the `owner`.                                                                                                                                     |
| `MemberService`           | `CreateInvitation`         | unary            | Create a one-time invitation with a role. Returns the secret once.                                                                                                   |
|                           | `ListInvitations`          | unary            | List pending invitations without secrets.                                                                                                                            |
|                           | `RevokeInvitation`         | unary            | Revoke a pending invitation.                                                                                                                                         |
|                           | `AcceptInvitation`         | unary            | Join the organization with an invitation secret.                                                                                                                     |
|                           | `ListMembers`              | unary            | List members, roles and display names.                                                                                                                               |
|                           | `UpdateMemberRole`         | unary            | Change a member role between `admin` and `member`.                                                                                                                   |
|                           | `RemoveMember`             | unary            | Remove a member, or leave the organization.                                                                                                                          |
| `OwnSandLinkService`      | `SetOwnSandLink`           | unary            | Link an OwnSand organization with an API key, or replace the key.                                                                                                    |
|                           | `GetOwnSandLink`           | unary            | Read the link and its `OwnSandLinkState`, without the key.                                                                                                           |
|                           | `RemoveOwnSandLink`        | unary            | Remove the link after tide deletes its runners.                                                                                                                      |
| `GitHubService`           | `CreateGitHubInstallUrl`   | unary            | Return the app installation URL with a single-use `state`.                                                                                                           |
|                           | `LinkGitHubInstallation`   | unary            | Link an installation with `installation_id`, `code` and `state`.                                                                                                     |
|                           | `ListGitHubInstallations`  | unary            | List linked installations with their account, state and runner group.                                                                                                |
|                           | `ListGitHubRunnerGroups`   | unary            | List the runner groups of an installation's GitHub organization.                                                                                                     |
|                           | `UpdateGitHubInstallation` | unary            | Set the runner group of an installation.                                                                                                                             |
|                           | `UnlinkGitHubInstallation` | unary            | Remove the link. Cancels its queued requests and deletes its waiting runners.                                                                                        |
| `AgentIntegrationService` | `CreateAgentIntegration`   | unary            | Create an integration with a vendor, name, settings and credentials.                                                                                                 |
|                           | `GetAgentIntegration`      | unary            | Read one integration without credentials. For `OPENAI_CODEX`, includes its webhook URL.                                                                              |
|                           | `ListAgentIntegrations`    | unary            | List integrations without credentials.                                                                                                                               |
|                           | `UpdateAgentIntegration`   | unary            | Change the name or settings, or replace credentials.                                                                                                                 |
|                           | `DeleteAgentIntegration`   | unary            | Delete an integration. Fails with `failed_precondition` while a runner profile names it.                                                                             |
| `RunnerProfileService`    | `CreateRunnerProfile`      | unary            | Create a runner profile.                                                                                                                                             |
|                           | `GetRunnerProfile`         | unary            | Read one runner profile.                                                                                                                                             |
|                           | `ListRunnerProfiles`       | unary            | List runner profiles.                                                                                                                                                |
|                           | `UpdateRunnerProfile`      | unary            | Change the mutable fields of a runner profile.                                                                                                                       |
|                           | `DeleteRunnerProfile`      | unary            | Delete a runner profile.                                                                                                                                             |
| `WorkService`             | `ListRequests`             | unary            | List requests, with optional profile, kind and state filters; ended requests only on request.                                                                        |
|                           | `GetRequest`               | unary            | Read one request.                                                                                                                                                    |
|                           | `CancelRequest`            | unary            | End a `QUEUED` request with `CANCELED`.                                                                                                                              |
|                           | `ListRunners`              | unary            | List runners, with optional profile and state filters; ended runners only on request.                                                                                |
|                           | `GetRunner`                | unary            | Read one runner.                                                                                                                                                     |
|                           | `DeleteRunner`             | unary            | Delete a runner and its sandbox.                                                                                                                                     |
|                           | `GetCapacity`              | unary            | Return the device pool view of the [control plane](servers-tide-api-foundation.md#capacity-view).                                                                    |
|                           | `WatchQueue`               | server-streaming | Stream request and runner changes of the organization for live views. Sends a keepalive at least every 30 seconds and ends before 14 minutes; the client reconnects. |
| `AuditService`            | `ListAuditEvents`          | unary            | List audit events of the organization.                                                                                                                               |

### Runner profile messages

`RunnerProfile` carries the fields of the [control plane runner profile table](servers-tide-api-foundation.md#runner-profiles). `source` is a `oneof` of `base_image` (`GuestOs`) and `ownsand_template_id`. `CreateRunnerProfile` sets every field. `UpdateRunnerProfile` uses a field mask and rejects a change of `workload_kind` or `agent_integration_id` with `invalid_argument`.

### Agent integration messages

`AgentIntegration` has `agent_integration_id`, `vendor`, `name`, a vendor settings `oneof` and the credential state. The settings and credentials per vendor:

| Vendor         | Settings       | Credentials                         |
| -------------- | -------------- | ----------------------------------- |
| `CLAUDE_CODE`  | none           | `environment_key`                   |
| `CURSOR`       | `pool_name`    | `service_account_api_key`           |
| `OPENAI_CODEX` | none           | `environment_key`, `webhook_secret` |
| `DEVIN`        | `outpost_name` | `api_token`, `outpost_token`        |

### Request and runner messages

- `Request`: `request_id`, `runner_profile_id`, `kind`, `source_key` (the GitHub job ID or vendor request key as text, for display), `state`, `end_reason`, `priority`, `queued_at`, `started_at`, `ended_at`, `runner_id` when a runner serves it, and for `GITHUB_JOB` the `github_installation_id`, the repository full name, the workflow run ID and the job name.
- `Runner`: `runner_id`, `runner_profile_id`, `kind`, `state`, `end_reason`, `ownsand_sandbox_id`, `ownsand_device_id`, `request_id` when it serves one, `github_installation_id` for `GITHUB_JOB`, `created_at`, `ended_at` and, when it ended with an OwnSand end reason, that reason as text.

### Enums

| Enum                      | Values                                                                                                                                                                        |
| ------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `WorkloadKind`            | `GITHUB_JOB`, `AGENT_SESSION`                                                                                                                                                 |
| `AgentVendor`             | `CLAUDE_CODE`, `CURSOR`, `OPENAI_CODEX`, `DEVIN`                                                                                                                              |
| `GuestOs`                 | `LINUX`, `MACOS`, `WINDOWS`                                                                                                                                                   |
| `Priority`                | `HIGH`, `NORMAL`, `LOW`                                                                                                                                                       |
| `RequestState`            | `QUEUED`, `STARTING`, `RUNNING`, `ENDED`, `CANCELED`, `EXPIRED`, `FAILED`                                                                                                     |
| `RequestEndReason`        | `COMPLETED`, `CANCELED_BY_SOURCE`, `CANCELED_BY_USER`, `MAX_QUEUE_WAIT`, `NO_ELIGIBLE_DEVICE`, `RUNNER_START_FAILED`, `RUNNER_LOST`, `PROFILE_DELETED`, `INTEGRATION_REMOVED` |
| `RunnerState`             | `CREATING`, `PREPARING`, `WARM`, `WAITING_FOR_WORK`, `BUSY`, `DELETING`, `DELETED`, `FAILED`                                                                                  |
| `RunnerEndReason`         | `WORK_DONE`, `SURPLUS`, `WARM_RECLAIMED`, `DELETED_BY_USER`, `START_FAILED`, `SANDBOX_ENDED`, `CLEAN_UP`                                                                      |
| `OwnSandLinkState`        | `ACTIVE`, `INVALID`                                                                                                                                                           |
| `GitHubInstallationState` | `ACTIVE`, `SUSPENDED`, `DELETED`                                                                                                                                              |

- `ENDED`, `CANCELED`, `EXPIRED` and `FAILED` are terminal request states. `DELETED` and `FAILED` are terminal runner states.
- `STARTING` means that a runner was assigned to an agent session request and is not yet `BUSY`. `GITHUB_JOB` requests go from `QUEUED` to `RUNNING` directly, because GitHub assigns the job.

## Storage

The schemas define no storage. The [control plane contract](servers-tide-api-foundation.md#storage) owns persistence.

## Security

- No response message carries a credential, a JIT configuration or a work order.
- Invitation secrets appear only in the `CreateInvitation` response.

## Logging

Generated code does not log. Components never log credential fields of request messages.

## Build and Test

- Generate the Go bindings and the TypeScript code with the repository generation command and commit the Go bindings. Record the commands with the implementation.
- Add a lint and breaking-change check for `delino.tide.v1` together with the OwnSand protobuf checks.

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
