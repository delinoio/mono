# protos-delino-accounts API contract

## Scope

This contract owns the Connect RPC schemas of [`delino-accounts`](project-delino-accounts.md):

- `protos/delino/accounts/v1/` (`planned`): `.proto` files of the protobuf package `delino.accounts.v1`.
- The generated Go bindings (`planned`), committed next to the schemas: message code in the Go package `github.com/delinoio/mono/protos/delino/accounts/v1` (package name `accountsv1`) and Connect handlers and clients in its `accountsv1connect` subpackage.
- The generated TypeScript bindings (`planned`) for the [web app](apps-delino-accounts-foundation.md) in `apps/delino-accounts/src/gen/`, with their template `apps/delino-accounts/buf.gen.yaml`. `scripts/protos.sh` generates and checks them together with the Go bindings, as it does for the OwnSand schemas.

The schemas share `protos/buf.yaml`, `protos/buf.gen.yaml` and `scripts/protos.sh` with the OwnSand schemas. The [OwnSand protos contract](protos-ownsand-api-contract.md#build-and-test) owns these tools and their versions.

The schemas do not exist yet. The change that adds the first schema changes the `planned` paths above, and the protos path in the [project index](project-delino-accounts.md#domain-ownership-map), to actual paths, and adds `delino/accounts` to the generation and drift check of `scripts/protos.sh`.

Schema files:

| File                    | Content                                                                                                                                                                                                                                                    |
| ----------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `billing_account.proto` | The package-wide rules, `BillingAccountService`, `BillingAccountRole`, `InvitationState`, `BillingAccount`, `BillingAccountMember` and `BillingAccountInvitation`.                                                                                         |
| `credit.proto`          | `CreditService`, `SubscriptionStatus`, `LedgerEntryKind`, `LedgerEntry`, `CreditGrant`, `Subscription`, `AutoTopUpSettings`, `AlertSettings` and `Invoice`.                                                                                                |
| `usage.proto`           | `UsageService` (for services), `BillingUsageService` (for users), `BillingStatus`, `BillingStatusReason`, `ServiceId`, `Meter`, `OrganizationBillingStatus`, `UsageReport`, `UsageReportResult`, `MeterUsage`, `LinkedOrganization` and `UsageSummaryRow`. |
| `schema_test.go`        | Schema checks; see [Build and Test](#build-and-test).                                                                                                                                                                                                      |

The schemas are the single source of truth for the API of [`accounts-api`](servers-accounts-api-foundation.md). Their comments carry the exact field rules; this contract records the rules that other components depend on. The API server, the web app and the consuming services, such as [`ownsand-api`](servers-ownsand-api-foundation.md), use them. The schemas and the generated code use Apache-2.0.

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
- Credentials: Logto access tokens for the API resource `https://accounts.api.delino.io`. A missing, expired or otherwise invalid token, and an ID token, fail with `unauthenticated`. A valid token without the scope of the service fails with `permission_denied`.
- Identifiers that `accounts-api` generates are UUID v7 values in canonical text form in `string` fields named `<entity>_id`. The server accepts either hex case and always returns lowercase text. A malformed identifier fails with `invalid_argument`. User ID fields (`user_id`, `inviter_user_id`) hold the Logto `sub` unchanged as an opaque string. Fields named `paddle_*_id` hold Paddle identifiers unchanged.
- `organization_id` in the service-facing messages is the organization ID of the consuming service, unchanged: 1 to 128 bytes of printable ASCII. It is unique only together with `service`. OwnSand sends its lowercase UUID v7 text.
- Times use `google.protobuf.Timestamp`.
- Money: every amount is a signed `int64` micro-USD field with a `_usd_micros` suffix, according to the [billing contract](repository-billing-contract.md#precision). A negative balance is debt. Paddle charges whole USD cents, so every amount that a user enters must be a whole number of cents (a multiple of 10,000 micro-USD); other values fail with `invalid_argument`.
- Every enum has a zero value `<ENUM>_UNSPECIFIED`. The server rejects `UNSPECIFIED` in a required enum field with `invalid_argument`.
- List RPCs take `page_size` (0 selects the default 50; values above 200 are lowered to 200; a negative value fails with `invalid_argument`) and an opaque `page_token` from the previous response. A token that the server did not issue for the same request fails with `invalid_argument`. An empty `next_page_token` means that no more results exist.
- Request and response messages follow the buf `STANDARD` lint rules: each RPC has its own `<Rpc>Request` and `<Rpc>Response` message.
- Errors use Connect error codes. Messages never contain secrets, tokens or payment data.

### Services and scopes

The server is the Logto API resource `https://accounts.api.delino.io`. Each service requires one scope:

| Scope                   | Token kind                                                 | Services                                                           |
| ----------------------- | ---------------------------------------------------------- | ------------------------------------------------------------------ |
| `accounts:billing`      | End-user access token; the token's `sub` is the caller     | `BillingAccountService`, `CreditService` and `BillingUsageService` |
| `accounts:report-usage` | Machine-to-machine access token (Logto client credentials) | `UsageService`                                                     |

- A user token on `UsageService` and a machine-to-machine token on a user service fail with `permission_denied`. The [API server contract](servers-accounts-api-foundation.md#authentication) owns the token checks. Roles inside a billing account come from `accounts-api` tables, not from token scopes.
- `accounts-api` maps each machine-to-machine client to the `ServiceId` values that it may use. A `UsageService` request for another service fails with `permission_denied`.
- Billing account access: for a request that names a billing account, a caller that is not a member gets `not_found`, also when the account exists, so that callers cannot discover other billing accounts. A member whose role does not allow the action gets `permission_denied`. A deleted billing account is `not_found` for every RPC.

| Service                 | RPC                             | Caller                                                               | Purpose and rules                                                                                                                                                                                                                                                                                                                                                                            |
| ----------------------- | ------------------------------- | -------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `BillingAccountService` | `CreateBillingAccount`          | any signed-in user                                                   | Create a billing account. The caller becomes its `owner`. A new billing account has a zero balance, no subscription and automatic top-up off.                                                                                                                                                                                                                                                |
|                         | `GetBillingAccount`             | member                                                               | Read one billing account with the caller's role (`caller_role`).                                                                                                                                                                                                                                                                                                                             |
|                         | `ListBillingAccounts`           | any signed-in user                                                   | List the billing accounts in which the caller is a member, in creation order.                                                                                                                                                                                                                                                                                                                |
|                         | `UpdateBillingAccount`          | `owner`, `admin`                                                     | Rename the billing account.                                                                                                                                                                                                                                                                                                                                                                  |
|                         | `DeleteBillingAccount`          | `owner`                                                              | Delete the billing account. `failed_precondition` while the balance is negative or an organization of any service is linked. Deletion cancels the Paddle subscription at once, forfeits the remaining credit and ends every membership and pending invitation. Financial records and audit events stay for 7 years.                                                                          |
|                         | `CreateInvitation`              | `owner`, `admin`                                                     | Invite an email address with the role `ADMIN` or `VIEWER` and send the invitation email. The invitation expires 7 days after it is created. `already_exists` while a pending invitation for the same address exists in the billing account. No response returns the token.                                                                                                                   |
|                         | `ListInvitations`               | `owner`, `admin`                                                     | List the pending invitations, newest first, without tokens.                                                                                                                                                                                                                                                                                                                                  |
|                         | `RevokeInvitation`              | `owner`, `admin`                                                     | Revoke a pending invitation. `failed_precondition` when the invitation is not pending.                                                                                                                                                                                                                                                                                                       |
|                         | `AcceptInvitation`              | any signed-in user                                                   | Join with the token from the invitation email. The caller's Logto primary email must equal the invited address (ASCII case folding); otherwise `permission_denied`, and the invitation stays pending. An unknown token is `not_found`; an accepted, revoked or expired invitation is `failed_precondition`; an existing member gets `already_exists`, and the invitation stays pending.      |
|                         | `ListMembers`                   | member                                                               | List members, roles and Logto display names (empty when unavailable), in join order.                                                                                                                                                                                                                                                                                                         |
|                         | `UpdateMemberRole`              | `owner`, `admin`                                                     | Change a member role between `ADMIN` and `VIEWER`. The owner's role cannot be changed (`failed_precondition`); a user that is not a member is `not_found`.                                                                                                                                                                                                                                   |
|                         | `RemoveMember`                  | `owner`, `admin`; every `admin` and `viewer` for itself              | Remove an `admin` or `viewer`. The owner cannot be removed (`failed_precondition`). A change of members does not change the organization links.                                                                                                                                                                                                                                              |
|                         | `TransferOwnership`             | `owner`                                                              | Make another existing member the `owner`; the previous owner becomes `ADMIN`. A user that is not a member is `not_found`; the caller's own user ID is `invalid_argument`.                                                                                                                                                                                                                    |
| `CreditService`         | `GetBalance`                    | member                                                               | Read the balance and the unexpired credit grants with remaining credit, in consumption order (`expire_time` ascending).                                                                                                                                                                                                                                                                      |
|                         | `ListLedgerEntries`             | member                                                               | List ledger entries, newest first.                                                                                                                                                                                                                                                                                                                                                           |
|                         | `GetSubscription`               | member                                                               | Read the `Subscription`: status, `cancel_at_period_end`, `current_period_end` and `price_usd_micros` (10 USD).                                                                                                                                                                                                                                                                               |
|                         | `StartSubscription`             | `owner`, `admin`                                                     | Create the Paddle checkout transaction of the $10/month subscription (billing account ID in its custom data) and return its `paddle_transaction_id` for the Paddle.js checkout. The status becomes `ACTIVE` only after the verified payment webhook. While `PENDING`, the call returns the open checkout transaction again. `already_exists` while `ACTIVE` or `PAST_DUE`.                   |
|                         | `CancelSubscription`            | `owner`, `admin`                                                     | Cancel at the end of the paid period; the subscription stays `ACTIVE` until `current_period_end`. A repeated call changes nothing. `failed_precondition` unless the status is `ACTIVE` or `PAST_DUE`.                                                                                                                                                                                        |
|                         | `ResumeSubscription`            | `owner`, `admin`                                                     | Remove a scheduled cancellation before the period ends. A call without a scheduled cancellation changes nothing. `failed_precondition` unless the status is `ACTIVE` or `PAST_DUE`.                                                                                                                                                                                                          |
|                         | `CreateTopUp`                   | `owner`, `admin`                                                     | Buy credit with a one-time charge on the subscription's payment method. See [top-ups](#top-ups).                                                                                                                                                                                                                                                                                             |
|                         | `GetAutoTopUpSettings`          | member                                                               | Read the automatic top-up settings, `consecutive_failures` and `month_to_date_usd_micros`.                                                                                                                                                                                                                                                                                                   |
|                         | `UpdateAutoTopUpSettings`       | `owner`, `admin`                                                     | Replace the automatic top-up settings. See [top-ups](#top-ups).                                                                                                                                                                                                                                                                                                                              |
|                         | `GetAlertSettings`              | member                                                               | Read the low-balance alert threshold.                                                                                                                                                                                                                                                                                                                                                        |
|                         | `UpdateAlertSettings`           | `owner`, `admin`                                                     | Replace the low-balance alert threshold: at least 0, whole cents (default 10 USD).                                                                                                                                                                                                                                                                                                           |
|                         | `ListInvoices`                  | `owner`, `admin`                                                     | List the completed Paddle transactions, newest first, with the kind and the credit amount (tax excluded). Owner and admin only, because invoices carry the payer's address. `Invoice` has no URL.                                                                                                                                                                                            |
|                         | `GetInvoiceUrl`                 | `owner`, `admin`                                                     | Return a short-lived link to the Paddle invoice PDF of one transaction, from Paddle on demand. A transaction of another billing account is `not_found`; a transaction without an invoice yet is `failed_precondition`; Paddle unavailability is `unavailable`.                                                                                                                               |
| `BillingUsageService`   | `ListLinkedOrganizations`       | member                                                               | List the organizations that are linked now, in link order, with `organization_name`, `link_time` and the current `OrganizationBillingStatus`.                                                                                                                                                                                                                                                |
|                         | `ListUsageSummary`              | member                                                               | Summarize the usage charged to the billing account in one UTC calendar month (`year` 2000 to 9999, `month` 1 to 12; a future month returns no rows) by service, organization and meter, with `total_amount_usd_micros` over all pages.                                                                                                                                                       |
| `UsageService`          | `ReportUsage`                   | service                                                              | Report 1 to 500 usage aggregates. See [usage reports](#usage-reports).                                                                                                                                                                                                                                                                                                                       |
|                         | `ListBillingStatusChanges`      | service                                                              | Poll the statuses of the service's linked organizations. See [billing status](#billing-status).                                                                                                                                                                                                                                                                                              |
|                         | `LinkOrganization`              | service, for a user who is `owner` or `admin` of the billing account | Link an organization of the service to a billing account. See [links](#links).                                                                                                                                                                                                                                                                                                               |
|                         | `UnlinkOrganization`            | service                                                              | Remove the link of an organization. See [links](#links).                                                                                                                                                                                                                                                                                                                                     |
|                         | `ListManageableBillingAccounts` | service, for a user                                                  | List the billing accounts in which the user is `owner` or `admin`, in creation order; `caller_role` holds the user's role.                                                                                                                                                                                                                                                                   |
|                         | `GetOrganizationBilling`        | service                                                              | Read the linked billing account ID and name, its balance, the status, `month_start` and one `MeterUsage` for every meter of the service (in `Meter` order, zero without usage) for windows that start in the current UTC month. It counts every report of the organization, also reports charged to a former billing account or unattributed. An organization without a link is `not_found`. |

- Field limits: billing account names are 1 to 100 Unicode code points after the server removes white space at the start and the end, without control characters. Invitation email addresses are `local@domain` with at most 254 bytes. `LinkOrganization.organization_name` is at most 200 bytes of UTF-8 without control characters and may be empty. Other values fail with `invalid_argument`.
- Invitation tokens: `bai_` followed by 32 random bytes in unpadded base64url. The token appears only in the invitation email link and in `AcceptInvitationRequest.invitation_token`. `accounts-api` stores only its SHA-256 hash.

### Top-ups

- `CreateTopUp` charges `amount_usd_micros` with Paddle `POST /subscriptions/{id}/charge` (`effective_from: immediately`). The amount is at least $25 and at most $10,000, in whole cents; other values fail with `invalid_argument`. The call fails with `failed_precondition` unless the subscription is `ACTIVE`, and with `failed_precondition` (a message without payment data) when Paddle rejects the charge; Paddle unavailability fails with `unavailable`.
- `CreateTopUpRequest.idempotency_key` is a UUID v7 that the caller generates for each logical top-up and reuses for every retry. A repeated key of the same billing account within 24 hours returns the first result and charges nothing again; a repeated key with another amount fails with `invalid_argument`.
- The response is empty: the credit arrives later with the verified payment webhook as a `MANUAL_TOP_UP` ledger entry and credit grant.
- `UpdateAutoTopUpSettings` replaces every input field. With `enabled` true, a threshold below $10, an amount below $25, a monthly cap below the amount (including a missing cap) or an amount that is not whole cents fails with `invalid_argument`. With `enabled` false, the values must be zero or non-negative whole cents and are kept for a later enable. `consecutive_failures` and `month_to_date_usd_micros` are output only. Enabling the setting and a successful top-up reset `consecutive_failures` to 0; after 2 failures in a row, `accounts-api` sets `enabled` to false.

### Usage reports

- A `UsageReport` carries the billing subject (`service` and `organization_id`), the `meter`, the `window_start`, the `quantity` and the `amount_usd_micros` that the service computed with its own prices and rounding. `accounts-api` does not price usage: each service owns its prices, according to the [billing contract](repository-billing-contract.md#billing-ownership).
- Validation: the meter must belong to the same service. `window_start` is aligned to 5 minutes in UTC (whole seconds that are a multiple of 300, zero nanoseconds). The window end (`window_start` plus 5 minutes) must not be later than the server time plus 1 minute; there is no lower limit, so a service can report old windows after an outage. `quantity` and `amount_usd_micros` must not be negative.
- The server validates every report of a request before it applies any; one invalid report fails the whole request with `invalid_argument` that names its index. A failure after validation can leave some reports applied; a retry of the whole request is safe.
- Idempotency key: (`service`, `organization_id`, `meter`, `window_start`). A repeated key, also within one request, returns the first result with `duplicate` set and deducts nothing again. A repeated key with another quantity or amount keeps the first values, and `accounts-api` logs the mismatch.
- Attribution: a report is charged to the billing account whose link of the organization overlapped the window `[window_start, window_start + 5 minutes)`. When several links overlapped it, the link that was active at the window end wins. So the usage of an organization that was unlinked or deleted stays on its former billing account.
- Without an overlapping link, `accounts-api` stores the report as unattributed usage (kept 7 years), deducts nothing and returns `unattributed` set, so the service marks the usage as reported. Usage is never dropped on either side.
- `UsageReportResult` echoes the key and returns `duplicate`, `unattributed`, the `billing_account_id` that was charged and the `ledger_entry_id` of the `USAGE` entry (both empty when unattributed). Results are in request order.
- The response also returns the current `OrganizationBillingStatus` of each linked organization that the request names, one entry per organization, after the deductions.

### Billing status

- `OrganizationBillingStatus` carries `service`, `organization_id`, `billing_account_id`, `status`, `reason`, `blocked_since`, `suspend_time` and `sequence`. `accounts-api` keeps a status only for linked organizations. The consuming service handles organizations without a link.
- Status rules for a linked organization:

| Status      | When                                                                                                                                                                         |
| ----------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ALLOWED`   | The subscription of the billing account is `ACTIVE` and its balance is greater than zero.                                                                                    |
| `BLOCKED`   | Otherwise (non-payment). `blocked_since` is the start of the uninterrupted non-payment of the billing account, or the link time when that is later.                          |
| `SUSPENDED` | The non-payment continued until `suspend_time`, which is `blocked_since` plus 3 days. `accounts-api` records this change when `suspend_time` passes, without a usage report. |

- Only `ACTIVE` is an active subscription. A canceled subscription stays `ACTIVE` until `current_period_end`. `NONE`, `PENDING`, `PAST_DUE` and `CANCELED` are not active.
- `reason` is `NONE` for `ALLOWED`. Otherwise it is `NO_ACTIVE_SUBSCRIPTION` or `NON_POSITIVE_BALANCE`; when both apply, it is `NO_ACTIVE_SUBSCRIPTION`. `blocked_since` and `suspend_time` are unset for `ALLOWED`.
- `BLOCKED` and `SUSPENDED` change back to `ALLOWED` when the balance is greater than zero and the subscription is `ACTIVE`.
- `sequence` is greater than zero and increases with every change of any other status field for any organization of the service.
- `ListBillingStatusChanges(service, after_sequence, page_size)` returns the current statuses whose `sequence` is greater than `after_sequence`, in `sequence` order, with at most one entry per organization (its latest status). `after_sequence` 0 returns every linked organization of the service; a negative value fails with `invalid_argument`. Only linked organizations appear: an unlink adds no entry. `next_sequence` is the highest returned `sequence`, or `after_sequence` when nothing is returned; fewer than `page_size` statuses mean that the caller is up to date.
- A caller keeps the status with the highest `sequence` and ignores a status whose `billing_account_id` differs from the link that it knows, which can arrive after an unlink.

### Links

- `LinkOrganization(service, organization_id, billing_account_id, user_id, organization_name)` links an organization on behalf of a user. The user must be `owner` or `admin` of the billing account (`permission_denied` otherwise). An unknown or deleted billing account, or one in which the user has no membership, is `not_found`. An organization that is linked to another billing account fails with `already_exists`; a repeated link to the same billing account changes only `organization_name`. The calling service checks its own organization role first. The response returns the organization's `OrganizationBillingStatus` after the link and the `billing_account_name`.
- `UnlinkOrganization(service, organization_id)` is idempotent: an organization without a link succeeds and changes nothing. Usage of earlier windows stays attributed to the former billing account.
- The link history (link and unlink times) is kept for attribution and for the 7-year retention. A change of billing account members has no effect on existing links. `organization_name` is the display name for the billing account members; it is empty when the service sends none.

### Enums

| Enum                  | Values (without the enum prefix)                                                           |
| --------------------- | ------------------------------------------------------------------------------------------ |
| `BillingAccountRole`  | `OWNER`, `ADMIN`, `VIEWER`                                                                 |
| `InvitationState`     | `PENDING`, `ACCEPTED`, `REVOKED`, `EXPIRED`                                                |
| `SubscriptionStatus`  | `NONE`, `PENDING`, `ACTIVE`, `PAST_DUE`, `CANCELED`                                        |
| `LedgerEntryKind`     | `SUBSCRIPTION_CREDIT`, `MANUAL_TOP_UP`, `AUTO_TOP_UP`, `USAGE`, `EXPIRY`, `REFUND`         |
| `BillingStatus`       | `ALLOWED`, `BLOCKED`, `SUSPENDED`                                                          |
| `BillingStatusReason` | `NONE`, `NO_ACTIVE_SUBSCRIPTION`, `NON_POSITIVE_BALANCE`                                   |
| `ServiceId`           | `OWNSAND`                                                                                  |
| `Meter`               | `OWNSAND_RELAY_EGRESS`, `OWNSAND_R2_STORAGE`, `OWNSAND_CLASS_A_RPC`, `OWNSAND_CLASS_B_RPC` |

- Stable identifiers: the stable identifier of a `ServiceId` value is its name without the prefix, in lowercase (`ownsand`); `LedgerEntry.service` and the database store it. The stable identifier of a meter is `<service>.<meter>` in lowercase: `ownsand.relay_egress`, `ownsand.r2_storage`, `ownsand.class_a_rpc` and `ownsand.class_b_rpc`; `LedgerEntry.meter` and the database store it. Each `Meter` value starts with the name of its `ServiceId` value.
- Quantity units: bytes for relay egress, byte-seconds for R2 storage and calls for the RPC meters. The [OwnSand project index](project-ownsand.md#billing) owns the prices.
- A new consuming service adds its `ServiceId` value and its meters.

Ledger and credit messages:

- `LedgerEntry` is append-only and kept for 7 years, also after the billing account is deleted. It carries `ledger_entry_id`, `billing_account_id`, `kind`, the signed `amount_usd_micros` (positive for credits; negative or zero for `USAGE`, `EXPIRY` and `REFUND`), `balance_after_usd_micros`, `create_time`, `paddle_transaction_id` (purchases and refunds) and `credit_grant_id` (the grant that a purchase created or that an `EXPIRY` or `REFUND` entry reduced). `USAGE` entries also carry `service`, `organization_id`, `meter`, `window_start` and `quantity`.
- `CreditGrant` carries `credit_grant_id`, its `source` (`SUBSCRIPTION_CREDIT`, `MANUAL_TOP_UP` or `AUTO_TOP_UP`), `original_usd_micros`, `remaining_usd_micros`, `paddle_transaction_id`, `create_time` and `expire_time` (12 months after `create_time`).
- `Invoice` carries `paddle_transaction_id`, `kind`, `amount_usd_micros` (the credit amount, tax excluded) and `create_time`; `GetInvoiceUrl` returns the PDF link.

## Storage

The schemas define no storage. The [API server contract](servers-accounts-api-foundation.md#storage) owns persisted data.

## Security

- The invitation token appears only in the invitation email link and in `AcceptInvitation`. Its field comment says `Secret: never log.` Generated code that logs messages must redact it. The invoice link of `GetInvoiceUrl` is short-lived; its field comment says `Never log.`
- No message carries card data. Paddle customer, subscription, transaction and event IDs are references, not secrets.
- Email addresses appear only in invitation messages. Components do not log them at the `info` level.

## Logging

The schemas define no logging. Components log RPC names, identifiers, enum values, amounts and durations, never secret fields, email bodies or card data.

## Build and Test

- Generation, lint, formatting, breaking-change and drift checks: `scripts/protos.sh generate`, `check` and `breaking`, as the [OwnSand protos contract](protos-ownsand-api-contract.md#build-and-test) describes; they cover the `delino/accounts` schemas with the Go bindings. The change that adds the TypeScript bindings of the web app extends the script and its fixtures to the web app template.
- `protos/delino/accounts/v1/schema_test.go` checks that every enum of the package has an `_UNSPECIFIED` zero value, that every field whose name mentions `usd` is a singular `int64` that ends with `_usd_micros`, and that each `Meter` value maps to its stable `<service>.<meter>` identifier, so a renamed or removed meter fails.
- The OwnSand `schema_test.go` checks that each OwnSand `BillingMeter` value has exactly one `Meter` value of OwnSand with the same name, and the reverse (see the [OwnSand protos contract](protos-ownsand-api-contract.md#build-and-test)).

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
