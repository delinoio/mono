# apps-ownsand-console foundation contract

## Scope

This contract owns the OwnSand web console, component `ownsand-console`, of [`ownsand`](project-ownsand.md) at `apps/ownsand/` (workspace package `ownsand-console`). The console is served at `https://ownsand.delino.io`. It manages organizations, members, devices, enrollment tokens, API keys and the agent version policy, and lists sandboxes. It uses Apache-2.0.

## Runtime and Language

- Rsbuild React single-page application in TypeScript, like the [`delino-accounts` web app](apps-delino-accounts-foundation.md). It is an authenticated application, so it uses Rsbuild instead of Rspress.
- Deployment: static output on Cloudflare Pages through the [deployment workflow](repository-workflow-contract.md#deployment-workflow). The `deploy` field of `apps/ownsand/package.json` is `{ "target": "cloudflare-pages", "project": "ownsand-console" }`; the Pages project must exist before the first deployment. `public/_headers` sets `X-Content-Type-Options`, `Referrer-Policy` and `X-Frame-Options: DENY`; the output has no top-level `404.html`, so Cloudflare Pages serves the single-page-application fallback.
- Server state: React Query with `@connectrpc/connect-query` and the TypeScript bindings from the [protos contract](protos-ownsand-api-contract.md), imported from the workspace package `ownsand-sdk` (`ownsand-sdk/gen/<file>_pb`). The console imports only the generated bindings, not the SDK client. The transport is `@connectrpc/connect-web` with the binary Connect protocol. Queries retry only `unavailable` (twice); mutations never retry. A mutation invalidates the queries of the services it changes.
- Routing: a small history-API router in `src/router.tsx` with the route IDs below; no routing library.
- UI components: [`delino-ui`](packages-delino-ui-foundation.md).
- Development URL: `pnpm --filter ownsand-console dev` runs the console through portless at `https://ownsand.delino.localhost`, in every checkout. `dev:app`, and `dev` with `PORTLESS=0`, use the fixed port `127.0.0.1:13300` (`strictPort`). Both are recorded in the [environment contract](repository-environment-contract.md#frontend-development-urls). Sign-in in development uses the console origin of the run, so the Logto development application must list `https://ownsand.delino.localhost/callback` and `https://ownsand.delino.localhost` (and the `http://127.0.0.1:13300` equivalents for `dev:app`).

## Users and Operators

Organization owners, admins and members who sign in with Logto. API keys do not sign in to the console.

## Interfaces and Contracts

- The console calls the control plane only through its absolute origin. It reads the origin from `PUBLIC_API_ORIGIN` and uses `https://ownsand.api.delino.io` when the value is not set. It must not import source from `servers/`.
- Authentication: the console is a Logto single-page application (`@logto/react`) that requests access tokens for the API resource `https://ownsand.api.delino.io`. The sign-in redirect URI is `<console origin>/callback` and the post sign-out redirect URI is the console origin; register both in the Logto application. The Logto SDK keeps its session in the browser. Recorded decision: the console always requests the scope `ownsand:use`, which the [control plane](servers-ownsand-api-foundation.md#authentication) requires in every user access token. The tenant endpoint, the application ID and any extra scopes are build-time configuration (`PUBLIC_LOGTO_ENDPOINT`, `PUBLIC_LOGTO_APP_ID`, `PUBLIC_LOGTO_SCOPES`, space-separated), because the Logto application registration is not recorded yet. A build without the endpoint or the application ID shows that sign-in is not configured instead of failing. Open item: record the registration (application ID, tenant endpoint) here and make them the defaults in `src/config.ts` before the first deployment, because the deployment workflow passes no build variables.
- Build-time variables (all public, inlined into the bundle, listed in `apps/ownsand/.env.example`): `PUBLIC_API_ORIGIN` (default `https://ownsand.api.delino.io`; an http or https origin without credentials, path, query or fragment), `PUBLIC_LOGTO_ENDPOINT`, `PUBLIC_LOGTO_APP_ID`, `PUBLIC_LOGTO_SCOPES` and `PUBLIC_OWNSAND_MOCK_API` (`true` or `false`). `rsbuild.config.ts` validates them and fails the build or development server on an invalid value. The development task passes them through Turbo `passThroughEnv`, and the build task declares them as `env`.
- Mock API (recorded decision): `PUBLIC_OWNSAND_MOCK_API=true` runs the console against an in-browser fake control plane (`src/mock/mock-api.ts`, a Connect router transport with seeded organizations, devices, sandboxes, snapshots and members that applies the role matrix) and a fake signed-in user, for local UI work and the [end-to-end suite](#build-and-test). A production build rejects the setting, and the mock module is a separate chunk that the minifier removes from every other build.
- Account settings: the console links to `https://accounts.delino.io`. It has no profile or account pages.
- Member display names: `ListMembers` returns the display name of each member, which the control plane reads from the `delino-accounts` API. The console does not call the `delino-accounts` API and needs no token for it. It shows the Logto user ID when a member has no display name. It does not store profiles.

| Route ID            | Path                                         | Content                                                                                 | Minimum role                                                               |
| ------------------- | -------------------------------------------- | --------------------------------------------------------------------------------------- | -------------------------------------------------------------------------- |
| `organizations`     | `/`                                          | Organization list and creation; invitation acceptance entry.                            | signed in                                                                  |
| `invitation`        | `/invitations/accept`                        | Accept an invitation secret as the signed-in user.                                      | signed in                                                                  |
| `callback`          | `/callback`                                  | Completes the Logto sign-in redirect and returns to a pending invitation or `/`.        | —                                                                          |
| `sandboxes`         | `/orgs/:organizationId/sandboxes`            | Sandbox list with state, device, resources, TTL deadlines and labels.                   | `member`                                                                   |
| `sandbox`           | `/orgs/:organizationId/sandboxes/:sandboxId` | One sandbox: details, labels, pause, resume, snapshot and delete.                       | `member`                                                                   |
| `snapshots`         | `/orgs/:organizationId/snapshots`            | Snapshots with export, templates from snapshots, and deletion of both.                  | `member`                                                                   |
| `devices`           | `/orgs/:organizationId/devices`              | Devices with state, capabilities, limits, allocation and agent version.                 | `member`                                                                   |
| `device`            | `/orgs/:organizationId/devices/:deviceId`    | One device: capabilities, allocation, limits with vCPU overcommit, and removal.         | `member` (limits and removal need `admin`)                                 |
| `members`           | `/orgs/:organizationId/members`              | Members with display names and roles; invitations for `admin` and above.                | `member` (leaving needs `admin` or `member`; other changes need `admin`)   |
| `enrollment-tokens` | `/orgs/:organizationId/enrollment-tokens`    | Create, list and revoke enrollment tokens.                                              | `admin`                                                                    |
| `api-keys`          | `/orgs/:organizationId/api-keys`             | Create, list and revoke API keys.                                                       | `admin`                                                                    |
| `settings`          | `/orgs/:organizationId/settings`             | Agent version policy (read for `member`), ownership transfer and organization deletion. | `member` (policy changes need `admin`; transfer and deletion need `owner`) |

- Recorded console behavior: the sandbox list filters by state and device and shows ended sandboxes only on request (`include_ended`), with "Show more" paging through `page_token`; an `UNREACHABLE` sandbox shows its last known state. The create dialog requires the idle TTL (minutes) and the maximum TTL (hours, at most 168), checks that the idle TTL is not longer than the maximum TTL, takes the source (base image, template or ready snapshot), resources, network mode, an optional device and labels (`key=value` lines, with the warning that every member and API key can read them), and generates one UUID v7 `idempotency_key` for each opened dialog, which every retry of that create reuses. Other lists request `page_size` 200 and show the first page. The sandbox list, the sandbox page and the snapshot list refetch every 3 seconds while a sandbox they show is `CREATING` or `DELETING`, or a snapshot is `CREATING`, `DELETING` or `EXPORTING`, because the control plane returns from these operations before the agent finishes them.
- The invitation secret arrives in the URL fragment of the invitation link, so it does not reach Cloudflare Pages logs. The link is `<console origin>/invitations/accept#secret=<secret>`. The console removes the fragment from the address bar (`history.replaceState`) after it reads it. When the user is not signed in, the console keeps the secret in `sessionStorage` (key `ownsand.pending-invitation`) only for the Logto sign-in redirect, because the fragment does not survive the redirect. It deletes the secret from `sessionStorage` when `AcceptInvitation` returns, also when the call fails, and when the user navigates inside the console away from the invitation flow. When the call fails, the page shows the error and keeps the "Accept invitation" button and the secret in memory (not in `sessionStorage`), so that the user can try again by hand, because the control plane leaves the invitation unused after a failure. The console never retries automatically and shows no separate retry button. A page without a secret shows the missing-invitation-link screen, and opening a new link on the page clears the error of the previous one. The sign-in redirect and its return are part of the flow and do not delete the secret.
- New secrets (invitation links, enrollment tokens, API keys) appear once in a dialog with a copy action and a statement that they will not appear again.
- Per-device admin settings UI (recorded decision): the `device` route. Its first section lists the platform, agent version and update state, last-seen time, host capacity, supported guests and the allocation. The limits section has the vCPU, memory (MiB) and disk (GiB) limits and a vCPU overcommit select from 100% to 400% in steps of 25, with a live "Effective vCPU limit" (floor of vCPU limit × percentage / 100, in integers) and a save button. A danger zone removes the device after a confirmation. Members see the same values read-only. The `devices` list shows the allocated vCPUs as `<allocated> of <effective> (<limit> × <ratio>)`, where `<effective>` is the exact `Device.effective_vcpu_limit_count` (an `int64`, so a bigint in the SDK; for example `2147483648`), derived as floor(`limits.vcpu_count` × `vcpu_overcommit_percent` / 100) from the returned limits when that field is 0 because the server predates it, and `0` for a device with zero limits; the console never reads the capped `int32` `effective_vcpu_limit` field. The "Add a device" action of the `devices` list, for `admin` and above, leads to the enrollment tokens.
- Organization switcher: a select in the top bar lists the caller's organizations and keeps the current section when it switches. The top bar also links to `https://accounts.delino.io` ("Account settings") and signs out.
- The console hides actions that the user's role does not allow, and the control plane still enforces every permission. It reads the role from `Organization.caller_role` of `GetOrganization` and applies the [authorization matrix](servers-ownsand-api-foundation.md#authorization): enrollment tokens, API keys, invitations, member roles and removal, device limits and removal and agent version policy changes for `admin` and above; leaving for `admin` and `member`; ownership transfer and deletion for `owner`. An `admin` cannot change or remove the `owner`. When the caller's role does not allow the route's section (the minimum role in the route table), for example after the organization switcher keeps the API keys section in an organization where the caller is a `member`, or from a bookmark, the console shows a permission notice with a link to the sandboxes instead of the page.
- Enrollment token lifetimes offered: 1, 6, 12 and 24 hours. The token dialog says to pass the token to `ownsand-agent enroll` on standard input, not as an argument.
- UX follows the Toss Design Guidelines: plain surfaces on a neutral background, one primary action per view, plain-language copy and generous touch targets, with `delino-ui` components and its `--dui-*` tokens (light and dark). Dialogs close with `Esc`. A form with a single critical input (organization name, API key name, template name) focuses it when it opens. Destructive confirmations put the cancel button first.

## Storage

The console stores nothing except the Logto SDK session state in the browser. It never stores API keys or enrollment tokens in browser storage. The only exception for invitation secrets is the short `sessionStorage` use during the sign-in redirect described above.

## Security

- Secrets appear once and are never stored by the console, except the short `sessionStorage` use of an invitation secret during the sign-in redirect described in [Interfaces and Contracts](#interfaces-and-contracts).
- Destructive actions (device removal, API key revocation, organization deletion, ownership transfer) need an explicit confirmation step.
- The console shows no exec output or file contents; it has no exec or file features in this version.

## Logging

The console sends no logs to a service in this version. Browser console messages never contain tokens or secrets.

## Build and Test

- Package scripts: `dev` (`rsbuild dev`), `build` (`rsbuild build` into the ignored `dist/`), `lint` and `typecheck` (`tsc --noEmit`), `test` (`rstest run` with `happy-dom` and Testing Library) and `test:e2e` (`e2e run`), run through root Turborepo tasks, for example `pnpm exec turbo run test --filter=ownsand-console`.
- Unit tests cover the configuration rules, the invitation secret handling (fragment removal, `sessionStorage` only, deletion when leaving the flow and after `AcceptInvitation`), the role matrix, the effective vCPU limit, the TTL and label rules and the router, and render the console against the mock API: role-based navigation, a one-time API key secret that never reaches browser storage and closes with `Esc`, the device limits and the invitation flow.
- End-to-end suite: `apps/ownsand/e2e/` according to the [e2e contract](repository-e2e-contract.md), against the development server with `PUBLIC_OWNSAND_MOCK_API=true`. It does not reach a control plane or Logto; acceptance against them is not covered.

## Dependencies and Integrations

- Upstream: the [control plane](servers-ownsand-api-foundation.md), the Logto shared tenant and the [protos contract](protos-ownsand-api-contract.md) (bindings from [`ownsand-sdk`](packages-ownsand-sdk-foundation.md)).
- Dependencies (exact pins, each released at least two weeks before it was pinned; record `pnpm audit` results in the pull request): `@connectrpc/connect` `2.3.0`, `@connectrpc/connect-web` `2.3.0`, `@connectrpc/connect-query` `2.3.1`, `@tanstack/react-query` `5.106.1`, `@bufbuild/protobuf` `2.16.0`, `@logto/react` `4.1.3`, React `19.3.1`; development: the Rsbuild, Rstest, Testing Library, `happy-dom` and e2e versions of `delino-accounts` and `delino-ui`.
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
