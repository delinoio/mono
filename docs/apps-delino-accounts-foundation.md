# apps-delino-accounts foundation contract

## Scope

This contract owns the `delino-accounts-web` component of [`delino-accounts`](project-delino-accounts.md) at `apps/delino-accounts/`:

- `apps/delino-accounts/rsbuild.config.ts`: build and development server configuration.
- `apps/delino-accounts/index.html`: HTML document template that sets `<html lang="en">`.
- `apps/delino-accounts/src/`: application source, entry point and unit tests.
- `apps/delino-accounts/public/`: static files copied to the build root, including the Cloudflare Pages `_headers` file. The directory must not contain a top-level `404.html` or a `_redirects` file with a rule that rewrites to `/index.html`; `src/static-hosting.test.ts` enforces this.
- `apps/delino-accounts/turbo.json`: package-local Turbo task configuration.
- `apps/delino-accounts/e2e.config.ts` and `apps/delino-accounts/e2e/`: end-to-end test config and tests, according to the [e2e contract](repository-e2e-contract.md).

## Runtime and Language

- Toolchain: Rsbuild `@rsbuild/core` 2.x with `@rsbuild/plugin-react`, React 19.
- Language: TypeScript.
- Test runner: Rstest `@rstest/core`.
- Runtime: Node.js from root `.nvmrc` for builds and tests. Production output is a static single-page application with no server runtime.
- Deployment target: Cloudflare Pages, production domain `accounts.delino.io`.
- Deviation from repository defaults: the app uses Rsbuild instead of Rspress because it is an authenticated application. See the [project index](project-delino-accounts.md).

## Users and Operators

- Signed-in Delino users who manage their account and billing information.
- Maintainers who develop and deploy the web app.

## Interfaces and Contracts

- Production origin: `https://accounts.delino.io`. The asset prefix is `/`.
- Content language: English (`lang="en"`).

| Route ID             | Path                          | Status    | Purpose                                                                                                                                                             |
| -------------------- | ----------------------------- | --------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `home`               | `/`                           | available | Entry page that introduces the account and billing sections.                                                                                                        |
| `account`            | `/account`                    | planned   | Profile and preferences attached to the Logto user.                                                                                                                 |
| `billing`            | `/billing`                    | available | The caller's billing accounts and billing account creation.                                                                                                         |
| `billing-account`    | `/billing/:billingAccountId`  | available | One billing account: balance, credit grants with their expiry, ledger, subscription, top-ups, settings, members and invitations, linked organizations and invoices. |
| `billing-invitation` | `/billing/invitations/accept` | available | Accept a billing account invitation from the email link as the signed-in user.                                                                                      |
| `callback`           | `/callback`                   | available | Logto sign-in redirect target. It completes the sign-in and opens the pending invitation, when one waits, or `billing`.                                             |

