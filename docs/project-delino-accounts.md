# Project: delino-accounts

## Goal

Provide one place where Delino users manage their account information and their billing information. Account information includes the profile and preferences that are attached to the shared Logto identity. Billing information includes payment methods, billing contacts, balances, credits and invoices.

## Project ID

`delino-accounts`

## Domain Ownership Map

- `apps/delino-accounts/`: Rsbuild React web app served at `https://accounts.delino.io`.
- `servers/accounts-api/`: Go API server served at `https://accounts.api.delino.io`.
- `protos/delino/accounts/`: Connect RPC schemas for the account and billing APIs, in the protobuf package `delino.accounts.v1`.

The project has two deployables. The web app is a static single-page application with no server runtime. The API server owns all account and billing state. The project has no `cmds`, `crates` or `packages` paths. The schemas (and their generated bindings) are not a deployable.

## Domain Contract Documents

- [apps-delino-accounts-foundation.md](apps-delino-accounts-foundation.md)
- [servers-accounts-api-foundation.md](servers-accounts-api-foundation.md)
- [protos-delino-accounts-api-contract.md](protos-delino-accounts-api-contract.md)

## Cross-Domain Invariants

- Component IDs:

| Component ID          | Path                    | Production origin                |
| --------------------- | ----------------------- | -------------------------------- |
| `delino-accounts-web` | `apps/delino-accounts/` | `https://accounts.delino.io`     |
| `accounts-api`        | `servers/accounts-api/` | `https://accounts.api.delino.io` |

- The web app calls the API server only through its absolute origin. The web app reads the origin from `PUBLIC_API_ORIGIN` and uses `https://accounts.api.delino.io` when the value is not set. The web app must not import source from `servers/`.
- Business flows between the web app, the consuming services and the API server use Connect RPC. The schemas live in `protos/delino/accounts/v1/`, and the [protos contract](protos-delino-accounts-api-contract.md) owns them. The web app will use `@connectrpc/connect-query` with React Query for server state. Plain HTTP endpoints are limited to operational endpoints such as health checks, with one documented exception: the Paddle webhook `POST /webhooks/paddle` of the API server, because Paddle does not call Connect RPC. The endpoint verifies the Paddle signature before it reads the payload and deduplicates events by Paddle event ID; its signature authenticates only the delivery of an event from Paddle and gives no access to any RPC ([API server contract](servers-accounts-api-foundation.md#paddle-webhook)). The [authentication contract](repository-auth-contract.md#documented-exceptions) lists this webhook signature as an exception to the rule that backend services accept only Logto access tokens; it adds no identity provider, password store or sign-in flow.
- Authentication follows the [repository authentication contract](repository-auth-contract.md). The web app is a Logto single-page application. The API server is a Logto API resource with the indicator `https://accounts.api.delino.io` and validates Logto access tokens. User RPCs need the scope `accounts:billing`. Services call the usage-report API with a Logto machine-to-machine access token for the same API resource and the scope `accounts:report-usage`; this is a Logto access token, so it needs no authentication contract exception. The Logto user ID (`sub`) is the owner key of all account and billing records.
- Billing amounts follow the [repository billing contract](repository-billing-contract.md): USD only, signed 64-bit integers in micro-USD with a `_usd_micros` suffix.
- New persisted entities use UUID v7 identifiers.
- Billing model (recorded decisions; billing accounts, members, roles, email invitations, prepaid credits, the Paddle subscription, top-ups, the Paddle webhook, the billing emails, usage reports, billing statuses and organization links are implemented in the API server): [API server contract](servers-accounts-api-foundation.md) and [protos contract](protos-delino-accounts-api-contract.md) own the details.
  - A billing account is a separate entity with the roles `owner`, `admin` and `viewer`. Any Logto user can create one and becomes its `owner`. Owners and admins invite members by email through Resend.
  - One billing account can pay for several organizations of several Delino services. Each organization of a service links to at most one billing account.
  - Billing accounts hold prepaid USD credits. Paddle is the payment provider and merchant of record. Charges are tax-exclusive, and Paddle cents convert to micro-USD by multiplying by 10,000 (exact). A required $10/month subscription adds a $10 credit grant with each payment; manual top-ups ($25 to $10,000) and optional automatic top-ups add more. Credit is added only by verified, deduplicated Paddle webhooks.
  - Credit grants expire 12 months after purchase and are consumed first-expiring first (FIFO). A new grant pays off debt first. Usage is always deducted, also below zero.
  - Services report usage with their own computed amounts and the idempotency key (service, organization, meter, window start), and read the `ALLOWED`, `BLOCKED` or `SUSPENDED` status of each linked organization.
  - Usage records, the ledger, credit grants and billing audit events are kept for 7 years.
- Consuming services: [`ownsand`](project-ownsand.md#billing) reports usage and enforces billing statuses; the API server side is implemented, and the `ownsand-api` client is `planned` in its own contract.
- `delino-accounts` is the shared account management and billing system for every Delino service, according to the [authentication](repository-auth-contract.md#account-management) and [billing](repository-billing-contract.md#billing-ownership) contracts. Other services link users to `https://accounts.delino.io` and call `https://accounts.api.delino.io`. They do not keep their own account management, payment-provider integration or balances.
- Deviation from repository defaults: the web app uses Rsbuild instead of Rspress because it is an authenticated application, not a documentation or marketing site. It still deploys to Cloudflare Pages as static output.
- The API server deploys to the shared DigitalOcean Droplet and its database is PlanetScale Postgres, according to the repository defaults. The [API server contract](servers-accounts-api-foundation.md) records the details.
- Recorded decisions: the payment provider is Paddle, the database migration tool is goose and email delivery uses Resend ([API server contract](servers-accounts-api-foundation.md)).
- Open decisions: the backup policy. Record it in the API server contract before the first deployment of a revision with persisted entities.
- Rollout: no feature flag. Deploy `accounts-api` with the billing APIs before the consuming services start to report usage. Migrations are forward-only.

## Change Policy

- Update [apps-delino-accounts-foundation.md](apps-delino-accounts-foundation.md), [servers-accounts-api-foundation.md](servers-accounts-api-foundation.md) and this index together when origins, component IDs or the web-to-API boundary change.
- Update the [protos contract](protos-delino-accounts-api-contract.md) and the protos path above when the schema package or its path changes.
- Update [repository-auth-contract.md](repository-auth-contract.md) when the Paddle webhook signature exception changes.
- Update the [documentation catalog](README.md) when contracts are added, renamed or removed.
- Update `apps/AGENTS.md` and `servers/AGENTS.md` only when development procedures, directory ownership or development rules change; follow the [instruction-update policy](README.md#instruction-update-policy).
- Record implementation status, validation results and unresolved limits in pull requests, Linear issues and CI logs/artifacts.

## References

- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [repository-billing-contract.md](repository-billing-contract.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [repository-environment-contract.md](repository-environment-contract.md)
- [repository-dependency-security-contract.md](repository-dependency-security-contract.md)
- [repository-license-contract.md](repository-license-contract.md)
- [project-template.md](project-template.md)
