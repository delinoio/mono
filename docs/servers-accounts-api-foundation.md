# servers-accounts-api foundation contract

## Scope

This contract owns the `accounts-api` component of [`delino-accounts`](project-delino-accounts.md) at `servers/accounts-api/`:

- `servers/accounts-api/cmd/accounts-api/`: process entry point.
- `servers/accounts-api/internal/config/`: environment configuration loading and validation.
- `servers/accounts-api/internal/server/`: HTTP handler, routes, middleware and the connection observer that logs requests rejected before the handler runs.
- `servers/accounts-api/.env.example`: configuration names and safe loopback defaults.
- `servers/accounts-api/package.json` and `turbo.json`: workspace task wiring and the `deploy` field.
- `servers/accounts-api/Dockerfile`: runtime image of the Droplet deployment.
- `servers/accounts-api/scripts/`: development launcher (`dev.mjs`) and its process-level tests.
- `servers/accounts-api/internal/db/` (`planned`): Postgres pool, embedded goose migrations in `internal/db/migrations/` and test databases. The billing implementation adds its other packages (authentication, Connect handlers, the credit ledger, the Paddle client, email delivery and metrics) and records them here in the change that adds each.

The API server is the system of record for account and billing information: billing accounts, members, invitations, credit grants, the ledger, subscriptions, organization links, usage reports and billing statuses. It is licensed under `FSL-1.1-ALv2` according to the [license contract](repository-license-contract.md).

## Runtime and Language

