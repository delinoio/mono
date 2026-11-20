# apps-ownsand-console foundation contract

## Scope

This contract owns the OwnSand web console, component `ownsand-console`, of [`ownsand`](project-ownsand.md) at `apps/ownsand/`. The console is served at `https://ownsand.delino.io`. It manages organizations, members, devices, enrollment tokens, API keys and the agent version policy, and lists sandboxes. It uses Apache-2.0.

- `apps/ownsand/package.json`: the workspace package `ownsand-console` (the component ID), its scripts and pinned dependencies.
- `apps/ownsand/rsbuild.config.ts`, `rstest.config.ts`, `lint.config.ts`, `tsconfig.json` and `turbo.json`: build, unit test, lint, type check and task configuration.
- `apps/ownsand/src/`: application source and unit tests.
- `apps/ownsand/public/`: static files copied to the build root, including the Cloudflare Pages `_headers` file. It must not contain a top-level `404.html` or a `_redirects` rule that rewrites to `/index.html`; `src/static-hosting.test.ts` enforces this, as in `delino-accounts`.
- `apps/ownsand/e2e.config.ts` and `apps/ownsand/e2e/`: the end-to-end suite according to the [e2e contract](repository-e2e-contract.md).
- `apps/ownsand/.env.example`: names and placeholders of the build-time variables.

## Runtime and Language

