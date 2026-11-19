# apps-ownsand-console foundation contract

## Scope

This contract owns the OwnSand web console, component `ownsand-console`, of [`ownsand`](project-ownsand.md) at `apps/ownsand/` (`planned`). The console is served at `https://ownsand.delino.io`. It manages organizations, members, devices, enrollment tokens, API keys and the agent version policy, and lists sandboxes. It uses Apache-2.0.

## Runtime and Language

- Rsbuild React single-page application in TypeScript, like the [`delino-accounts` web app](apps-delino-accounts-foundation.md). It is an authenticated application, so it uses Rsbuild instead of Rspress.
- Deployment: static output on Cloudflare Pages through the [deployment workflow](repository-workflow-contract.md#deployment-workflow).
- Server state: React Query with `@connectrpc/connect-query` and the TypeScript bindings from the [protos contract](protos-ownsand-api-contract.md).
- UI components: [`delino-ui`](packages-delino-ui-foundation.md).
- Development port (`planned`): `127.0.0.1:3300`. Add it to the [environment contract](repository-environment-contract.md) with the implementation.

## Users and Operators

Organization owners, admins and members who sign in with Logto. API keys do not sign in to the console.

## Interfaces and Contracts

- The console calls the control plane only through its absolute origin. It reads the origin from `PUBLIC_API_ORIGIN` and uses `https://ownsand.api.delino.io` when the value is not set. It must not import source from `servers/`.
- Authentication: the console is a Logto single-page application that requests access tokens for the API resource `https://ownsand.api.delino.io`. Record the application registration and scopes with the implementation.
- Account settings: the console links to `https://accounts.delino.io`. It has no profile or account pages.
- Member display names: `ListMembers` returns the display name of each member, which the control plane reads from the `delino-accounts` API. The console does not call the `delino-accounts` API and needs no token for it. It shows the Logto user ID when a member has no display name. It does not store profiles.

| Route ID            | Path                                      | Content                                                                                 | Minimum role                                                               |
| ------------------- | ----------------------------------------- | --------------------------------------------------------------------------------------- | -------------------------------------------------------------------------- |
| `organizations`     | `/`                                       | Organization list and creation; invitation acceptance entry.                            | signed in                                                                  |
| `invitation`        | `/invitations/accept`                     | Accept an invitation secret as the signed-in user.                                      | signed in                                                                  |
| `sandboxes`         | `/orgs/:organizationId/sandboxes`         | Sandbox list with state, device, resources, TTL deadlines and labels.                   | `member`                                                                   |
| `devices`           | `/orgs/:organizationId/devices`           | Devices with state, capabilities, limits, allocation and agent version.                 | `member`                                                                   |
| `members`           | `/orgs/:organizationId/members`           | Members with display names and roles; invitations for `admin` and above.                | `member` (leaving needs `admin` or `member`; other changes need `admin`)   |
| `enrollment-tokens` | `/orgs/:organizationId/enrollment-tokens` | Create, list and revoke enrollment tokens.                                              | `admin`                                                                    |
| `api-keys`          | `/orgs/:organizationId/api-keys`          | Create, list and revoke API keys.                                                       | `admin`                                                                    |
| `settings`          | `/orgs/:organizationId/settings`          | Agent version policy (read for `member`), ownership transfer and organization deletion. | `member` (policy changes need `admin`; transfer and deletion need `owner`) |

- The invitation secret arrives in the URL fragment of the invitation link, so it does not reach Cloudflare Pages logs. The console removes it from the address bar after it reads it. When the user is not signed in, the console keeps the secret in `sessionStorage` only for the Logto sign-in redirect, because the fragment does not survive the redirect. It deletes the secret from `sessionStorage` when `AcceptInvitation` returns, also when the call fails, and when the user navigates inside the console away from the invitation flow. The sign-in redirect and its return are part of the flow and do not delete the secret.
- New secrets (invitation links, enrollment tokens, API keys) appear once in a dialog with a copy action and a statement that they will not appear again.
- The per-device admin settings UI (device limits and removal) is an open decision. Record its route and layout here before the implementation.
- The console hides actions that the user's role does not allow, and the control plane still enforces every permission.
- UX follows the Toss Design Guidelines. Dialogs close with `Esc`. A form with a single critical input focuses it when it opens.

## Storage

The console stores nothing except the Logto SDK session state in the browser. It never stores API keys or enrollment tokens in browser storage. The only exception for invitation secrets is the short `sessionStorage` use during the sign-in redirect described above.

## Security

- Secrets appear once and are never stored by the console.
- Destructive actions (device removal, API key revocation, organization deletion, ownership transfer) need an explicit confirmation step.
- The console shows no exec output or file contents; it has no exec or file features in this version.

## Logging

The console sends no logs to a service in this version. Browser console messages never contain tokens or secrets.

## Build and Test

- Package scripts `dev`, `build`, `lint`, `typecheck` and `test` run through root Turborepo tasks. Record them with the implementation.
- Add an end-to-end suite according to the [e2e contract](repository-e2e-contract.md) when the console has its first route.

## Dependencies and Integrations

- Upstream: the [control plane](servers-ownsand-api-foundation.md), the Logto shared tenant and the [protos contract](protos-ownsand-api-contract.md).
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
