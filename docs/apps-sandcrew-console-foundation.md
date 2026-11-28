# apps-sandcrew-console foundation contract

## Scope

This contract owns the SandCrew web console, component `sandcrew-console`, of [`sandcrew`](project-sandcrew.md) at `apps/sandcrew/` (`planned`, workspace package `sandcrew-console`). The console is served at `https://sandcrew.delino.io`. Members of SandRun organizations use it to create agent sessions, chat with the agent, and manage their vendor credentials, session secrets and notifications; `owner` and `admin` members also configure SandCrew for the organization.

## Runtime and Language

- Rsbuild React single-page application in TypeScript, like the [SandRun console](apps-sandrun-console-foundation.md).
- Deployment: static output on Cloudflare Pages through the [deployment workflow](repository-workflow-contract.md#deployment-workflow). The `deploy` field of `apps/sandcrew/package.json` names the Pages project `sandcrew-console`, and its `buildEnv` maps `PUBLIC_LOGTO_ENDPOINT` to `LOGTO_ENDPOINT` and `PUBLIC_LOGTO_APP_ID` to `SANDCREW_CONSOLE_LOGTO_APP_ID`.
- Server state: React Query with `@connectrpc/connect-query` and the TypeScript bindings of `@delino/sandcrew-protos`.
- UI components: [`delino-ui`](packages-delino-ui-foundation.md) with light and dark themes.
- Service worker: one service worker at the site root, only for Web Push. It caches nothing.
- Development port (recorded when the path is created): `127.0.0.1:13700` for `dev:app`, behind the portless name `sandcrew.delino` (`https://sandcrew.delino.localhost`).

## Users and Operators

Members of SandRun organizations who sign in with Logto. `owner` and `admin` members also see the organization settings.

## Interfaces and Contracts

### Routes

| Route ID            | Path                                          | Content                                                                                                                        |
| ------------------- | --------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `ORGANIZATION_PICK` | `/`                                           | Select one of the user's SandRun organizations.                                                                                |
| `SESSION_LIST`      | `/orgs/:organizationId/sessions`              | Session list.                                                                                                                  |
| `SESSION_NEW`       | `/orgs/:organizationId/sessions/new`          | New-session form.                                                                                                              |
| `SESSION_CHAT`      | `/orgs/:organizationId/sessions/:sessionId`   | Chat view (owner) or metadata view (other members).                                                                            |
| `SETTINGS_USER`     | `/orgs/:organizationId/settings`              | The user's vendor credentials, session secrets and Web Push opt-in.                                                            |
| `SETTINGS_ORG`      | `/orgs/:organizationId/settings/organization` | Organization vendor credentials, allowed runner profiles, pause delay and paused-session retention (`owner` and `admin` only). |
| `SIGN_IN_CALLBACK`  | `/callback`                                   | Logto sign-in callback.                                                                                                        |

- The organization picker lists the organizations from `OrganizationService.ListOrganizations`. It links to the SandRun console (`https://sandrun.delino.io`) to create organizations and manage members. SandCrew has no member pages. Account management links to `https://accounts.delino.io`.

### Session list

- Lists sessions with their state (queued, starting, running, waiting for input, pausing, paused, resume queued, resuming, disconnected, or ended with its reason), vendor, label, owner and times. It updates live with `WatchSessions`.
- Filters: "My sessions" (default) and "All sessions". Sessions of other members show metadata only and open the metadata view.
- A paused session shows its scheduled deletion time when the retention is limited.

### New-session form

- Fields: vendor, runner profile (only the allowed and selectable profiles), first message (the single critical input, focused when the form opens), optional label, session secrets (none preselected), and the model and mode when `GetAgentOptions` returns options for the vendor.
- Without a vendor credential for the selected vendor (user or organization), the form explains it and links to the settings.
- When `CreateSession` fails with `resource_exhausted` for the SandRun session limit, the form shows the server message: delete a session or ask an administrator.
- When no runner profile is allowed, the form tells members to ask an administrator and shows `owner` and `admin` a link to the organization settings.

### Chat view

- Streams the transcript with `WatchSession` from the last sequence it has, and loads earlier events with `ListSessionEvents`. It reconnects when the stream ends and keeps the stream open only while the page is visible (the Page Visibility API), so that the server can suppress notifications for a visible session.
- Shows user and agent messages (Markdown), agent thoughts (collapsed), tool calls with their status, collapsible input and output, plans, diffs, permission requests with one button per option, permission answers and auto-approved records (marked as auto-approved), turn ends with their stop reason, bridge errors and unknown updates (collapsed, with their raw JSON).
- Composer: sends messages with `SendMessage`. While a turn runs, a "Stop" button calls `CancelTurn`. A permission button calls `AnswerPermission`.
- Mode and model: a menu with the modes and configuration options that the agent advertised, and the switch between auto-approve and ask mode (`SetSessionMode`, `SetSessionConfigOption`).
- Session state: a banner for `PAUSED` (with the scheduled deletion time), `RESUME_QUEUED`, `RESUMING` and `DISCONNECTED`. Sending a message or a permission answer to a paused session resumes it. When a resume waited too long, the banner offers "Resume again" (`ResumeSession`).
- Ended sessions: the transcript stays readable, the composer is replaced by the end reason and the action "Start new session from transcript" (`RestartSessionFromTranscript`, with a first message). A start failure caused by an agent download failure explains that the session could not install the agent.
- Delete: "Delete session" opens a confirmation dialog that says that the transcript is deleted permanently.

### Settings

- User settings: vendor credentials per vendor (API key, Claude Code token from `claude setup-token`, or the Codex `auth.json` file), session secrets (name and value), and Web Push. Values are write-only: the page shows only kinds, names and times.
- Web Push: a "Turn on notifications on this browser" button. The browser permission prompt appears only after the click. The console registers the service worker, subscribes with the VAPID public key of `GetWebPushConfig` and calls `RegisterPushSubscription`. The page lists the user's subscriptions and can delete them.
- Organization settings (`owner` and `admin`): organization vendor credentials, allowed runner profiles (profiles that are not selectable are shown disabled with their reason), pause delay and paused-session retention (with "Unlimited").
- Notification click: opens the chat view of the session.

### States and accessibility

- Every view has empty, loading, error and permission states. A `not_found` for an organization that the user no longer belongs to returns to the organization picker.
- English only. The layout works down to 320 px wide.
- Follows the Toss Design Guidelines. Dialogs close with `Esc`. A form with a single critical input focuses it.

## Storage

- Logto session storage by the Logto browser SDK. The selected organization and the "My sessions" filter in `localStorage`, wrapped in error handling; the console works without them.
- No transcript, credential or secret is stored in the browser beyond the React Query memory cache.

## Security

- The console sends only the user's Logto access token for `https://sandcrew.api.delino.io` (scope `sandcrew:use`) and never receives credential or secret values.
- Agent output is rendered as untrusted content: Markdown is rendered without raw HTML, links open with `rel="noopener noreferrer"`, and raw JSON is shown as text.
- No PostHog autocapture and no session recording. Product analytics are server-side only.

## Logging

The console sends no logs. Errors appear in the user interface with the Connect error code; no prompt or output reaches a third-party service.

## Build and Test

- `pnpm exec turbo run build lint typecheck test --filter=sandcrew-console` and `pnpm test` from the repository root, with unit tests for the session list, the new-session form, the chat view (every event kind, permission buttons, cancel, mode switch), the settings and the Web Push opt-in.
- Browser end-to-end tests in `apps/sandcrew/e2e/` according to the [e2e contract](repository-e2e-contract.md), against a fake `sandcrew-api`: the session list, the new-session form, the chat view, the settings, and the empty, loading, error and permission states.

## Dependencies and Integrations

- Upstream: [`sandcrew-api`](servers-sandcrew-api-foundation.md), Logto, the browser Push API.
- Links: the SandRun console for organizations and members, `https://accounts.delino.io` for account management.
- New dependencies (for example, a Markdown renderer) need owner approval and a [dependency security](repository-dependency-security-contract.md) review in the change that adds them.

## Change Triggers

- Update the [protos contract](protos-sandcrew-api-contract.md) first when the console needs an RPC change.
- Update the [project index](project-sandcrew.md) when routes, the origin or the deployment change.

## References

- [project-sandcrew.md](project-sandcrew.md)
- [apps-sandrun-console-foundation.md](apps-sandrun-console-foundation.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-e2e-contract.md](repository-e2e-contract.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
