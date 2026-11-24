# apps-tide-console foundation contract

## Scope

This contract owns the tide web console, component `tide-console`, of [`tide`](project-tide.md) at `apps/tide/` (workspace package `tide-console`). The console is served at `https://tide.delino.io`. It manages tide organizations, members, the OwnSand link, GitHub installations, agent integrations and runner profiles, and shows the queue, the runners and the device pool.

## Runtime and Language

- Rsbuild React single-page application in TypeScript, like the [OwnSand console](apps-ownsand-console-foundation.md).
- Deployment: static output on Cloudflare Pages through the [deployment workflow](repository-workflow-contract.md#deployment-workflow). The `deploy` field of `apps/tide/package.json` names the Pages project `tide-console`, which must exist before the first deployment, and its `buildEnv` maps `PUBLIC_LOGTO_ENDPOINT` to the repository variable `LOGTO_ENDPOINT` and `PUBLIC_LOGTO_APP_ID` to `TIDE_CONSOLE_LOGTO_APP_ID`, so a production build always has the sign-in configuration and a missing variable fails the deployment (see [Deployment environments](repository-workflow-contract.md#deployment-environments)). The production build uses the defaults of `PUBLIC_API_ORIGIN` and `PUBLIC_LOGTO_SCOPES`.
- Server state: React Query with `@connectrpc/connect-query` and the TypeScript code from the [protos contract](protos-tide-api-contract.md).
- UI components: [`delino-ui`](packages-delino-ui-foundation.md).
- Development port: `127.0.0.1:13400` for `dev:app`, behind the portless name `tide.delino` (`https://tide.delino.localhost`), according to the [environment contract](repository-environment-contract.md#frontend-development-urls). The control plane allows both development origins in its local CORS configuration.

## Users and Operators

Organization owners, admins and members who sign in with Logto.

## Interfaces and Contracts

- The console calls the control plane only through its absolute origin. It reads the origin from `PUBLIC_API_ORIGIN` and uses `https://tide.api.delino.io` when the value is not set. It must not import source from `servers/`.
- Authentication: the console is a Logto single-page application (`@logto/react`) that requests access tokens for the API resource `https://tide.api.delino.io`. The Logto SDK keeps its session in the browser. The console reports loading to its pages only until the SDK has restored the session for the first time, so a token request never unmounts the page that made it.
- Logto application registration (recorded with placeholders until the tenant registration exists):

  | Item                       | Value                                                                                                          |
  | -------------------------- | -------------------------------------------------------------------------------------------------------------- |
  | Application type           | Single-page application                                                                                        |
  | Application ID             | `<tide-console-logto-app-id>` (`PUBLIC_LOGTO_APP_ID`)                                                          |
  | Tenant endpoint            | `<logto-tenant-endpoint>` (`PUBLIC_LOGTO_ENDPOINT`)                                                            |
  | Sign-in redirect URI       | `<console origin>/callback`, for example `https://tide.delino.io/callback`                                     |
  | Post sign-out redirect URI | `<console origin>`                                                                                             |
  | API resource               | `https://tide.api.delino.io`                                                                                   |
  | Scopes                     | `tide:use`, which the control plane requires; the console always requests it. `PUBLIC_LOGTO_SCOPES` adds more. |

  A build without the endpoint or the application ID shows that sign-in is not configured instead of failing. A production build from the deployment workflow always has both values, because `deploy.buildEnv` maps them to repository variables. Register the single-page application in the shared Logto tenant and store its ID in the repository variable `TIDE_CONSOLE_LOGTO_APP_ID` before the first deployment.

- Build-time variables (all public, inlined into the bundle, listed in `apps/tide/.env.example`): `PUBLIC_API_ORIGIN` (default `https://tide.api.delino.io`; an http or https origin without credentials, path, query or fragment), `PUBLIC_LOGTO_ENDPOINT`, `PUBLIC_LOGTO_APP_ID`, `PUBLIC_LOGTO_SCOPES` and `PUBLIC_TIDE_MOCK_API` (`true` or `false`). `rsbuild.config.ts` validates them and fails the build or development server on an invalid value. The Turbo `dev` task passes them through `passThroughEnv`, and the `build` task declares them as `env`.
- Mock API (recorded decision): `PUBLIC_TIDE_MOCK_API=true` runs the console against an in-browser fake control plane (`src/mock/mock-api.ts`, a Connect router transport with seeded organizations for each role, an `INVALID` OwnSand link, an organization without setup, requests, runners, devices, dev sessions, integrations and an invitation) that applies the role matrix, and a fake signed-in user whose session lives in `sessionStorage` like a provider session. It serves the development server, the unit tests and the end-to-end suite. A production build rejects the setting, and the mock module is a separate chunk that the minifier removes from every other build.
- Server state: the pages call the generated services with `@connectrpc/connect-query`. The `overview` route uses the `watchQueue` helper of the [TypeScript SDK](packages-tide-sdk-foundation.md) (`tide-sdk`) on the console transport for the reconnects and the gap handling.
- The console links to the OwnSand console at `https://ownsand.delino.io`, and to the devices page of the linked OwnSand organization (`https://ownsand.delino.io/orgs/<ownsand_organization_id>/devices`).
- Account settings: the console links to `https://accounts.delino.io`. It has no profile or account pages.
- The console never calls `ownsand-api`. Device management (enrollment, device limits including the vCPU overcommit, device removal) stays in the [OwnSand console](apps-ownsand-console-foundation.md), and the tide console links to it.

| Route ID        | Path                                 | Content                                                                                                                                               | Minimum role |
| --------------- | ------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------- | ------------ |
| `organizations` | `/`                                  | Organization list and creation; invitation acceptance entry.                                                                                          | signed in    |
| `invitation`    | `/invitations/accept`                | Accept an invitation secret as the signed-in user.                                                                                                    | signed in    |
| `callback`      | `/callback`                          | Completes the Logto sign-in redirect and returns to `/`.                                                                                              | —            |
| `github-setup`  | `/github/setup`                      | Callback of the GitHub App installation. Calls `LinkGitHubInstallation` and continues to `integrations`.                                              | `admin`      |
| `overview`      | `/orgs/:organizationId`              | Live queue (waiting requests in queue order), runners by state and the device pool from `GetCapacity`.                                                | `member`     |
| `requests`      | `/orgs/:organizationId/requests`     | Request list with filters, states, end reasons and queue wait; cancel for `admin`.                                                                    | `member`     |
| `runners`       | `/orgs/:organizationId/runners`      | Runner list with profile, device, state and end reason; delete for `admin`.                                                                           | `member`     |
| `profiles`      | `/orgs/:organizationId/profiles`     | Runner profiles; create, change and delete for `admin`. For `GITHUB_JOB` profiles, shows the `runs-on` label to copy.                                 | `member`     |
| `integrations`  | `/orgs/:organizationId/integrations` | OwnSand link, GitHub installations with runner groups, and agent integrations with credential state; changes for `admin`.                             | `member`     |
| `members`       | `/orgs/:organizationId/members`      | Members and roles; invitations for `admin` and above.                                                                                                 | `member`     |
| `dev-sessions`  | `/orgs/:organizationId/dev-sessions` | Dev sessions of the organization with owner, profile, guest OS, state and end reason. Delete for the session owner, and for `admin` on every session. | `member`     |
| `api-keys`      | `/orgs/:organizationId/api-keys`     | The signed-in user's personal API keys: create (shows the secret once), list and revoke.                                                              | `member`     |
| `audit`         | `/orgs/:organizationId/audit`        | Audit events.                                                                                                                                         | `member`     |
| `settings`      | `/orgs/:organizationId/settings`     | The per-user dev session limit (`admin`), ownership transfer and organization deletion (`owner`).                                                     | `admin`      |

- The `overview` route uses `WatchQueue` and reconnects when the stream ends. After a gap, and after a manual retry of a stream that ended with an error, it keeps the last state on screen, marked as reconnecting, until the new snapshot is complete, and then replaces it. It shows the waiting requests in queue order (priority, then queue time, then request ID).
- Invitation links carry the secret in the URL fragment (`/invitations/accept#secret=<secret>`). The `invitation` route reads it, removes the fragment from the address bar at once and keeps the secret only in component state; it never stores it. A signed-out user sees the sign-in with a notice to open the link again after sign-in, because the console keeps no state across the Logto redirect.
- The `github-setup` route reads `installation_id`, `code` and `state` and removes the query from the address bar at once. The callback URL does not name the tide organization, so the route calls `LinkGitHubInstallation` for the only organization in which the user is `owner` or `admin`, or asks the user to choose one. After a failure it explains that the installation must start again from `integrations`, because the `state` is single-use.
- On the `integrations` route, "Show more" loads the next page of GitHub installations for the selected organization. A page failure keeps the loaded installations visible and lets the user retry the same page. Owners and admins can manage installations on every loaded page.
- Empty states guide the first setup in this order: link OwnSand, connect GitHub or create an agent integration, create a runner profile. Each empty state names the next step.
- When the OwnSand link is `INVALID`, every organization route shows a banner that explains that dispatch has stopped and links to `integrations`.
- Credential forms accept a value once and never show it again. After a save, the form shows only that the credential is set and when.
- For an `OPENAI_CODEX` integration, the console shows the webhook URL to register in OpenAI.
- The `profiles` route requires `workspace_path` for `DEV_SESSION` profiles. It offers the default priority of the workload kind only when it creates a profile, because an update requires `HIGH`, `NORMAL` or `LOW`. The `dev-sessions` route shows no command, output or file content, and it does not run commands; commands and file transfers are available only through the CLI, the MCP server and the SDKs.
- The `api-keys` route shows a new secret once with a copy action and a notice that tide cannot show it again, and explains that the key works only for dev sessions. It lists the keys page by page with "Show more", so the console can show and revoke every key.
- Organization deletion tells the owner to uninstall the GitHub App in GitHub, because tide cannot uninstall it.
- The console hides actions that the user's role does not allow; the control plane still enforces every permission.
- UX follows the Toss Design Guidelines. Dialogs close with `Esc`. A form with a single critical input focuses it when it opens.

## Storage

The console stores nothing except the Logto SDK session state in the browser (in a mock build, the fake session of the mock API). It never stores credentials in browser storage. The GitHub `state` and `code` stay only in the callback URL until the console sends them to `LinkGitHubInstallation`, and the console then removes them from the address bar.

## Security

- Credential inputs use password-type fields and are cleared after submission.
- Destructive actions (OwnSand link removal, GitHub installation unlink, integration deletion, profile deletion, runner deletion, dev session deletion, API key revocation, organization deletion, ownership transfer) need an explicit confirmation step.
- The console shows no runner output and no dev session output. It never stores an API key secret.

## Logging

The console sends no logs to a service in this version. Browser console messages never contain tokens or credentials.

## Build and Test

- Package scripts: `dev` (portless, started as `node node_modules/portless/dist/cli.js run --prefix=` through the shared `scripts/dev/with-infisical.mjs` with the Infisical folder `/apps/tide`, according to the [environment contract](repository-environment-contract.md#local-development-injection)), `dev:app` (`rsbuild dev`, which portless starts), `build` (`rsbuild build` into the ignored `dist/`), `preview`, `lint` and `typecheck` (`tsc --noEmit`), `test` (`rstest run` with `happy-dom`) and `test:e2e` (`e2e run`). They run through root Turborepo tasks.
- Unit tests cover the configuration rules, the router, the role matrix and the dev session delete rule, the runner profile form rules (`workspace_path` for `DEV_SESSION`, guest OS path syntax, the OpenAI Codex queue wait, the update mask, the default priority only on create), the queue view across a gap and a manual retry, the queue order, the setup steps, the GitHub callback parsing and address removal, the invitation link handling and the Logto auth bridge, and render the console against the mock API: role-based navigation and actions, the `INVALID` banner, credential forms with password fields that show only "Set at", the OpenAI Codex webhook URL, the `runs-on` label, the GitHub callback, a reconnect of the live overview, the setup guidance, confirmations of destructive actions, and a one-time API key secret that never reaches browser storage and closes with `Esc`.
- End-to-end suite: `apps/tide/e2e/` according to the [e2e contract](repository-e2e-contract.md), against the development server with `PUBLIC_TIDE_MOCK_API=true`; the app command runs `rsbuild dev --no-env`, so local `.env` files do not affect it. It does not reach a control plane, GitHub or Logto; acceptance against them is not covered.

## Dependencies and Integrations

- Upstream: `tide-api`, Logto, `delino-ui`, the generated TypeScript code (`@delino/tide-protos`) and the TypeScript SDK (`tide-sdk`).
- Dependencies: the exact pins of the [OwnSand console](apps-ownsand-console-foundation.md#dependencies-and-integrations) (`@connectrpc/connect` and `@connectrpc/connect-web` `2.3.0`, `@connectrpc/connect-query` `2.3.1`, `@tanstack/react-query` `5.106.1`, `@bufbuild/protobuf` `2.16.0`, `@logto/react` `4.1.3`, React `19.3.1`, and the same development tools). The console adds no new third-party dependency.
- Links to the OwnSand console and to `https://accounts.delino.io`.

## Change Triggers

- Update this contract with route, role or flow changes.
- Update the [protos contract](protos-tide-api-contract.md) first when the console needs a new RPC.
- Update `apps/AGENTS.md` only when development procedures, directory ownership or development rules change; follow the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-tide.md](project-tide.md)
- [servers-tide-api-foundation.md](servers-tide-api-foundation.md)
- [apps-ownsand-console-foundation.md](apps-ownsand-console-foundation.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-e2e-contract.md](repository-e2e-contract.md)
