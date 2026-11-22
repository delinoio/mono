# apps-tide-console foundation contract

## Scope

This contract owns the tide web console, component `tide-console`, of [`tide`](project-tide.md) at `apps/tide/` (`planned`). The console is served at `https://tide.delino.io`. It manages tide organizations, members, the OwnSand link, GitHub installations, agent integrations and runner profiles, and shows the queue, the runners and the device pool.

## Runtime and Language

- Rsbuild React single-page application in TypeScript, like the [OwnSand console](apps-ownsand-console-foundation.md).
- Deployment: static output on Cloudflare Pages through the [deployment workflow](repository-workflow-contract.md#deployment-workflow).
- Server state: React Query with `@connectrpc/connect-query` and the TypeScript code from the [protos contract](protos-tide-api-contract.md).
- UI components: [`delino-ui`](packages-delino-ui-foundation.md).
- Development port (`planned`): `127.0.0.1:13400`. Add it to the [environment contract](repository-environment-contract.md) with the implementation.

## Users and Operators

Organization owners, admins and members who sign in with Logto.

## Interfaces and Contracts

- The console calls the control plane only through its absolute origin. It reads the origin from `PUBLIC_API_ORIGIN` and uses `https://tide.api.delino.io` when the value is not set. It must not import source from `servers/`.
- Authentication: the console is a Logto single-page application that requests access tokens for the API resource `https://tide.api.delino.io`. Record the application registration and scopes with the implementation.
- Account settings: the console links to `https://accounts.delino.io`. It has no profile or account pages.
- The console never calls `ownsand-api`. Device management (enrollment, device limits including the vCPU overcommit, device removal) stays in the [OwnSand console](apps-ownsand-console-foundation.md), and the tide console links to it.

| Route ID        | Path                                 | Content                                                                                                                                               | Minimum role |
| --------------- | ------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------- | ------------ |
| `organizations` | `/`                                  | Organization list and creation; invitation acceptance entry.                                                                                          | signed in    |
| `invitation`    | `/invitations/accept`                | Accept an invitation secret as the signed-in user.                                                                                                    | signed in    |
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

- The `overview` route uses `WatchQueue` and reconnects when the stream ends.
- Empty states guide the first setup in this order: link OwnSand, connect GitHub or create an agent integration, create a runner profile. Each empty state names the next step.
- When the OwnSand link is `INVALID`, every organization route shows a banner that explains that dispatch has stopped and links to `integrations`.
- Credential forms accept a value once and never show it again. After a save, the form shows only that the credential is set and when.
- For an `OPENAI_CODEX` integration, the console shows the webhook URL to register in OpenAI.
- The `profiles` route requires `workspace_path` for `DEV_SESSION` profiles. The `dev-sessions` route shows no command, output or file content, and it does not run commands; commands and file transfers are available only through the CLI, the MCP server and the SDKs.
- The `api-keys` route shows a new secret once with a copy action and a notice that tide cannot show it again, and explains that the key works only for dev sessions.
- Organization deletion tells the owner to uninstall the GitHub App in GitHub, because tide cannot uninstall it.
- The console hides actions that the user's role does not allow; the control plane still enforces every permission.
- UX follows the Toss Design Guidelines. Dialogs close with `Esc`. A form with a single critical input focuses it when it opens.

## Storage

The console stores nothing except the Logto SDK session state in the browser. It never stores credentials in browser storage. The GitHub `state` and `code` stay only in the callback URL until the console sends them to `LinkGitHubInstallation`, and the console then removes them from the address bar.

## Security

- Credential inputs use password-type fields and are cleared after submission.
- Destructive actions (OwnSand link removal, GitHub installation unlink, integration deletion, profile deletion, runner deletion, dev session deletion, API key revocation, organization deletion, ownership transfer) need an explicit confirmation step.
- The console shows no runner output and no dev session output. It never stores an API key secret.

## Logging

The console sends no logs to a service in this version. Browser console messages never contain tokens or credentials.

## Build and Test

- Package scripts `dev`, `build`, `lint`, `typecheck` and `test` run through root Turborepo tasks. Record them with the implementation.
- When the console has its first route, extend the scope of the [e2e contract](repository-e2e-contract.md) to `tide-console` and add an end-to-end suite according to it.

## Dependencies and Integrations

- Upstream: `tide-api`, Logto, `delino-ui` and the generated TypeScript code.
- Links to the OwnSand console and to `https://accounts.delino.io`.

## Change Triggers

- Update this contract with route, role or flow changes.
- Update the [protos contract](protos-tide-api-contract.md) first when the console needs a new RPC.
- Update `apps/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-tide.md](project-tide.md)
- [servers-tide-api-foundation.md](servers-tide-api-foundation.md)
- [apps-ownsand-console-foundation.md](apps-ownsand-console-foundation.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-e2e-contract.md](repository-e2e-contract.md)