- Rsbuild React single-page application in TypeScript, like the [`delino-accounts` web app](apps-delino-accounts-foundation.md). It is an authenticated application, so it uses Rsbuild instead of Rspress. It uses the same exact versions of React, Rsbuild, Rstest, TypeScript, Testing Library, happy-dom and the e2e tooling as `delino-accounts` and `delino-ui`.
- Deployment: static output on Cloudflare Pages through the [deployment workflow](repository-workflow-contract.md#deployment-workflow). Deployment is not set up yet; see [Deployment](#deployment).
- Server state: React Query (`@tanstack/react-query`) with `@connectrpc/connect-query` and the generated TypeScript bindings of the [protos contract](protos-ownsand-api-contract.md), imported from `@delino/ownsand-sdk/gen/<file>_pb`. The console uses no other part of the SDK. `@connectrpc/connect-web` provides the Connect transport: the Connect protocol with JSON encoding over `fetch`.
- Authentication: the Logto React SDK (`@logto/react`).
- Routing: a small History API router in `src/router.tsx` with the route table in `src/routes.ts`; the console adds no routing dependency.
- UI components: [`delino-ui`](packages-delino-ui-foundation.md) and its stylesheet.
- Development port: `127.0.0.1:3300`, recorded in the [environment contract](repository-environment-contract.md#process-boundaries). The server fails instead of choosing another port.

### Runtime dependencies

The owner approved these runtime dependencies. Each is pinned to an exact version in `apps/ownsand/package.json`; `@tanstack/react-query` and `@logto/react` were published at least 14 days before they were added: `5.106.1` (2026-11-05) is the newest `5.x` release that met that rule on 2026-11-20, and `@logto/react` `4.1.3` was published on 2026-10-30.

| Package                     | Version   | License    | Purpose                                                             |
| --------------------------- | --------- | ---------- | ------------------------------------------------------------------- |
| `@bufbuild/protobuf`        | `2.17.0`  | Apache-2.0 | Protobuf runtime of the generated bindings.                         |
| `@connectrpc/connect`       | `2.3.0`   | Apache-2.0 | Connect client core and error codes.                                |
| `@connectrpc/connect-web`   | `2.3.0`   | Apache-2.0 | Browser `fetch` transport.                                          |
| `@connectrpc/connect-query` | `2.3.1`   | Apache-2.0 | React Query hooks for the generated method descriptors.             |
| `@tanstack/react-query`     | `5.106.1` | MIT        | Server-state cache; a required peer of `@connectrpc/connect-query`. |
| `@logto/react`              | `4.1.3`   | MIT        | Logto single-page application sign-in and access tokens.            |

`@logto/react` adds `@logto/browser`, `@logto/client`, `@logto/js`, `@silverhand/essentials`, `jose`, `js-base64` and `camelcase-keys` with its own dependencies (MIT, BSD-3-Clause and `MIT OR CC0-1.0` licenses). The [dependency security](repository-dependency-security-contract.md) check of the change that added them ran `pnpm audit` and `pnpm audit --prod` (pnpm 10.26.2) at the repository root on 2026-11-20: no known vulnerabilities.

## Users and Operators

Organization owners, admins and members who sign in with Logto. API keys do not sign in to the console.

## Interfaces and Contracts

- The console calls the control plane only through its absolute origin. It reads the origin from `PUBLIC_API_ORIGIN` and uses `https://ownsand.api.delino.io` when the value is not set. It must not import source from `servers/`. `src/config.ts` validates the value with the same rules as the [`delino-accounts` API origin](apps-delino-accounts-foundation.md#interfaces-and-contracts): an `http` or `https` origin without credentials, path, query or fragment; error messages never echo the value. `rsbuild.config.ts` fails the build and the development server on an invalid value, and `src/index.tsx` sets the `data-api-origin` attribute of `<html>` to the resolved origin.
- Authentication: the console is a Logto single-page application (application type: single-page application) that requests access tokens for the API resource indicator `https://ownsand.api.delino.io`. The resource indicator stays the same when `PUBLIC_API_ORIGIN` points to a local server. Every RPC carries the access token as `Authorization: Bearer`; the console never logs it.
  - Build-time variables `PUBLIC_LOGTO_ENDPOINT` (the tenant endpoint, an origin validated like `PUBLIC_API_ORIGIN`) and `PUBLIC_LOGTO_APP_ID` (letters, digits, `-` and `_`). Set both or neither: a build without them shows that sign-in is not configured.
  - Redirect URI: `<console origin>/callback`. Post sign-out redirect URI: `<console origin>/`. Register both for `https://ownsand.delino.io` and for `http://127.0.0.1:3300` in the shared tenant.
  - Open: the application registration in the shared tenant and the scopes of the API resource. The [control plane contract](servers-ownsand-api-foundation.md#authentication) records the scope names with token validation; the console then adds them to its Logto configuration. Until then the console requests the resource without extra scopes.
  - After the sign-in callback, the console opens `/invitations/accept` when an invitation secret waits for the sign-in, and `/` otherwise. It stores no return path.
- Account settings: the console links to `https://accounts.delino.io`. It has no profile or account pages.
- Member display names: `ListMembers` returns the display name of each member, which the control plane reads from the `delino-accounts` API. The console does not call the `delino-accounts` API and needs no token for it. It shows the Logto user ID when a member has no display name. It does not store profiles.

| Route ID            | Path                                      | Content                                                                                 | Minimum role                                                               |
| ------------------- | ----------------------------------------- | --------------------------------------------------------------------------------------- | -------------------------------------------------------------------------- |
| `organizations`     | `/`                                       | Organization list and creation; invitation acceptance entry.                            | signed in                                                                  |
| `invitation`        | `/invitations/accept`                     | Accept an invitation secret as the signed-in user.                                      | signed in                                                                  |
| `sign-in-callback`  | `/callback`                               | Logto sign-in callback; returns to the invitation flow or to `/`.                       | none                                                                       |
| `sandboxes`         | `/orgs/:organizationId/sandboxes`         | Sandbox list with state, device, resources, TTL deadlines and labels.                   | `member`                                                                   |
| `devices`           | `/orgs/:organizationId/devices`           | Devices with state, capabilities, limits, allocation and agent version.                 | `member`                                                                   |
| `device`            | `/orgs/:organizationId/devices/:deviceId` | Device details; the per-device admin settings (limits and removal) for `admin` and up.  | `member` (limits and removal need `admin`)                                 |
| `members`           | `/orgs/:organizationId/members`           | Members with display names and roles; invitations for `admin` and above.                | `member` (leaving needs `admin` or `member`; other changes need `admin`)   |
| `enrollment-tokens` | `/orgs/:organizationId/enrollment-tokens` | Create, list and revoke enrollment tokens.                                              | `admin`                                                                    |
| `api-keys`          | `/orgs/:organizationId/api-keys`          | Create, list and revoke API keys.                                                       | `admin`                                                                    |
| `settings`          | `/orgs/:organizationId/settings`          | Agent version policy (read for `member`), ownership transfer and organization deletion. | `member` (policy changes need `admin`; transfer and deletion need `owner`) |

- Organization routes show a navigation bar with only the routes that the caller's role allows; `GetOrganization` returns the caller's role. A route whose minimum role the caller does not have shows a message instead of its content. Unknown paths show a not-found view.
- Invitation links have the form `<console origin>/invitations/accept#secret=<invitation secret>`. `CreateInvitation` returns the secret, and the console builds the link from its own origin. The organizations page also accepts a pasted invitation link and opens the same flow.
- The invitation secret arrives in the URL fragment of the invitation link, so it does not reach Cloudflare Pages logs. The console removes it from the address bar after it reads it. When the user is not signed in, the console keeps the secret in `sessionStorage` only for the Logto sign-in redirect, because the fragment does not survive the redirect. It deletes the secret from `sessionStorage` when `AcceptInvitation` returns, also when the call fails, and when the user navigates inside the console away from the invitation flow. The sign-in redirect and its return are part of the flow and do not delete the secret. The `sessionStorage` key is `ownsand.pendingInvitationSecret`. A signed-in user who opens a link accepts with an explicit button; the secret then stays in memory only.
- New secrets (invitation links, enrollment tokens, API keys) appear once in a dialog with a copy action and a statement that they will not appear again. The secret lives only in React state and is dropped when the dialog closes.
- Per-device admin settings (decision): they are on the device detail route `device` (`/orgs/:organizationId/devices/:deviceId`), which the devices list links to for every member. The page shows the device details to every member. Below them, an "Admin settings" section, shown only to `admin` and `owner` and not for a removed device, holds two cards: "Limits", a form with the vCPU, memory (MiB) and disk (GiB) limits that calls `UpdateDeviceLimits`, and "Remove device", a danger card whose button opens the confirmation step and then calls `RemoveDevice`.
- Enrollment tokens take a device name and a lifetime of 1, 6 or 24 hours (default 1 hour). The agent version policy form offers follow the default, pin a version and hold updates.
- The console hides actions that the user's role does not allow, and the control plane still enforces every permission.
- UX follows the Toss Design Guidelines. Dialogs close with `Esc`. A form with a single critical input focuses it when it opens: the organization name, the enrollment token device name, the API key name and, when the user has no organization yet, the invitation link entry.
- Error messages map the Connect code to a fixed message and never show the server message.
- Account settings: the header links to `https://accounts.delino.io`.

## Storage

The console stores nothing except the Logto SDK session state in the browser (the SDK's own `localStorage` and `sessionStorage` keys). It never stores API keys or enrollment tokens in browser storage. The only exception for invitation secrets is the short `sessionStorage` use during the sign-in redirect described above.

## Security

- Secrets appear once and are never stored by the console, except the short `sessionStorage` use of an invitation secret during the sign-in redirect described in [Interfaces and Contracts](#interfaces-and-contracts).
- Destructive actions (device removal, API key revocation, organization deletion, ownership transfer) need an explicit confirmation step. The console also confirms member removal, leaving the organization and the revocation of invitations and enrollment tokens. The confirmation is an alert dialog that focuses "Cancel" first.
- End-to-end sign-in stand-in: `src/auth/fake-auth.tsx` replaces Logto only when the development server runs with `PUBLIC_OWNSAND_E2E_FAKE_AUTH=1`, which only `e2e.config.ts` sets. `src/index.tsx` loads it behind the build-time `import.meta.env.DEV` constant, so production bundles do not contain it, and `rsbuild.config.ts` fails a production build that sets the variable. The stand-in keeps a signed-in flag (no token) in `sessionStorage` and sends a fixed bearer value to the mocked API.
- The console shows no exec output or file contents; it has no exec or file features in this version.

## Logging

The console sends no logs to a service in this version. Browser console messages never contain tokens or secrets.

## Build and Test

Package scripts, run through root Turborepo tasks or `pnpm exec turbo run <task> --filter=ownsand-console`:

- `dev`: `rsbuild dev` on `127.0.0.1:3300`. Root `pnpm dev` starts it.
- `build`: `rsbuild build` into `apps/ownsand/dist/`. Remove `dist` after validation.
- `lint`: `rstack lint -c lint.config.ts`, Rslint through the root pinned `rstack` with the recommended JavaScript rules and the type-checked TypeScript rules. The file is not named `rstack.config.ts`, so it does not change the repository formatter configuration.
- `typecheck`: `tsc --noEmit`, including the e2e config and tests.
- `test`: Rstest unit tests with happy-dom and Testing Library. They cover the configuration resolver, the route table, the role matrix and the role-based navigation, the invitation secret handling (fragment reading and removal, `sessionStorage` use and deletion on return, on failure and on navigation away) and the one-time secret dialog (statement, copy, `Esc`, no storage).
- `test:e2e`: the end-to-end suite according to the [e2e contract](repository-e2e-contract.md). `e2e.config.ts` passes `PUBLIC_API_ORIGIN=http://127.0.0.1:18300`, an origin that nothing listens on, and `PUBLIC_OWNSAND_E2E_FAKE_AUTH=1`. `e2e/mock-api.ts` answers every API request with `browser.route` at the network layer, so the suite needs no control plane and no Logto tenant. The suite covers sign-in through the stand-in, the bearer header, the account settings link, the API origin, the sandbox list, the invitation flow, role-based hiding, the per-device admin settings, a one-time API key dialog with `Esc` and a confirmed revocation. It has no agent-tagged tests.

`turbo.json` passes `PUBLIC_API_ORIGIN`, `PUBLIC_LOGTO_ENDPOINT` and `PUBLIC_LOGTO_APP_ID` through to `dev`, and declares them and `PUBLIC_OWNSAND_E2E_FAKE_AUTH` as `build` environment variables, with local `.env*` files as `build` inputs.

The tests do not cover a real Logto sign-in, a real control plane, the production build served by Cloudflare Pages or a deployed origin.

## Deployment

The console is not deployed yet. Its `package.json` has no `deploy` field on purpose: the [deployment workflow](repository-workflow-contract.md#deployment-workflow) deploys every package with a `deploy` field that a pending changeset releases, and Changesets can add the console to a release when `@delino/ownsand-sdk`, a `workspace:*` dependency, is released. Before the first deployment, a change must create the Direct Upload Pages project, register the Logto application, decide how the deployment build receives `PUBLIC_LOGTO_ENDPOINT` and `PUBLIC_LOGTO_APP_ID`, and then add `"deploy": { "target": "cloudflare-pages", "project": "<project>" }` with a changeset.

## Dependencies and Integrations

- Upstream: the [control plane](servers-ownsand-api-foundation.md), the Logto shared tenant and the [protos contract](protos-ownsand-api-contract.md) through the generated bindings of `@delino/ownsand-sdk`.
- Shared UI: [`delino-ui`](packages-delino-ui-foundation.md).
- `delino-accounts`: link target for account settings.

## Change Triggers

- Update this contract when routes, the origin or the public RPCs that the console uses change.
- Update [project-ownsand.md](project-ownsand.md) when the origin or the component ID changes.
- Update `apps/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-ownsand.md](project-ownsand.md)
- [apps-delino-accounts-foundation.md](apps-delino-accounts-foundation.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [repository-e2e-contract.md](repository-e2e-contract.md)