- Billing pages, with `BillingAccountService`, `CreditService` and `BillingUsageService` of the [protos contract](protos-delino-accounts-api-contract.md):
  - Layout: `billing` lists the caller's billing accounts with the caller's role and creates one in a dialog. `billing-account` shows the name and the caller's role and groups the sections in tabs: Overview (balance, subscription and credit grants), Usage (usage summary and ledger), Members (members and invitations), Organizations, Invoices (only for `owner` and `admin`) and Settings (name, automatic top-up, low-balance alert and, for the `owner`, deletion).
  - Complete lists: the lists of billing accounts (`ListBillingAccounts`), members (`ListMembers`), invitations (`ListInvitations`) and linked organizations (`ListLinkedOrganizations`) request `page_size` 200 and follow `next_page_token` as `page_token`, with the same request scope, until the server returns an empty token, so every item shows in server order; an exact page with an empty token makes no further request. The list shows the loaded pages while later pages load. When a later page fails, the loaded rows stay, a notice offers a retry that resumes at the failed page without reading the first page again, and the app makes no further request until the user retries.
  - Create and delete billing accounts. Deletion is offered only to the `owner` and explains that it needs a balance of 0 or more and no linked organizations.
  - Members: invite by email with the role `admin` or `viewer`, change roles between `admin` and `viewer`, remove admins and viewers (every `admin` and `viewer` can also leave), revoke invitations and transfer ownership (`owner` only). The `owner` row has no role change or removal action. After a viewer or admin confirms leaving, the app opens `/billing` first and only then refreshes the cached billing account queries, so the left account never shows its not-found notice and is not listed; a failed leave stays in the dialog with its error and does not navigate. The invitation link is `<web origin>/billing/invitations/accept#token=<token>`. The token is in the URL fragment, so it does not reach Cloudflare Pages logs; the app removes the fragment from the address bar after it reads it and keeps the token only as long as the sign-in redirect needs it. Opening a new link, on the first load of the page and while the page is shown, also deletes any token that an earlier link left in `sessionStorage`, so a later load without a fragment cannot accept the earlier invitation; an address without a token leaves the stored token alone, because the sign-in return reads it.
  - Subscription: start the $10/month subscription with the Paddle.js checkout for the `paddle_transaction_id` that `StartSubscription` returns, cancel it at the end of the paid period, and resume a scheduled cancellation before the period ends (`ResumeSubscription`). A subscription stays `PENDING` until the verified payment webhook arrives, so the page shows the pending state after the checkout closes.
  - Balance and credit grants with their expiry; the ledger with usage by service, organization and meter; the usage summary of a selected UTC calendar month (`ListUsageSummary` with `year` and `month`) with its total.
  - Manual top-up of at least $25 and at most $10,000, in whole cents. The top-up dialog generates one UUID v7 `idempotency_key` when it opens and reuses it for every retry of that top-up. The credit appears after the payment webhook, not with the response. Automatic top-up settings: on or off, threshold (at least $10), amount (at least $25 and at most $10,000) and the required monthly cap. The low-balance alert threshold (at least $0, default $10). The app checks these minimums before it sends a request; the API server enforces them.
  - Linked organizations, read-only. Linking happens in the consuming service, for example in the [OwnSand console](apps-ownsand-console-foundation.md).
  - Invoices, for `owner` and `admin` only: the list from `ListInvoices` and a short-lived link to each Paddle invoice PDF from `GetInvoiceUrl`, fetched when the user opens it. The app never stores or logs the link.
  - Role-based views: the app hides or disables the actions that the caller's role (`BillingAccount.caller_role`) does not allow, according to the [role table](servers-accounts-api-foundation.md#billing-accounts-and-members). A `viewer` sees read-only pages. The API server still enforces every permission.
- Display: English only. Amounts show in USD with 2 decimals. Usage costs below $0.01 show with up to 6 decimals. Rounding is for display only.
- UX follows the repository frontend rules: the Toss Design Guidelines, focus on the single critical input when a form with one critical input opens (for example the billing account name, the invitation email or the top-up amount), and dialogs that close with `Esc`.