- Language: Go, in the repository root module `github.com/delinoio/mono`. The import path of the server packages is `github.com/delinoio/mono/servers/accounts-api/...`. The `go` directive in the root `go.mod` selects the toolchain version.
- Runtime: one long-running HTTP server process. It uses only the Go standard library in the current scaffold. The billing implementation adds the dependencies in [Dependencies and Integrations](#dependencies-and-integrations), background workers (automatic top-ups, credit expiry, status changes and email delivery) and an internal metrics listener on `METRICS_ADDR` (`planned`, see [Metrics](#metrics)).
- Logging: `log/slog`.
- HTTP timeouts: `newServer` in `cmd/accounts-api` sets fixed constants on the `http.Server`. They are not configuration variables, so they have no `.env.example` entry and no Turbo `passThroughEnv` entry.
  - `ReadHeaderTimeout` is 10 seconds: the time a client has to send the request headers.
  - `ReadTimeout` is 30 seconds and covers the whole request read, headers and body. `net/http` clears the header deadline after the headers arrive, so without `ReadTimeout` a client that declares a body and then sends nothing would hold the connection open forever. `ReadTimeout` must stay greater than or equal to `ReadHeaderTimeout`.
  - `WriteTimeout` is 30 seconds for writing a response.
  - `IdleTimeout` is 120 seconds: a keep-alive connection that is waiting for the next request is closed after this time.
  - `WriteTimeout` limits the lifetime of every response, so a long-lived streaming RPC must lift or extend it for that handler (`http.ResponseController.SetWriteDeadline`) in the change that adds the stream.
- Deployment target: the Droplet service `accounts-api` on the shared DigitalOcean Droplet, deployed by the [deployment workflow](repository-workflow-contract.md#deployment-workflow). The `deploy` field sets the host name `accounts.api.delino.io` and the container port `8080`. The workflow builds a static `linux/amd64` binary with `CGO_ENABLED=0`; `servers/accounts-api/turbo.json` passes `CGO_ENABLED`, `GOOS` and `GOARCH` through to the uncached `build` task. The `Dockerfile` copies `dist/accounts-api`, the FSL-1.1-ALv2 `LICENSE` (`/usr/share/licenses/accounts-api/LICENSE`) and the Go toolchain BSD-3-Clause notice (`/usr/share/licenses/accounts-api/GO-LICENSE`) into the digest-pinned `gcr.io/distroless/static-debian12:nonroot` image and runs it as the `nonroot` user. The deployment checks `GET /healthz` before the reverse proxy sends traffic to the new container, and Docker restarts the container when the process exits (restart policy `unless-stopped`).
- Configuration source: the env file `/srv/delino/env/accounts-api.env` on the Droplet, which maintainers keep. The deployment sets `ADDR` to `:8080` from the `deploy` field, so the env file must not set `ADDR`. Secrets are also in the env file; they never enter the repository or the GitHub environment.
- TLS termination: the Caddy reverse proxy on the Droplet terminates TLS for `accounts.api.delino.io` and forwards plain HTTP/1.1 to the container.

## Users and Operators

- The [`delino-accounts-web`](apps-delino-accounts-foundation.md) app, which calls the API on behalf of signed-in users.
- Other Delino services. The repository rules require every service to use this API for shared account data and for billing, including billable usage or charge reports. Record each consuming service and the RPCs that it uses here before it calls the API.
- [`ownsand-api`](servers-ownsand-api-foundation.md#billing) (`planned`), billing: reports OwnSand usage and reads billing statuses, server-to-server with its own Logto machine-to-machine credential and the scope `accounts:report-usage`. It calls every `UsageService` RPC of the [protos contract](protos-delino-accounts-api-contract.md#services-and-scopes): `ReportUsage`, `ListBillingStatusChanges`, `LinkOrganization`, `UnlinkOrganization`, `ListManageableBillingAccounts` and `GetOrganizationBilling`. `ownsand-api` checks the OwnSand organization role before it links, unlinks or reads; this API trusts the machine credential and its scope, and checks the billing account role of the user that `ownsand-api` names. No browser origin is needed.
- [`ownsand-api`](servers-ownsand-api-foundation.md) (`planned`), display names: reads the display profile (display name) of OwnSand organization members by Logto user ID, server-to-server with its own Logto machine-to-machine credential. `ownsand-api` checks the membership before it asks; this API trusts the machine credential and its scope, not the end user. This needs an `AccountService` RPC that returns the display profiles of a list of users, and a scope for it. Record the RPC and the scope here in the change that adds them. No browser origin is needed.
- Operators who deploy and monitor the server.

## Interfaces and Contracts

- Production origin: `https://accounts.api.delino.io`.
- Operational endpoints:

| Endpoint ID      | Method and path                              | Authentication               | Response                                                                |
| ---------------- | -------------------------------------------- | ---------------------------- | ----------------------------------------------------------------------- |
| `healthz`        | `GET /healthz`                               | none                         | `200` with `{"status":"ok"}` while the process can serve HTTP requests. |
| `paddle-webhook` | `POST /webhooks/paddle` (`planned`)          | Paddle signature             | See [Paddle webhook](#paddle-webhook).                                  |
| `metrics`        | `GET /metrics` on `METRICS_ADDR` (`planned`) | none; internal listener only | Prometheus metrics. See [Metrics](#metrics).                            |

- Connect-only exception: `POST /webhooks/paddle` is a plain HTTP endpoint, because Paddle does not call Connect RPC. It is the only plain HTTP endpoint besides the operational endpoints.
- Business APIs (`planned`): Connect RPC services generated from `protos/delino/accounts/v1/`. The [protos contract](protos-delino-accounts-api-contract.md) owns every RPC, message and enum.

| Service ID              | Status  | Responsibility                                                                                                 |
| ----------------------- | ------- | -------------------------------------------------------------------------------------------------------------- |
| `AccountService`        | planned | Read and update the profile and preferences of the signed-in Logto user.                                       |
| `BillingAccountService` | planned | Billing accounts, members, roles, email invitations and ownership transfer.                                    |
| `CreditService`         | planned | Balance, credit grants, ledger, the Paddle subscription, manual and automatic top-ups, alerts and invoices.    |
| `BillingUsageService`   | planned | Linked organizations and the monthly usage of a billing account by service, organization and meter, for users. |
| `UsageService`          | planned | Usage reports, billing statuses, organization links and organization billing reads, for services only.         |

### Authentication

- Status: `planned` with the first business API.
- The server is the Logto API resource `https://accounts.api.delino.io`. It validates every access token according to the [authentication contract](repository-auth-contract.md#token-validation): signature, issuer, expiry, the audience `https://accounts.api.delino.io` and the scope of the RPC. ID tokens are rejected.

| Scope                   | Token kind                      | Services                                                           |
| ----------------------- | ------------------------------- | ------------------------------------------------------------------ |
| `accounts:billing`      | End-user access token           | `BillingAccountService`, `CreditService` and `BillingUsageService` |
| `accounts:report-usage` | Machine-to-machine access token | `UsageService`                                                     |

- A missing, expired or otherwise invalid token, and an ID token, fail with `unauthenticated`. A valid token without the scope of the service fails with `permission_denied`.
- Logto client credentials tokens carry the application ID as `sub`, equal to `client_id`. The server accepts `UsageService` calls only with such a machine-to-machine token, and rejects machine-to-machine tokens on the user services, with `permission_denied` in both cases. Only the machine-to-machine applications of consuming services get the role that holds `accounts:report-usage`.
- Service mapping: the server maps each machine-to-machine client ID to the `ServiceId` values that it may use, and a `UsageService` request for another service fails with `permission_denied`. The mapping is configuration; record its variable here in the change that adds it.
- Logto application registrations: the [web app](apps-delino-accounts-foundation.md) is a single-page application that requests `accounts:billing`. Each consuming service has its own machine-to-machine application. The server itself has a machine-to-machine application for the Logto Management API (`LOGTO_M2M_CLIENT_ID`), which it uses only to read the primary email of the caller of `AcceptInvitation`.
- Browser access (`planned` with the first business API): CORS allows only the origins in `CORS_ORIGINS`: `https://accounts.delino.io` in production and the loopback development origin locally.
- The Paddle webhook accepts no Logto credential. Its Paddle signature authenticates only the delivery of an event from Paddle. It gives no access to any RPC.

### Billing accounts and members

- A billing account is a separate entity. Any signed-in Logto user can create one and becomes its `owner`.

| Action                                                                                          | `owner` | `admin` | `viewer` |
| ----------------------------------------------------------------------------------------------- | ------- | ------- | -------- |
| Delete the billing account, transfer ownership                                                  | yes     | no      | no       |
| Payment method, subscription, manual top-up, automatic top-up, alerts and organization links    | yes     | yes     | no       |
| Invitations, roles of admins and viewers, removal of admins and viewers, renaming               | yes     | yes     | no       |
| Invoices and invoice links (they carry the payer's address)                                     | yes     | yes     | no       |
| Read the balance, credit grants, ledger, subscription, usage, settings and linked organizations | yes     | yes     | yes      |

- Access errors: a caller that is not a member of the named billing account gets `not_found`, also when the account exists, and a deleted billing account is `not_found` for every RPC. A member whose role does not allow the action gets `permission_denied`.
- A billing account has exactly one `owner`. Nobody can change the role of the `owner` or remove the `owner` (`failed_precondition`), and nobody can grant `owner` with a role change or an invitation. Only `TransferOwnership` makes another existing member the `owner`, and the previous owner becomes `admin`. Every `admin` and `viewer` may remove itself.
- Owners and admins invite by email address with the role `admin` or `viewer`; a second pending invitation for the same address fails with `already_exists`. The server sends the invitation email through Resend with a link to the [web app](apps-delino-accounts-foundation.md) that carries a one-time invitation token (`bai_` followed by 32 random bytes in unpadded base64url). An invitation expires 7 days after it is created. `AcceptInvitation` accepts the token only for a signed-in user whose Logto primary email equals the invited address (ASCII case folding), and creates the membership for that Logto user ID; another user gets `permission_denied`, and the invitation stays pending. An unknown token is `not_found`; an accepted, revoked or expired invitation is `failed_precondition`. The server stores only the SHA-256 hash of the token.
- One billing account can pay for several organizations of several Delino services. A change of members has no effect on existing links.
- `DeleteBillingAccount` succeeds only when the balance is 0 or more and no organization is linked; otherwise it fails with `failed_precondition`. It cancels the Paddle subscription at once, forfeits the remaining credit (credits are not refunded), ends every membership and pending invitation and marks the billing account deleted. Financial records and audit events stay for the retention period (see [Storage](#storage)).
- The [protos contract](protos-delino-accounts-api-contract.md) records the exact request rules, limits and error codes of every RPC.

### Payments and credits

- Payment provider (recorded decision): [Paddle](https://www.paddle.com/), the merchant of record, in the `sandbox` or `production` environment that `PADDLE_ENVIRONMENT` selects. The server calls the Paddle API with a minimal hand-written HTTP client; it uses no Paddle SDK.
- Charges are USD only and tax-exclusive. When a user buys $X of credit, Paddle adds the tax on top, and the balance receives the full $X.
- Conversion: Paddle amounts are whole USD cents. The server converts cents to micro-USD by multiplying by 10,000, only at the payment boundary. The conversion is exact and needs no rounding. Every amount that the server sends to Paddle is a whole number of cents; a requested amount that is not a multiple of 10,000 micro-USD fails with `invalid_argument`.
- Subscription: an active $10/month Paddle subscription (`PADDLE_SUBSCRIPTION_PRICE_ID`) is required for any billable use of a consuming service. `StartSubscription` creates the Paddle transaction with the billing account ID in its custom data, and the web app opens the Paddle.js checkout with it. Each successful subscription payment adds a $10 credit grant. A canceled subscription stays active until the end of the paid period, and `ResumeSubscription` removes a scheduled cancellation before then. The subscription becomes `ACTIVE` only after the verified payment webhook; while it is `PENDING`, `StartSubscription` returns the open checkout transaction again.
- Manual top-up: owners and admins add credit with a one-time charge on the subscription (`POST /subscriptions/{id}/charge` with `effective_from: immediately`, on `PADDLE_TOP_UP_PRODUCT_ID`), only while the subscription is `ACTIVE`. The amount is at least $25 and at most $10,000, in whole cents. The caller's `idempotency_key` (UUID v7) makes a retry within 24 hours return the first result without a second charge.
- Automatic top-up: off by default. An owner or admin turns it on with a threshold of at least $10, an amount of at least $25 and a required monthly cap of at least the amount. Other settings fail with `invalid_argument`.
  - It runs when a usage deduction makes the balance fall below the threshold. At most one automatic top-up is in flight for each billing account.
  - The monthly cap counts only automatic top-ups in a UTC calendar month; the subscription and manual top-ups do not count. A top-up that would exceed the cap does not run.
  - The server charges the amount with `POST /subscriptions/{id}/charge` (`effective_from: immediately`). A charge that Paddle rejects, for example because the subscription is `past_due` or its next billing period starts within 30 minutes, and a failed payment count as failed automatic top-ups.
  - After 2 failed automatic top-ups in a row, automatic top-up turns off, and owners and admins get an email. A successful automatic top-up and enabling the setting reset the count.
- Credit is added only by verified Paddle webhooks, never by the charge call or the checkout.
- Credit grants: each subscription payment, manual top-up and automatic top-up creates a grant that expires 12 months after the purchase. Usage consumes the grant that expires first (FIFO). A new grant first pays off any debt (a negative balance), and only the rest becomes usable credit. Owners and admins get an email 7 days before a grant expires. Expired credit leaves the balance as an `EXPIRY` ledger entry.
- Refunds: credits are non-refundable. Support can refund only manually in the Paddle dashboard. The refund webhook removes the matching credit with a `REFUND` ledger entry, and the balance can go negative.
- Debt: usage is always deducted, also below zero. A negative balance is debt.
- Low-balance alert: when automatic top-up is off, owners and admins get one email each time the balance falls below the alert threshold. Owners and admins set the threshold (at least $0). The default is $10.
- Invoices (owner and admin only): `ListInvoices` lists the completed Paddle transactions without links, and `GetInvoiceUrl` returns a short-lived link to the Paddle invoice PDF of one transaction, from Paddle `GET /transactions/{id}/invoice` on demand.

### Paddle webhook

- `POST /webhooks/paddle` verifies the `Paddle-Signature` header (`ts=<unix time>;h1=<hex HMAC-SHA256 of "<ts>:<raw body>" with PADDLE_WEBHOOK_SECRET>`) in constant time before it parses the payload, and rejects a timestamp more than 5 minutes from the server time. A missing or invalid signature gets `401` and changes nothing.
- The server records each Paddle event ID and ignores a repeated event, so a replayed webhook adds no credit.
- The server applies the event (credit grant, refund, subscription status or failed payment) in the same transaction that records the event ID, and then answers `2xx`.

### Usage reports and billing status

- `UsageService` implements the [usage reports](protos-delino-accounts-api-contract.md#usage-reports), the [billing status](protos-delino-accounts-api-contract.md#billing-status) and the [links](protos-delino-accounts-api-contract.md#links) of the protos contract: idempotent reports with the key (service, organization, meter, window start), attribution through the link history, unattributed usage, FIFO deductions that can make the balance negative, and the status rules `ALLOWED`, `BLOCKED` and `SUSPENDED` (3 days after the block starts).
- `ReportUsage` validates every report of a request before it applies any, and a window may end at most 1 minute after the server time. A repeated key with other values keeps the first values and is logged as a mismatch. A status poll with cursor 0 returns every linked organization, with at most one entry per organization, and an unlink adds no entry ([protos contract](protos-delino-accounts-api-contract.md#billing-status)).
- Concurrent reports for one billing account serialize on the billing account, so the balance, the grants and the automatic top-up trigger stay consistent.
- A status change of a linked organization sends an email to owners and admins when it becomes `BLOCKED` (with the suspension time, 3 days later) and when it becomes `SUSPENDED`.

### Emails

- Delivery: Resend, with the API key `RESEND_API_KEY` and the sender `EMAIL_FROM`, in English, as plain text and simple HTML. A sender abstraction has a fake for tests that records the messages.
- Recipients are the `owner` and `admin` members of the billing account, except the invitation, which goes to the invited address.

| Email                       | Trigger                                                                   |
| --------------------------- | ------------------------------------------------------------------------- |
| Invitation                  | `CreateInvitation`                                                        |
| Payment failed              | A failed automatic top-up or a failed subscription payment                |
| Automatic top-up turned off | The second failed automatic top-up in a row                               |
| Low balance                 | The balance falls below the alert threshold while automatic top-up is off |
| Blocked                     | A linked organization becomes `BLOCKED`; includes the suspension time     |
| Suspended                   | A linked organization becomes `SUSPENDED`                                 |
| Credit expiry               | A credit grant expires in 7 days                                          |

- The server records each notification with a deduplication key, so a retry never sends one notification twice.

### Metrics

`GET /metrics` on `METRICS_ADDR` (`planned`) serves Prometheus metrics on an internal listener. The listener is reachable only on the Droplet's `delino` Docker network: the deployment publishes no host port for it, and Caddy never routes it, so it is never reachable through a public origin. An empty `METRICS_ADDR` turns the listener off. The metrics cover at least usage reports, Paddle webhooks, top-up attempts and failures, emails and the number of linked organizations for each billing status. Metric labels never contain billing account IDs, organization IDs, user IDs, email addresses or Paddle IDs. Record the metric names and labels here in the change that adds them. A scraper, dashboards and alerts are out of scope.

### Identifiers and money

- Identifiers: persisted entities use UUID v7. User-owned records reference the Logto user ID (`sub`).
- Money: all amounts are `int64` micro-USD values with a `_usd_micros` suffix, according to the [billing contract](repository-billing-contract.md). The only provider conversion is the exact cents conversion in [Payments and credits](#payments-and-credits). `accounts-api` does no per-unit pricing: the consuming services compute and round each usage amount and document the rounding in their own contracts.

## Configuration

| Variable    | Required | Default           | Values                           |
| ----------- | -------- | ----------------- | -------------------------------- |
| `ADDR`      | no       | `127.0.0.1:18100` | `host:port` listen address       |
| `LOG_LEVEL` | no       | `info`            | `debug`, `info`, `warn`, `error` |

- The server validates the configuration at startup and exits with a non-zero status when a value is not valid.
- The server removes only ASCII white space (space, tab, line feed, vertical tab, form feed and carriage return) from the start and end of `ADDR` and `LOG_LEVEL`. A value that consists only of ASCII white space counts as unset and selects the default. Unicode white space, such as U+00A0 (no-break space) and U+3000 (ideographic space), is not removed, so a value that contains it is not valid, also when the whole value consists of such characters. The server matches `LOG_LEVEL` against the level names with ASCII letter case folding only (`INFO` and `iNfO` select `info`). Any other character makes the value not valid, so a value that lower-cases to a level name only under Unicode case mapping, such as `İNFO` (U+0130 followed by `NFO`), stops the server at startup like any other invalid value.
- `ADDR` accepts an empty host (all interfaces), an IP address (IPv6 in brackets, with an optional zone) or an RFC 1123 hostname; configuration validation checks only the host syntax, so a hostname that does not resolve still fails when the server starts listening.
- Root `pnpm dev` and `pnpm exec turbo run dev --filter=accounts-api` pass `ADDR` and `LOG_LEVEL` from the shell to the server through Turbo `passThroughEnv` in `servers/accounts-api/turbo.json`. Add every new configuration variable to that list in the change that makes the server read it. Values from the Infisical `/servers/accounts-api` development folder do not need that list, because the injection runs inside the task, and they override shell values.
- Production must set `ADDR` explicitly for its deployment target. The default listens on loopback only.
- Billing configuration (`planned`). Add each variable to the table above, to `.env.example` (name and placeholder only) and to the Turbo `passThroughEnv` list in the change that makes the server read it:

| Variable                       | Required | Default                          | Meaning                                                                                            |
| ------------------------------ | -------- | -------------------------------- | -------------------------------------------------------------------------------------------------- |
| `DATABASE_URL`                 | yes      |                                  | Postgres connection URL. Secret.                                                                   |
| `RUN_MIGRATIONS`               | no       | `true`                           | Apply the embedded goose migrations at startup.                                                    |
| `LOGTO_ISSUER`                 | yes      |                                  | Logto issuer, `<tenant endpoint>/oidc`.                                                            |
| `LOGTO_AUDIENCE`               | no       | `https://accounts.api.delino.io` | Expected `aud`.                                                                                    |
| `LOGTO_MANAGEMENT_ENDPOINT`    | yes      |                                  | Logto Management API endpoint, used to read the primary email of the caller of `AcceptInvitation`. |
| `LOGTO_M2M_CLIENT_ID`          | yes      |                                  | Machine-to-machine application ID for the Logto Management API.                                    |
| `LOGTO_M2M_CLIENT_SECRET`      | yes      |                                  | Its secret. Secret.                                                                                |
| `CORS_ORIGINS`                 | no       | `https://accounts.delino.io`     | Comma-separated browser origins.                                                                   |
| `WEB_ORIGIN`                   | no       | `https://accounts.delino.io`     | Origin of the web app in invitation email links.                                                   |
| `RESEND_API_KEY`               | yes      |                                  | Resend API key. Secret.                                                                            |
| `EMAIL_FROM`                   | yes      |                                  | Sender address of billing emails.                                                                  |
| `PADDLE_ENVIRONMENT`           | yes      |                                  | `sandbox` or `production`.                                                                         |
| `PADDLE_API_KEY`               | yes      |                                  | Paddle API key of that environment. Secret.                                                        |
| `PADDLE_WEBHOOK_SECRET`        | yes      |                                  | Secret of the Paddle notification destination. Secret.                                             |
| `PADDLE_SUBSCRIPTION_PRICE_ID` | yes      |                                  | Paddle price of the $10/month subscription.                                                        |
| `PADDLE_TOP_UP_PRODUCT_ID`     | yes      |                                  | Paddle product of manual and automatic top-up charges.                                             |
| `METRICS_ADDR`                 | no       | empty (listener off)             | `host:port` of the internal metrics listener.                                                      |

- Paddle and Resend use separate sandbox and production credentials. Development uses only the Paddle `sandbox` environment and Resend test credentials, according to the [environment contract](repository-environment-contract.md#billing-integration-credentials).

## Storage

The scaffold has no persistent storage. The database is PlanetScale Postgres, according to the repository defaults. File uploads, such as invoice documents, use Cloudflare R2 with signed URLs according to the repository defaults unless this contract documents another choice. Invoices stay in Paddle: the server stores no invoice documents and fetches the Paddle invoice link on demand.

- Driver (`planned`): `github.com/jackc/pgx/v5`, already in the root Go module.
- Migration tool (recorded decision, `planned`): [goose](https://github.com/pressly/goose) (`github.com/pressly/goose/v3`) with SQL migrations embedded from `internal/db/migrations/`. Migrations are forward-only and are never edited after they reach `main`; a change adds a new file. The server applies pending migrations at startup unless `RUN_MIGRATIONS=false`. The old container keeps serving while the new one starts during a deployment, so every migration must stay compatible with the code of the previous release (expand first, contract in a later release).
- Tables (summary, `planned`): billing accounts (with a deletion time), members, invitations (token hash, email, role, expiry, acceptance and revocation times), organization links with their link history, billing audit events, credit grants, ledger entries, subscriptions, Paddle customer and transaction references, processed Paddle event IDs, automatic top-up and alert settings, notifications with their deduplication keys, usage reports (including unattributed usage) and organization billing statuses with their change sequence.
- The server stores Paddle customer, subscription, transaction and event IDs only. It never stores card data.
- Billing audit events record member, role, invitation, ownership, settings, subscription, top-up and link changes with the actor's Logto user ID.
- Retention (recorded decision): usage records, the ledger, credit grants and billing audit events are kept for 7 years. Deleting a billing account (a soft delete) or unlinking or deleting an organization keeps these records. The first version has no job that deletes records after the retention period.
- Open decision: the backup policy. Record it here before the first deployment of a revision with persisted entities.
- Deployment: the database stage that runs before the servers is `planned` in the [deployment workflow](repository-workflow-contract.md#deployment-workflow). It must exist before the first deployment of an `accounts-api` revision with persisted entities.

## Security

- Accept only Logto access tokens as API credentials. Do not accept ID tokens. The Paddle webhook is the only endpoint that accepts another proof, the Paddle signature, and only for the delivery of a Paddle event (see [Authentication](#authentication)).
- Payment card data stays with the payment provider. The server stores provider references, not raw card numbers.
- The server never logs secrets, tokens, authorization codes, application secrets, API keys, card data, payment details, invitation tokens or email bodies. It does not log email addresses at the `info` level.
- Secrets, such as the database URL, Logto Management API credentials, the Paddle API key and webhook secret and the Resend API key, follow the [environment contract](repository-environment-contract.md): Infisical for development and the Droplet env file for production. They never appear in `.env.example`.

## Graceful Shutdown

- `SIGINT` or `SIGTERM` stops the listener from accepting new connections and starts a drain that waits at most 10 seconds (`shutdownTimeout`) for in-flight requests.
- The signal does not cancel request contexts. Request contexts derive from a server-lifetime context, so handlers that honour `r.Context()` finish their work normally while the server drains.
- The server releases its signal registration as soon as it observes the first signal, before it logs `server_shutdown_started` and starts the drain. A second `SIGINT` or `SIGTERM` during the drain therefore uses the default disposition and terminates the process immediately, with the signal as its exit cause, instead of waiting for the drain to finish. A single signal still drains in-flight requests and exits with status 0 once they finish.
- The server cancels the server-lifetime context only after the drain ends. A handler that outlives the 10-second drain observes a cancelled request context at that point, the shutdown is reported as failed, and the process exits with a non-zero status.

## Logging

- The server writes structured JSON logs to standard output with `log/slog`. These are service logs for log aggregation, not interactive CLI output, so they do not use ANSI color.
- Startup logs the listen address and log level. Shutdown logs the signal-driven start and the result. A listener failure is never discarded, even when it races with the shutdown signal: the process exits with a non-zero status and logs the error.
- Every HTTP request produces one `http_request` log with `method`, `path`, `status`, `duration_ms` and `bytes` fields. `status` is the first committed response status (the one the client receives), `200` for an implicit body write, and informational `1xx` responses other than `101` do not count as committed. A status code outside `100` to `999` is never committed, because `net/http` panics on it before it writes a response, so a handler that writes one is logged like a handler that panics before any status. A request whose handler panics is logged too: `status` is `500` when no response status was committed before the panic and the committed status otherwise. A flush (`http.ResponseController.Flush` or `http.Flusher`) commits `200` when no status was committed, so a handler that flushes and then panics is logged with the status the client received. A successful hijack only transfers the connection: `net/http` sends nothing for it, and the handler may write a raw response with any status or none, which the logger cannot observe. The record of a hijacked request therefore adds a boolean `hijacked` field with the value `true`, `status` is the status committed before the hijack or `200` when none was (a value that is not a client-observed status for that connection, and never `101` unless the handler wrote it before the hijack), a status written after the hijack is ignored, and a later panic is not logged as `500`. Records of requests that do not hijack have no `hijacked` field. The panic is not recovered by the logger, so `net/http` still aborts the connection. `bytes` is the number of response body bytes that `net/http` accepted from the handler. It equals the body the client receives for a response that completes. `net/http` buffers the response and discards the buffered part when the handler panics or the client disconnects, so the count can exceed what the client received; the buffered bytes of a panicking handler are counted although `net/http` discards them. It is `0` for a `HEAD` request even though the handler writes the body it would send for the equivalent `GET`; the status is the same as for that `GET`.
- A server-wide `OPTIONS *` request is logged like every other request, with `path` `*`. `net/http` would otherwise answer it before the configured handler runs, so the server construction sets `DisableGeneralOptionsHandler` and the handler answers it inside the request logger: `200` with `Content-Length: 0` and an empty body. The handler reads at most 4 KiB of request body, as the `net/http` built-in does; a larger body closes the connection after the response. Other requests with the request target `*` are rejected with `400` by the router and are logged too.
- A request whose `Expect` header is not empty and does not contain the `100-continue` token (for example `Expect: a-pony`) is rejected by `net/http` with `417 Expectation Failed` and a closed connection before it calls the handler. It is logged like every other request: one `http_request` record with its `method` and `path` (never routed, so `/missing` stays `/missing`), `status` `417` and `bytes` `0`. The `417` response, its `Connection: close` header and the connection closure are unchanged. The server serves a listener wrapped by `server.ObserveListener`, and `newServer` sets `ConnContext`, `ConnState`, `MaxHeaderBytes` and `DisableGeneralOptionsHandler` for it. `DisableGeneralOptionsHandler` is a requirement of the observer: when it is left `false`, `net/http` answers a server-wide `OPTIONS *` request itself, so that request is not logged, a later `417` on the same connection gets no `http_request` record, and the `duration_ms` of later `http_request_rejected` records on that connection is measured from the `OPTIONS *` request (`MaxHeaderBytes` is `server.MaxHeaderBytes`, the `net/http` default of 1 MiB, and the observer derives its limit from the same constant):
  - The observer tees the inbound bytes of each connection, synchronously and before `net/http` reads them, into an incremental parser that holds at most one request head in memory (the head limit of `net/http`, which is 1 MiB plus 4096 bytes for the first head of a connection; a later head on a kept-alive connection may be up to 4096 bytes larger, because `net/http` does not charge to it the bytes that its keep-alive wait after the previous response already buffered, so the observer allows that much more for every head after the first). It parses each head with `http.ReadRequest`, skips `Content-Length` and chunked bodies (chunk-size lines and trailers are limited to 4096 bytes each) and queues at most 1024 parsed requests that `net/http` has read but not yet dispatched. It stores only the method, the path and whether the `Expect` header is unsupported, never header values or bodies, and it starts no goroutine, so it releases everything when `net/http` closes the connection.
  - The request logger tells the observer when a handler call starts and ends, which dequeues the oldest request. The observer logs the oldest queued request when `net/http` writes a `417` response with `Connection: close` while no handler call or response of a handler is in progress and that request has an unsupported `Expect` header. The record is written once, before the response is written, as the request logger does. A request that `net/http` never answers with `417` (a client that disconnects, an earlier request that closes the connection, a shutdown, a request `net/http` rejects as malformed) produces no `http_request` record from the observer; a request that `net/http` rejects before it exists as a request produces an `http_request_rejected` record (see below).
  - If the bytes do not parse as `net/http` parses them (a malformed or oversized head, an invalid chunked body, a full queue), the observer stops parsing for that connection. A later `417` on that connection may then be missing from the logs, but a request is never logged as `417`-rejected unless `net/http` sent the `417`. The detection of `http_request_rejected` records below does not depend on the parser, so it keeps working after the parser stopped, with one exception: a `header_read_timeout` record needs the parser to be waiting for a request head, so it is not written for a connection whose parser stopped while `net/http` kept the connection open.
  - `net/http` discards up to 4 leading CR or LF bytes before a request line when the previous request on the connection was a `POST`, so the parser tracks the method of the last head that parsed and skips the same bytes in that case, wherever the reads split them. A `417` after a `POST` and a leading empty line is logged as for any other request. After another method, at the start of the connection, or with a fifth leading CR or LF byte, `net/http` answers `400` (an `http_request_rejected` record) and the parser stops.
- The one exception to the one-`http_request`-record-per-request rule is a request that `net/http` rejects before a request exists, such as a malformed request line. It has no reliable method and path, so it is not an `http_request` record. The observer logs it as one `http_request_rejected` record at `INFO` level, the level of `http_request`, so `LOG_LEVEL=warn` and `LOG_LEVEL=error` hide it like every `http_request` record. The record has these fields and no others besides the standard `time`, `level` and `msg`:
  - `reason`: one of the values below (the Go type is `server.RejectionReason`).
  - `status`: the status that `net/http` sent to the client, or `0` when it sent no response (`header_read_timeout`).
  - `duration_ms`: the time from the first byte of the rejected request (or from the connection being accepted when no byte arrived) to the rejection, in milliseconds. The request waits behind earlier pipelined requests on the connection, so it starts when the last handler finished if that was later.
  - There is no `method`, `path` or `bytes` field: none of them is known, or can be trusted, before the request parses, and the record never contains header values, the request line or the body.

  | `reason`                        | Rejection by `net/http`                                                                                                                 | `status` |
  | ------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- | -------- |
  | `malformed_request`             | A malformed request line or header, a missing or malformed `Host` header, or a malformed partial line when `ReadHeaderTimeout` expires. | `400`    |
  | `request_header_too_large`      | A request head over `MaxHeaderBytes`.                                                                                                   | `431`    |
  | `unsupported_transfer_encoding` | A `Transfer-Encoding` other than `chunked`.                                                                                             | `501`    |
  | `unsupported_protocol_version`  | An HTTP version that `net/http` does not serve over HTTP/1.x, such as `HTTP/2.0` in the request line.                                   | `505`    |
  | `header_read_timeout`           | The client did not send a complete request head within `ReadHeaderTimeout`, and `net/http` closed the connection without a response.    | `0`      |
  | `rejected`                      | Any other status that `net/http` answers directly (not produced by a known request today).                                              | observed |
  - `net/http` answers the `400`, `431`, `501` and `505` cases itself, with one write to the raw connection of a response that has a fixed shape: `HTTP/1.1 <status> <status text>` (optionally `: <detail>`), exactly the headers `Content-Type: text/plain; charset=utf-8` and `Connection: close`, and a text body, with no `Date` and no `Content-Length`. The observer recognizes that write on the outbound bytes once the response of the last handler call is complete and logs it once, before the response is written, like the request logger. A response written by a handler always carries `Date`, so it never has this shape, and bytes of a handler's own response body are never taken for it even when they look like it: `net/http` flushes the buffered end of a handler's response after the handler returned, so the observer treats the response as in progress until `net/http` reports the connection idle through `ConnState`; a `400` written by the handler (the router rejecting the request target `*`) stays an `http_request` record. The response and the connection closure are unchanged. `net/http` closes the connection after each of these responses, so a connection logs at most one `http_request_rejected` record.
  - When `ReadHeaderTimeout` expires, `net/http` either closes the connection without any response or answers `400` and closes it, depending on the bytes after the last line break of the head it has received. It parses that partial line as if it were complete: an empty tail (a line boundary), a valid request line or a valid header line closes the connection silently, and any other tail (for example `GET /hea`, `GE`, `GET / HTTP/1.1\r`, a cut header name such as `User-Ag`, or a lone `\r` after a header) gets the `400`. During a graceful shutdown `net/http` never sends the `400`. The observer does not copy that validation. It sees the timed-out read, which `net/http` makes with a deadline set by itself, and handles it when the parser was waiting for a request head and either no request head ever parsed on the connection, or at least four bytes of the next request had arrived after an earlier request (the leading CR and LF bytes that the parser skipped after a `POST` count as received). `net/http` waits for the first four bytes of a request after a response under `IdleTimeout` and arms `ReadHeaderTimeout` only once it has them, so a stall with fewer than four bytes of the next request is the keep-alive wait and is not logged, while on a connection that has not parsed a head any partly received head or empty wait counts. With nothing after the last line break (including the skipped leading bytes and a connection that sent nothing) it logs `header_read_timeout` with `status` `0` at once. With a partial line buffered it waits for `net/http`: the `400` is logged as `malformed_request` with `status` `400` like any other `400` of this kind, and when the connection closes without a response (reported through `ConnState` as closed) it logs `header_read_timeout` with `status` `0`, the start and fields unchanged, a moment after the read. Either way it is one record, whose `duration_ms` is measured from the first byte of the rejected request. A client that connects and sends nothing for `ReadHeaderTimeout` is therefore logged too (`status` `0`, `duration_ms` about the timeout), while one that connects and closes sooner is not. These are not logged: the timeout of the keep-alive wait between requests (`IdleTimeout`), which is a normal close, including a stall after one to three bytes of the next request; the timeout of a request body (`ReadTimeout`); a read that `net/http` interrupts itself with a deadline in the past after a request; and a connection that a handler hijacked. The server sets `ConnState` to `server.ConnState` so the observer learns about a hijack and never reports a hijacked connection's own reads, writes and deadlines, and about a closed connection with a pending header timeout.
  - A request that `net/http` rejects is never logged twice: a request that reaches the handler is logged by the request logger only, a `417` rejection stays an `http_request` record, and the `http_request_rejected` record is written at most once per connection.

- `net/http` internal errors, such as the report of a recovered handler panic with its stack trace and the report of a superfluous `WriteHeader` call, are logged as `ERROR` JSON records on standard output through `http.Server.ErrorLog`. They are never written as plain text to standard error, and they are not downgraded to `INFO` or dropped when `LOG_LEVEL` is `warn` or `error`. `http.ErrAbortHandler` panics stay unlogged, as `net/http` defines. The server is constructed in one place (`newServer` in `cmd/accounts-api`) so that this setting applies to every production server.
- Authentication logs (`planned`) include the Logto user ID, the API resource and the rejection reason, according to the authentication contract.
- Billing events (`planned`): structured `billing_*` records for each usage report, status change, top-up attempt and result, webhook, block, suspension, resume, email and link change. They carry the relevant fields among `billing_account_id`, `service`, `organization_id`, `meter`, `window_start`, `paddle_transaction_id` and `paddle_event_id`. An email record carries the email kind and the billing account ID, never the address or the body.

## Build and Test

Run from the repository root:

- `pnpm --filter accounts-api dev`: run the server through the development launcher `scripts/dev.mjs` (see [Development Launcher](#development-launcher)). The `dev` script starts the launcher through the shared `scripts/dev/with-infisical.mjs`, which injects the Infisical `dev` variables of the `/servers/accounts-api` folder when the checkout has a local Infisical project configuration, according to the [environment contract](repository-environment-contract.md#local-development-injection).
- `pnpm exec turbo run build --filter=accounts-api`: build the binary into `servers/accounts-api/dist/`.
- `pnpm exec turbo run lint --filter=accounts-api`: `go vet ./...`.
- `pnpm exec turbo run test --filter=accounts-api`: `go test ./...` followed by the launcher tests, `node --test "scripts/*.test.mjs"`.

The `build` script passes `-buildvcs=false`, so the binary carries no VCS stamping (`vcs.revision`, `vcs.time` and `vcs.modified` build settings). This is a workaround with the following boundaries:

- Reason: Go's default `-buildvcs=auto` discovery recognizes only a `.git` directory. In a linked Git worktree, where `.git` is a file, it walks up to an ancestor `.git` directory. When that ancestor is not a valid repository, `go build` fails with `error obtaining VCS status`; when it is a valid but unrelated repository, the binary is stamped with the wrong revision.
- Scope: the `accounts-api` `build` script and the `go build` step of the development launcher. The `lint` and `test` scripts do not stamp binaries and are unchanged.
- Removal condition: remove the flag when the Go toolchain recognizes the linked-worktree `.git` file, or when release builds move to a normal-clone job that needs VCS stamping.

Go tasks are uncached in Turbo because the Go toolchain and module cache are not declared task inputs. Remove `servers/accounts-api/dist/` after validation.

Billing tests (`planned`):

- Postgres-backed tests read `ACCOUNTS_TEST_DATABASE_URL`, a URL whose user may create databases, like the `OWNSAND_TEST_DATABASE_URL` fixture of `ownsand-api`; without it they are skipped.
- Unit tests: FIFO consumption with debt payoff, credit expiry, the monthly cap, the 2-failure shutoff, the cents conversion and the status rules with a fake clock.
- Integration tests with Postgres, a fake Paddle and the email fake: each RPC and role, invitation acceptance (valid, reused, and for another email address), idempotent and concurrent usage reports, attribution through the link history, the poll cursor, Paddle signature rejection and duplicate event IDs, a refund that makes the balance negative, and the machine-to-machine scope checks.
- Manual acceptance on the Paddle sandbox: checkout, the $10 subscription, manual top-up, automatic top-up, failing test cards and refunds. Record it separately from fixture and build checks.

CI runs the repository Go checks in the root Go module when Go sources or Go module files change: `gofmt` check, `go vet ./...` and `go test ./...`. See the [workflow contract](repository-workflow-contract.md). The `workspace-servers` job also runs the package `build`, `lint` and `test` scripts, which include the `scripts/*.test.mjs` tests of the development launcher. CI does not build container images or deploy the server.

### Development Launcher

`scripts/dev.mjs` replaces `go run ./cmd/accounts-api`. `go run` forwards only `SIGINT` and `SIGQUIT` to the compiled server and does not tie the server's lifetime to its own, so a `SIGTERM` sent to the `go run` process (`kill`, `timeout`, a supervisor, pnpm or Turbo) left the server listening without a graceful shutdown.

- The launcher builds the server with `go build -buildvcs=false -o <temporary directory>/accounts-api ./cmd/accounts-api`, then runs the binary as its direct child. The temporary directory is created under the operating system temporary directory and removed after the last child has exited, on every exit path that lets the launcher finish. A `SIGKILL` of the launcher is not such a path: it leaves the temporary directory and, as described below, the server behind.
- Child processes use explicit argument arrays and `shell: false`. The working directory is `servers/accounts-api/`; standard streams and the environment, including `ADDR` and `LOG_LEVEL`, reach the server unchanged. The server configuration, its logs and `/healthz` are unchanged, and the production binary is built by the `build` script, not by the launcher.
- The launcher forwards `SIGINT`, `SIGTERM` and `SIGQUIT` to the active child (the build or the server) and writes an `accounts_api_dev_signal` JSON event to standard error. It does not handle `SIGHUP`. It waits for the child to exit, so after `SIGINT` or `SIGTERM` the server drains and logs `server_shutdown_started` and `server_shutdown_completed` before the launcher completes. `SIGQUIT` is the Go runtime's quit signal: the server prints one goroutine dump (`SIGQUIT: quit`) and exits at once without a graceful drain, so no `server_shutdown_completed` event is logged, and the launcher completes after the server has exited.
- On POSIX platforms the launcher starts each child (the build and the server) in its own process group (`detached: true`) and forwards each signal it receives once. A terminal Ctrl-C (or Ctrl-\) signals the whole foreground process group, so only the launcher receives it and the server receives exactly one `SIGINT` (or `SIGQUIT`). Without this, the server would receive the group signal directly and again from the forward; the server releases its signal registration on the first signal, so the duplicate would terminate it during the drain. A second Ctrl-C during the drain still reaches the launcher, is forwarded and terminates the server immediately, as described in Graceful Shutdown. On Windows `detached` is not used because it opens a separate console window instead of creating a process group; the Windows behavior is unchanged and not validated.
- Orphan outcome: the server is no longer in the launcher's process group, so a `SIGKILL` of the launcher, or a `SIGHUP` to its process group (closing the terminal), stops the launcher only and leaves the server running with its listening port. Stop such a server by its process ID. `SIGHUP` forwarding is not part of the launcher. `SIGQUIT` is not an orphan path: the launcher forwards it, waits for the server to exit and removes the temporary directory.
- A signal received before the build starts (while the temporary directory is being created) stops the launcher without starting the build. A signal received during the build stops the launcher after the build exits, even when the build succeeded. The server never starts after a termination request.
- Exit status: the status of the failed build, the server's own status (non-zero for an invalid configuration or an occupied port), or `128` plus the signal number after a forwarded signal (`130` for `SIGINT`, `131` for `SIGQUIT`, `143` for `SIGTERM`).
- The launcher tests build and run the real server with a free loopback port. They cover a `SIGTERM` to the launcher alone, a process-group `SIGINT` with the real server, a process-group `SIGINT` delivered exactly once to a signal-counting server (the drain completes and the launcher exits `130`), a second process-group `SIGINT` during the drain that still ends the server immediately, a process-group `SIGQUIT` with the real server (one forwarded signal event, one goroutine dump, launcher exit `131`, no `server_shutdown_completed`, the port freed and the temporary directory empty), an occupied port, an invalid `LOG_LEVEL`, a build failure, a `SIGTERM` before the build starts (a preload module delays the temporary directory creation so the window is observable; the build and the server are never started) a `SIGTERM` during the build and a process-group `SIGQUIT` during the build (the server is never started and the launcher exits `131`). They use POSIX signals and are skipped on Windows. Because the server runs in its own process group, the test cleanup kills the launcher's children by process ID before it kills the launcher's process group, and it keeps the process IDs it recorded while the launcher was alive so a server orphaned by a dead launcher is still killed. Two tests cover this cleanup for a live launcher and for a dead launcher, so a failed or timed-out launcher test leaves no server process or listening port behind.
- The `go build` step carries the same `-buildvcs=false` workaround and removal condition as the `build` script above.

## Dependencies and Integrations

- Upstream: Logto shared tenant and its Management API (`planned`), Paddle (`planned`, payment provider and merchant of record), Resend (`planned`, email delivery), PlanetScale Postgres (`planned`).
- Downstream: [`delino-accounts-web`](apps-delino-accounts-foundation.md) and the consuming services in [Users and Operators](#users-and-operators).
- Protocol schemas: [protos contract](protos-delino-accounts-api-contract.md) for `protos/delino/accounts/v1/` (`planned`).
- New Go dependencies (`planned`): `github.com/pressly/goose/v3` (migrations), `github.com/resend/resend-go/v2` (email delivery) and `github.com/prometheus/client_golang` (internal metrics). Each needs owner approval and a [dependency security](repository-dependency-security-contract.md) review (for example `govulncheck ./servers/accounts-api/...`) in the change that adds it; record the exact versions here. The server also uses `github.com/jackc/pgx/v5`, `github.com/go-jose/go-jose/v4`, `connectrpc.com/connect` and `google.golang.org/protobuf`, which are already in the root Go module. The Paddle client uses only the Go standard library.
- Rollout: deploy `accounts-api` with the billing APIs before `ownsand-api` starts to report usage and enforce billing. There is no feature flag. Add a changeset for each deployed package.

## Change Triggers

- Update [project-delino-accounts.md](project-delino-accounts.md) when the origin, component IDs, deployment target or open decisions change.
- Update [apps-delino-accounts-foundation.md](apps-delino-accounts-foundation.md) when an API capability or allowed browser origin changes.
- Update the [protos contract](protos-delino-accounts-api-contract.md) before or with every RPC, message or enum change.
- Update the contract of each consuming service, such as [servers-ownsand-api-foundation.md](servers-ownsand-api-foundation.md#billing), when the usage-report, link or status behavior changes.
- Update [repository-billing-contract.md](repository-billing-contract.md) when the payment provider, the credit model or the shared billing account model changes.
- Update [repository-workflow-contract.md](repository-workflow-contract.md) when Go validation or deployment workflows change.
- Update `servers/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-delino-accounts.md](project-delino-accounts.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [repository-billing-contract.md](repository-billing-contract.md)
- [repository-environment-contract.md](repository-environment-contract.md)
- [repository-license-contract.md](repository-license-contract.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [repository-dependency-security-contract.md](repository-dependency-security-contract.md)
- [protos-delino-accounts-api-contract.md](protos-delino-accounts-api-contract.md)
- [Connect for Go](https://connectrpc.com/docs/go/getting-started)
- [Paddle: one-time charges on a subscription](https://developer.paddle.com/build/subscriptions/bill-add-one-time-charge/)
- [Paddle pricing](https://www.paddle.com/pricing)
