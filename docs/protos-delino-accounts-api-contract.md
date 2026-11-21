# protos-delino-accounts API contract

## Scope

This contract owns the Connect RPC schemas of [`delino-accounts`](project-delino-accounts.md):

- `protos/delino/accounts/v1/` (`planned`): `.proto` files of the protobuf package `delino.accounts.v1`.
- The generated Go bindings (`planned`), committed next to the schemas: message code in the Go package `github.com/delinoio/mono/protos/delino/accounts/v1` (package name `accountsv1`) and Connect handlers and clients in its `accountsv1connect` subpackage.
- The generated TypeScript bindings (`planned`) for the [web app](apps-delino-accounts-foundation.md) in `apps/delino-accounts/src/gen/`, with their template `apps/delino-accounts/buf.gen.yaml`. `scripts/protos.sh` generates and checks them together with the Go bindings, as it does for the OwnSand schemas.

The schemas share `protos/buf.yaml`, `protos/buf.gen.yaml` and `scripts/protos.sh` with the OwnSand schemas. The [OwnSand protos contract](protos-ownsand-api-contract.md#build-and-test) owns these tools and their versions.

The schemas do not exist yet. The change that adds the first schema changes the `planned` paths above, and the protos path in the [project index](project-delino-accounts.md#domain-ownership-map), to actual paths, and adds `delino/accounts` to the generation and drift check of `scripts/protos.sh`.

Schema files:

| File                    | Content                                                                                                                                                                                             |
| ----------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `billing_account.proto` | `BillingAccountService`, `BillingAccountRole`, `BillingAccount`, `BillingAccountMember` and `BillingAccountInvitation`.                                                                             |
| `credit.proto`          | `CreditService`, `SubscriptionStatus`, `LedgerEntryKind`, `LedgerEntry`, `CreditGrant`, `AutoTopUpSettings` and `Invoice`.                                                                          |
| `usage.proto`           | `UsageService` (for services), `BillingUsageService` (for users), `BillingStatus`, `BillingStatusReason`, `ServiceId`, `Meter`, `OrganizationBillingStatus`, `UsageReport` and `UsageReportResult`. |

The schemas are the single source of truth for the API of [`accounts-api`](servers-accounts-api-foundation.md). The API server, the web app and the consuming services, such as [`ownsand-api`](servers-ownsand-api-foundation.md), use them. The schemas and the generated code use Apache-2.0.

## Runtime and Language

- Language: Protocol Buffers (proto3), compiled for Connect RPC.
- Go bindings: committed to the repository in the root Go module `github.com/delinoio/mono`. Go consumers import them directly and do not run code generation. `servers/accounts-api/` and `servers/ownsand-api/` (both FSL-1.1-ALv2) may import these Apache-2.0 bindings.
- TypeScript bindings: generated with `protoc-gen-es` and committed in `apps/delino-accounts/src/gen/`. The web app pins `@bufbuild/protoc-gen-es` at the same exact version as the [OwnSand SDK](packages-ownsand-sdk-foundation.md#dependencies-and-integrations), and keeps it equal to its `@bufbuild/protobuf` runtime version.
- Code generation tools: the buf CLI, `protoc-gen-go` and `protoc-gen-connect-go` at the versions that the [OwnSand protos contract](protos-ownsand-api-contract.md#runtime-and-language) records.

## Users and Operators

- Signed-in Delino users, through the [web app](apps-delino-accounts-foundation.md): `BillingAccountService`, `CreditService` and `BillingUsageService`.
- Delino services, server-to-server with a Logto machine-to-machine access token: `UsageService`. The first consumer is [`ownsand-api`](servers-ownsand-api-foundation.md#billing).

## Interfaces and Contracts

### General rules

- Package: `delino.accounts.v1`. A breaking change needs a new package version (`v2`). Within `v1`, do not reuse or renumber fields, do not change field types, and reserve the numbers and names of removed fields.
- Identifiers that `accounts-api` generates are UUID v7 values in canonical lowercase text form in `string` fields named `<entity>_id`. The server accepts either hex case and always returns lowercase text. User ID fields (`user_id`, `inviter_user_id`) hold the Logto `sub` unchanged as an opaque string.
- `organization_id` in the service-facing messages is the organization ID of the consuming service, as an opaque string. It is unique only together with `service`. OwnSand sends its lowercase UUID v7 text.
- Times use `google.protobuf.Timestamp`.
- Money: every amount is a signed `int64` micro-USD field with a `_usd_micros` suffix, according to the [billing contract](repository-billing-contract.md#precision). A negative balance is debt.
- Every enum has a zero value `<ENUM>_UNSPECIFIED`. The server rejects `UNSPECIFIED` in a required enum field with `invalid_argument`.
- List RPCs use `page_size` (default 50, maximum 200) and an opaque `page_token`.
- Request and response messages follow the buf `STANDARD` lint rules: each RPC has its own `<Rpc>Request` and `<Rpc>Response` message.
- Errors use Connect error codes. Messages never contain secrets, tokens, card data or email bodies.

### Services and scopes

The server is the Logto API resource `https://accounts.api.delino.io`. Each service requires one scope:

| Scope                   | Token kind                                                 | Services                                                           |
| ----------------------- | ---------------------------------------------------------- | ------------------------------------------------------------------ |
| `accounts:billing`      | End-user access token                                      | `BillingAccountService`, `CreditService` and `BillingUsageService` |
| `accounts:report-usage` | Machine-to-machine access token (Logto client credentials) | `UsageService`                                                     |

A user token on `UsageService` and a machine-to-machine token on a user service fail with `permission_denied`. The [API server contract](servers-accounts-api-foundation.md#authentication) owns the token checks. Roles inside a billing account come from `accounts-api` tables, not from token scopes.

| Service                 | RPC                             | Caller                                                               | Purpose                                                                                                                                           |
| ----------------------- | ------------------------------- | -------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| `BillingAccountService` | `CreateBillingAccount`          | any signed-in user                                                   | Create a billing account with a name. The caller becomes its `owner`.                                                                             |
|                         | `GetBillingAccount`             | member                                                               | Read one billing account with the caller's role (`caller_role`).                                                                                  |
|                         | `ListBillingAccounts`           | any signed-in user                                                   | List the billing accounts in which the caller is a member.                                                                                        |
|                         | `UpdateBillingAccount`          | `owner`, `admin`                                                     | Change the name.                                                                                                                                  |
|                         | `DeleteBillingAccount`          | `owner`                                                              | Delete the billing account. See [billing accounts](servers-accounts-api-foundation.md#billing-accounts-and-members).                              |
|                         | `CreateInvitation`              | `owner`, `admin`                                                     | Invite an email address with the role `admin` or `viewer`, and send the invitation email.                                                         |
|                         | `ListInvitations`               | `owner`, `admin`                                                     | List pending invitations without tokens.                                                                                                          |
|                         | `RevokeInvitation`              | `owner`, `admin`                                                     | Revoke a pending invitation.                                                                                                                      |
|                         | `AcceptInvitation`              | signed-in user whose Logto primary email is the invited address      | Join the billing account with the invitation token from the email link.                                                                           |
|                         | `ListMembers`                   | member                                                               | List members and roles.                                                                                                                           |
|                         | `UpdateMemberRole`              | `owner`, `admin`                                                     | Change a member role between `admin` and `viewer`.                                                                                                |
|                         | `RemoveMember`                  | `owner`, `admin`; any member for itself except the `owner`           | Remove a member.                                                                                                                                  |
|                         | `TransferOwnership`             | `owner`                                                              | Make another existing member the `owner`. The previous owner becomes `admin`.                                                                     |
| `CreditService`         | `GetBalance`                    | member                                                               | Read the balance and the credit grants that are not expired and have a remaining amount.                                                          |
|                         | `ListLedgerEntries`             | member                                                               | List ledger entries, newest first.                                                                                                                |
|                         | `GetSubscription`               | member                                                               | Read the subscription status, `cancel_at_period_end` and `current_period_end`.                                                                    |
|                         | `StartSubscription`             | `owner`, `admin`                                                     | Create the Paddle transaction of the $10/month subscription and return its `paddle_transaction_id` for the Paddle.js checkout.                    |
|                         | `CancelSubscription`            | `owner`, `admin`                                                     | Cancel the subscription at the end of the paid period.                                                                                            |
|                         | `CreateTopUp`                   | `owner`, `admin`                                                     | Charge a manual top-up of at least $25 in whole cents.                                                                                            |
|                         | `GetAutoTopUpSettings`          | member                                                               | Read the automatic top-up settings and the count of consecutive failures.                                                                         |
|                         | `UpdateAutoTopUpSettings`       | `owner`, `admin`                                                     | Turn automatic top-up on or off and set the threshold, the amount and the monthly cap.                                                            |
|                         | `GetAlertSettings`              | member                                                               | Read the low-balance alert threshold.                                                                                                             |
|                         | `UpdateAlertSettings`           | `owner`, `admin`                                                     | Set the low-balance alert threshold.                                                                                                              |
|                         | `ListInvoices`                  | member                                                               | List the Paddle transactions of the billing account with the credit amount (tax excluded).                                                        |
|                         | `GetInvoiceUrl`                 | member                                                               | Return the Paddle invoice link of one transaction, fetched from Paddle on demand.                                                                 |
| `BillingUsageService`   | `ListLinkedOrganizations`       | member                                                               | List the organizations of every service that are linked to the billing account.                                                                   |
|                         | `ListUsageSummary`              | member                                                               | Return the usage of one UTC calendar month by service, organization and meter, with quantities and amounts.                                       |
| `UsageService`          | `ReportUsage`                   | service                                                              | Report up to 500 usage aggregates. See [usage reports](#usage-reports).                                                                           |
|                         | `ListBillingStatusChanges`      | service                                                              | Return the statuses of the service's linked organizations that changed after a cursor. See [billing status](#billing-status).                     |
|                         | `LinkOrganization`              | service, for a user who is `owner` or `admin` of the billing account | Link an organization of the service to a billing account. See [links](#links).                                                                    |
|                         | `UnlinkOrganization`            | service                                                              | Remove the link of an organization. Idempotent.                                                                                                   |
|                         | `ListManageableBillingAccounts` | service, for a user                                                  | List the billing accounts in which the user is `owner` or `admin` (ID, name and role).                                                            |
|                         | `GetOrganizationBilling`        | service                                                              | Return the linked billing account ID and name, the balance, the status and the usage of the current UTC month for each meter of the organization. |

### Usage reports

- A `UsageReport` carries the billing subject (`service` and `organization_id`), the `meter`, the `window_start`, the `quantity` and the `amount_usd_micros` that the service computed with its own prices. `accounts-api` does not recompute the amount: each service owns its prices, according to the [billing contract](repository-billing-contract.md#billing-ownership).
- `window_start` is the start of a 5-minute UTC window (`[window_start, window_start + 5m)`). The meter must belong to the service. `quantity` and `amount_usd_micros` must not be negative. Other values fail the request with `invalid_argument`.
- Idempotency key: (`service`, `organization_id`, `meter`, `window_start`). A report with a key that exists returns the first result with `duplicate` set and deducts nothing again, also when its quantity or amount differs.
- Attribution: a report is charged to the billing account whose link of the organization (from the link history) overlapped the window. When several links overlapped it, the link that was active at the window end wins. So the usage of an organization that was unlinked or deleted stays on its former billing account.
- Without an overlapping link, `accounts-api` stores the report as unattributed usage, deducts nothing and returns `unattributed` set, so the service marks the usage as reported. Usage is never silently dropped on either side.
- `UsageReportResult` echoes the key and returns `duplicate`, `unattributed` and the ID of the usage ledger entry when there is one.
- The response also returns the `OrganizationBillingStatus` of each linked organization that the reports affected.

### Billing status

- `OrganizationBillingStatus` carries `service`, `organization_id`, `billing_account_id`, `status`, `reason`, `blocked_since`, `suspend_time` and `sequence`. `accounts-api` keeps a status only for linked organizations. The consuming service handles organizations without a link.
- Status rules for a linked organization:

| Status      | When                                                                                                                               |
| ----------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| `ALLOWED`   | The billing account has an active subscription and a balance above 0.                                                              |
| `BLOCKED`   | The billing account has no active subscription or a balance of 0 or less. `blocked_since` is the start of this non-payment period. |
| `SUSPENDED` | Non-payment continued without interruption for 3 days after `blocked_since`. `suspend_time` is `blocked_since` plus 3 days.        |

- An active subscription is a subscription with the status `ACTIVE`. A canceled subscription stays `ACTIVE` until the end of the paid period. `PENDING`, `PAST_DUE`, `CANCELED` and `NONE` are not active.
- `reason` is `NONE` for `ALLOWED`. Otherwise it is `NO_ACTIVE_SUBSCRIPTION` or `NON_POSITIVE_BALANCE`; when both apply, it is `NO_ACTIVE_SUBSCRIPTION`.
- `BLOCKED` and `SUSPENDED` change back to `ALLOWED` when the balance is above 0 and the subscription is active.
- Every status change gets a new `sequence` value that is larger than every earlier one. `ListBillingStatusChanges(service, after_sequence, page_size)` returns the statuses of the service's organizations with a larger `sequence`, in `sequence` order, and `next_sequence` for the next call.

### Links

- `LinkOrganization(service, organization_id, billing_account_id, user_id)` checks that `user_id` is `owner` or `admin` of the billing account (`permission_denied` otherwise) and that the billing account exists and is not deleted (`not_found` otherwise). An organization has at most one active link: a link to another billing account fails with `already_exists`, and a repeated link to the same billing account succeeds without change. The consuming service checks its own organization role before it calls.
- `UnlinkOrganization(service, organization_id)` ends the active link and succeeds without change when there is none.
- The link history (link and unlink times) is kept for attribution and for the 7-year retention. A change of billing account members has no effect on existing links.

### Enums

| Enum                  | Values (without the enum prefix)                                                           |
| --------------------- | ------------------------------------------------------------------------------------------ |
| `BillingAccountRole`  | `OWNER`, `ADMIN`, `VIEWER`                                                                 |
| `SubscriptionStatus`  | `NONE`, `PENDING`, `ACTIVE`, `PAST_DUE`, `CANCELED`                                        |
| `LedgerEntryKind`     | `SUBSCRIPTION_CREDIT`, `MANUAL_TOP_UP`, `AUTO_TOP_UP`, `USAGE`, `EXPIRY`, `REFUND`         |
| `BillingStatus`       | `ALLOWED`, `BLOCKED`, `SUSPENDED`                                                          |
| `BillingStatusReason` | `NONE`, `NO_ACTIVE_SUBSCRIPTION`, `NON_POSITIVE_BALANCE`                                   |
| `ServiceId`           | `OWNSAND`                                                                                  |
| `Meter`               | `OWNSAND_RELAY_EGRESS`, `OWNSAND_R2_STORAGE`, `OWNSAND_CLASS_A_RPC`, `OWNSAND_CLASS_B_RPC` |

Stable meter IDs, which `accounts-api` stores and `LedgerEntry.meter` returns: `ownsand.relay_egress`, `ownsand.r2_storage`, `ownsand.class_a_rpc` and `ownsand.class_b_rpc`. The [OwnSand project index](project-ownsand.md#billing) owns their prices and units. A new consuming service adds its `ServiceId` value and its meters.

`LedgerEntry` carries `ledger_entry_id`, `kind`, the signed `amount_usd_micros`, `balance_after_usd_micros`, `create_time` and `paddle_transaction_id`; usage entries also carry `service`, `organization_id`, `meter` (the stable meter ID) and `window_start`. `CreditGrant` carries `credit_grant_id`, its `source` kind, `original_usd_micros`, `remaining_usd_micros`, `create_time` and `expire_time`. `AutoTopUpSettings` carries `enabled`, `threshold_usd_micros`, `amount_usd_micros`, `monthly_cap_usd_micros` and `consecutive_failures`.

## Storage

The schemas define no storage. The [API server contract](servers-accounts-api-foundation.md#storage) owns persisted data.

## Security

- The invitation token appears only in the invitation email link and in `AcceptInvitation`. Mark its field with the comment `Secret: never log.` Generated code that logs messages must redact it.
- No message carries card data. Paddle customer, subscription, transaction and event IDs are references, not secrets.
- Email addresses appear only in invitation messages. Components do not log them at the `info` level.

## Logging

The schemas define no logging. Components log RPC names, identifiers, enum values, amounts and durations, never secret fields, email bodies or card data.

## Build and Test

- Generation, lint, formatting, breaking-change and drift checks: `scripts/protos.sh generate`, `check` and `breaking`, as the [OwnSand protos contract](protos-ownsand-api-contract.md#build-and-test) describes. The change that adds the first schema extends the script and its fixtures to the `delino/accounts` schemas and the web app template.
- A schema test checks the `_UNSPECIFIED` zero value of every enum and the stable meter IDs.

## Dependencies and Integrations

- Upstream: the generation tools and runtimes that the [OwnSand protos contract](protos-ownsand-api-contract.md#dependencies-and-integrations) records.
- Downstream: [`accounts-api`](servers-accounts-api-foundation.md), [`delino-accounts-web`](apps-delino-accounts-foundation.md) and [`ownsand-api`](servers-ownsand-api-foundation.md#billing).

## Change Triggers

- Update the [API server contract](servers-accounts-api-foundation.md), the [web app contract](apps-delino-accounts-foundation.md) and the contracts of the consuming services when an RPC, message or enum that they use changes.
- Update the [OwnSand project index](project-ownsand.md#billing) and this contract together when an OwnSand meter changes.
- Update [project-delino-accounts.md](project-delino-accounts.md) when the package version or the protos path changes.
- Update `protos/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-delino-accounts.md](project-delino-accounts.md)
- [servers-accounts-api-foundation.md](servers-accounts-api-foundation.md)
- [protos-ownsand-api-contract.md](protos-ownsand-api-contract.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-billing-contract.md](repository-billing-contract.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [Connect protocol](https://connectrpc.com/docs/protocol)