- Client-side routes load on direct navigation through the Cloudflare Pages single-page-application fallback: when a project has no top-level `404.html`, Pages assumes a single-page application and serves the root (`/`, the `index.html` document) for any path that does not correspond to a static asset. The fallback applies only while the build output has no top-level `404.html`, so the app must not emit a `404.html`. The app must not add a `_redirects` rule that rewrites to `/index.html` (for example `/* /index.html 200`): Cloudflare rejects it as an infinite loop and ignores it, so it provides no routing. Unknown paths return status `200`, so the app renders its own not-found view once routes exist.
- `public/_headers` sets Cloudflare Pages response headers: `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin` and `X-Frame-Options: DENY`.
- API origin: `src/config.ts` resolves the API server origin from the build-time variable `PUBLIC_API_ORIGIN`. An unset value, an empty value and a value made only of tab, line feed, vertical tab, form feed, carriage return and space characters select `https://accounts.api.delino.io`. Unicode white space such as U+00A0, U+FEFF and U+3000 is not ignored and fails the build as an invalid URL, also when the value consists only of such characters. A value made only of other ignored characters, for example U+0001, is not empty and also fails as an invalid URL. The value must be an `http` or `https` origin with no credentials (user name or password), path, query or fragment, including an empty `?` or `#`; the build fails for other values. As the URL parser does, the check ignores leading and trailing C0 control characters (U+0000 to U+001F) and spaces, and tab, carriage return and line feed anywhere in the value, so a value such as `\u0001https://accounts.api.delino.io` resolves to the same origin that the parser reads. Error messages never echo the raw value because it may carry a credential. `readAccountsApiOrigin` is the only reader of the variable: `rsbuild.config.ts` calls it with `process.env` to fail the build and the dev server early, and `src/api-origin.ts` calls it with `import.meta.env` to export the validated `ACCOUNTS_API_ORIGIN` constant, which Rsbuild inlines into the client bundle. `src/index.tsx` sets the `data-api-origin` attribute of `<html>` to that constant so the build-time value is observable in the running app; it adds no visible markup, request or route.
- API calls: Connect RPC through `@connectrpc/connect-query` and React Query. The app must not call the API through Tauri-specific or ad-hoc bindings.
- Authentication: the app is registered as a Logto single-page application (`@logto/react`) in the shared tenant and requests access tokens for the API resource `https://accounts.api.delino.io` with the scope `accounts:billing`. The build-time variables `PUBLIC_LOGTO_ENDPOINT` (the tenant origin; an http or https origin without credentials, path, query or fragment; a single trailing slash is accepted, while a dot segment such as `/tenant/..` or `/%2e`, a backslash or any other path is rejected even when the URL parser collapses it to `/`) and `PUBLIC_LOGTO_APP_ID` configure it; they are public configuration, set together or not at all. Without them, the billing routes show that sign-in is not configured, and `home` still works. The sign-in redirect URI is `<web origin>/callback`, with the `https://accounts.delino.io` origin in production and the loopback or portless development origin locally; the sign-out redirect URI is the web origin. The Connect transport adds the access token only to requests for the configured API origin.
- Monetary values from the API are `bigint` micro-USD amounts. The app formats them for display only and never converts them to `number` for arithmetic. Amounts that the user enters are parsed from decimal text into `bigint` micro-USD without floating-point arithmetic, and the top-up amounts must be whole cents.
- Paddle checkout: the app loads Paddle.js through `@paddle/paddle-js` on the first checkout and opens the overlay checkout for the `paddle_transaction_id` that `StartSubscription` returned. When the checkout closes or completes, the page reads the subscription again and shows the `PENDING` state until the webhook confirms the payment. The build-time variables `PUBLIC_PADDLE_CLIENT_TOKEN` (the Paddle client-side token, which is public) and `PUBLIC_PADDLE_ENVIRONMENT` (`sandbox` or `production`) configure it; `turbo.json`, `.env.example` and the build validation in `src/config.ts` list them, and the Infisical `/apps/delino-accounts` development folder holds the development values. A token requires the environment, and the token must match it: a sandbox token starts with `test_` and a production token with `live_`; the build fails otherwise without echoing the token. Without a token, the subscription section shows that checkout is not configured. Development uses the Paddle `sandbox` environment.

## Storage

The app has no server-side storage. All account and billing state belongs to the [API server](servers-accounts-api-foundation.md). Browser storage is limited to the Logto SDK session data, per-user display preferences and, only during the sign-in redirect of the invitation flow, the billing account invitation token in `sessionStorage`. The app deletes the token when `AcceptInvitation` returns, also after a failure, and when the user leaves the invitation flow. Static assets of at least 512 KiB require Git LFS according to the repository workflow contract.

## Security

- Do not embed secrets in the build. Every `PUBLIC_` variable is public and is inlined into the client bundle.
- Logto application IDs and the tenant endpoint are configuration, not secrets. The app must not use a Logto application secret.
- Billing input that contains payment card data must go directly to the payment provider's hosted fields. The app and the API server must not receive raw card numbers. Payment details are entered only in the Paddle checkout.
- The Paddle client-side token is public configuration, not a secret. The app never holds a Paddle API key or webhook secret.
- Invitation tokens are never written to logs or kept in browser storage longer than the sign-in redirect needs.
- The page must not be framed (`X-Frame-Options: DENY`).

## Logging

The app has no server logs. Build diagnostics come from the Rsbuild CLI output in local runs and CI logs. Cloudflare Pages owns request analytics. Client-side logs must not contain access tokens, ID tokens or payment details.

## Build and Test

Run from the repository root:

- `pnpm install --frozen-lockfile`
- `pnpm --filter delino-accounts dev`: development server through portless at `https://accounts.delino.localhost`, in every checkout. See [Frontend development URLs](repository-environment-contract.md#frontend-development-urls). The `dev` script starts portless (`node node_modules/portless/dist/cli.js run --prefix=`) through the shared `scripts/dev/with-infisical.mjs`, which injects the public build values of the Infisical `/apps/delino-accounts` development folder, such as `PUBLIC_API_ORIGIN`, when the checkout has a local Infisical project configuration, according to the [environment contract](repository-environment-contract.md#local-development-injection). `dev:app` alone does not inject them.
- `pnpm --filter delino-accounts dev:app`, or `dev` with `PORTLESS=0`: development server at `http://127.0.0.1:13100`. The server fails when the port is in use.
- `pnpm exec turbo run build --filter=delino-accounts`: production build into `apps/delino-accounts/dist/`.
- `pnpm exec turbo run typecheck --filter=delino-accounts`: TypeScript type check, including the e2e config and tests.
- `pnpm exec turbo run test --filter=delino-accounts`: Rstest unit tests.
- `pnpm exec turbo run test:e2e --filter=delino-accounts`: browser end-to-end tests against the development server on a free loopback port. See the [e2e contract](repository-e2e-contract.md). `e2e.config.ts` passes `PUBLIC_API_ORIGIN=http://127.0.0.1:28100` to the app command, and `e2e/home.e2e.ts` asserts that the `data-api-origin` attribute of `<html>` equals it without a model key, so the test fails when the build-time variable does not reach the client bundle. The app command is `rsbuild dev --no-env --host 127.0.0.1 --port {port}`: `--no-env` stops Rsbuild from loading `.env` and `.env.local`, so a developer's local values, valid or invalid, cannot change or break the suite. Because the Rsbuild CLI then does not expose `process.env` values to `import.meta.env`, `rsbuild.config.ts` inlines the five `PUBLIC_*` variables through `source.define` (an unset variable becomes an empty string, which the readers in `src/config.ts` treat as unset). Regular `dev:app`, `dev` and `build` runs still load the dotenv files. `src/e2e-config.test.ts` fails when an e2e config of an app that has a `.env.example` drops `--no-env`. The suite sets no Logto registration; it also checks that `home` links to `/billing` and that a direct load of `/billing` shows the sign-in configuration notice. The development server serves `index.html` for every route (`server.historyApiFallback`), like the Cloudflare Pages fallback.
- `pnpm --filter delino-accounts preview`: serve the production build locally.

Billing tests: `src/billing.test.tsx` renders every billing page against an in-memory fake of the three user services (`src/testing/fake-api.ts`, a Connect router transport) and a fake checkout, and covers the actions of each role, the hidden or read-only views of `admin` and `viewer`, the validation of the minimums before a request, the reuse of the top-up idempotency key, the invitation token handling and the invoice link. `src/money.test.ts` tests the amount parsing and the display rounding with `bigint` inputs, `src/logic.test.ts` the routes, roles and form checks, `src/config.test.ts` the configuration and `src/paddle.test.ts` the Paddle.js loader with a mocked module. Checkout and top-up need a manual check on the Paddle sandbox, recorded separately from fixture and build checks.

`turbo.json` declares `PUBLIC_API_ORIGIN`, `PUBLIC_LOGTO_ENDPOINT`, `PUBLIC_LOGTO_APP_ID`, `PUBLIC_PADDLE_CLIENT_TOKEN` and `PUBLIC_PADDLE_ENVIRONMENT` as `build` environment variables and local `.env*` files as `build` inputs. It also declares the same five variables, `PORTLESS` and `PORTLESS_*` as `dev` pass-through variables, so root `pnpm dev` and `pnpm exec turbo run dev --filter=delino-accounts` honor a value provided by the shell and validate it at startup, like `pnpm --filter delino-accounts dev`. Remove `apps/delino-accounts/dist/` after validation. CI runs the e2e suite in the `e2e` job. CI runs its `build`, `typecheck` and unit `test` scripts in the `workspace-packages` job whenever a path inside the app changes and for every push to `main`, as the [workflow contract](repository-workflow-contract.md#validation) describes. That CI build does not deploy the app. The [deployment workflow](repository-workflow-contract.md#deployment-workflow) builds and deploys it when a pending changeset releases it; `PUBLIC_API_ORIGIN` is not set there, so the build uses the production API origin. The deployment workflow does not set the Logto and Paddle variables yet, so a deployed build shows that sign-in and checkout are not configured until the workflow provides the production values.

## Dependencies and Integrations

- `@rsbuild/core`, `@rsbuild/plugin-react`, `react`, `react-dom` and `@rstest/core`, pinned in `apps/delino-accounts/package.json`.
- Generated bindings: the workspace package `@delino/accounts-protos` (`workspace:*`), imported as `@delino/accounts-protos/v1/<file>_pb`, which the [protos contract](protos-delino-accounts-api-contract.md#runtime-and-language) owns, and the `@bufbuild/protobuf` `2.16.0` runtime, equal to the version of the binding package.
- Billing pages, at the exact versions that the [OwnSand console](apps-ownsand-console-foundation.md#dependencies-and-integrations) pins: `@connectrpc/connect` `2.3.0`, `@connectrpc/connect-web` `2.3.0`, `@connectrpc/connect-query` `2.3.1`, `@tanstack/react-query` `5.106.1` and `@logto/react` `4.1.3`; the shared UI [`delino-ui`](packages-delino-ui-foundation.md); `@paddle/paddle-js` `1.6.6` (Apache-2.0, released 2026-11-04) for the checkout; development: `@testing-library/react` `16.3.3`, `@testing-library/dom` `10.4.2` and `happy-dom` `21.0.4`, the versions of `delino-ui`. Record the [dependency security](repository-dependency-security-contract.md) results of each dependency change in the pull request.
- Paddle: the checkout of the subscription; the payment provider of the [API server](servers-accounts-api-foundation.md#payments-and-credits).
- `e2e`, `@e2e-dev/web` and `ai` for end-to-end tests, pinned in `apps/delino-accounts/package.json`.
- [`accounts-api`](servers-accounts-api-foundation.md) at `https://accounts.api.delino.io`.
- Logto shared tenant, according to the [authentication contract](repository-auth-contract.md).
- Cloudflare Pages: static hosting for `accounts.delino.io`. The `deploy` field of `apps/delino-accounts/package.json` names the Direct Upload Pages project `delino-accounts`. The deployment workflow builds with `pnpm exec turbo run build --filter=delino-accounts` and uploads `apps/delino-accounts/dist`. The `buildEnv` of the `deploy` field maps `PUBLIC_LOGTO_ENDPOINT` to the repository variable `LOGTO_ENDPOINT`, `PUBLIC_LOGTO_APP_ID` to `DELINO_ACCOUNTS_LOGTO_APP_ID`, `PUBLIC_PADDLE_CLIENT_TOKEN` to `DELINO_ACCOUNTS_PADDLE_CLIENT_TOKEN` and `PUBLIC_PADDLE_ENVIRONMENT` to `DELINO_ACCOUNTS_PADDLE_ENVIRONMENT`, so a production build always has the sign-in and checkout configuration and a missing variable fails the deployment (see [Deployment environments](repository-workflow-contract.md#deployment-environments)). The production build uses the default of `PUBLIC_API_ORIGIN`.

## Change Triggers

- Update [project-delino-accounts.md](project-delino-accounts.md) when paths, the production origin or the web-to-API boundary change.
- Update [servers-accounts-api-foundation.md](servers-accounts-api-foundation.md) when the app needs a new API capability or a new allowed browser origin.
- Update [repository-workflow-contract.md](repository-workflow-contract.md) when this app enters CI or a deployment workflow.
- Update `apps/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-delino-accounts.md](project-delino-accounts.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [repository-billing-contract.md](repository-billing-contract.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [protos-delino-accounts-api-contract.md](protos-delino-accounts-api-contract.md)
- [servers-accounts-api-foundation.md](servers-accounts-api-foundation.md)
- [Rsbuild documentation](https://rsbuild.rs/)
- [Rstest documentation](https://rstest.rs/)
- [Connect-Query](https://github.com/connectrpc/connect-query-es)
