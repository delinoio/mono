# apps-sandcrew-console foundation contract

## Scope

This contract owns the SandCrew web console, component `sandcrew-console`, of [`sandcrew`](project-sandcrew.md) at `apps/sandcrew/` (workspace package `sandcrew-console`). The console is served at `https://sandcrew.delino.io`. Members of SandRun organizations use it to create agent sessions, chat with the agent, and manage their vendor credentials, session secrets and notifications; `owner` and `admin` members also configure SandCrew for the organization.

## Runtime and Language

- Rsbuild React single-page application in TypeScript, like the [SandRun console](apps-sandrun-console-foundation.md).
- Deployment: static output on Cloudflare Pages through the [deployment workflow](repository-workflow-contract.md#deployment-workflow). The `deploy` field of `apps/sandcrew/package.json` names the Pages project `sandcrew-console`, and its `buildEnv` maps `PUBLIC_LOGTO_ENDPOINT` to `LOGTO_ENDPOINT` and `PUBLIC_LOGTO_APP_ID` to `SANDCREW_CONSOLE_LOGTO_APP_ID`.
- Server state: React Query with `@connectrpc/connect-query` and the TypeScript bindings of `@delino/sandcrew-protos`.
- UI components: [`delino-ui`](packages-delino-ui-foundation.md) with light and dark themes.
- Service worker: one service worker at the site root, only for Web Push. It caches nothing.
- Development port: `127.0.0.1:13700` for `dev:app`, behind the portless name `sandcrew.delino` (`https://sandcrew.delino.localhost`), according to the [environment contract](repository-environment-contract.md#frontend-development-urls).
- The console calls the control plane only through its absolute origin. It reads the origin from `PUBLIC_API_ORIGIN` and uses `https://sandcrew.api.delino.io` when the value is not set.
- Authentication: a Logto single-page application (`@logto/react`) with the API resource `https://sandcrew.api.delino.io` and the scope `sandcrew:use`, which the console always requests. The auth bridge, the sign-in callback, the unreadable sign-in session guard and the `Unauthenticated` recovery follow the [SandRun console](apps-sandrun-console-foundation.md#interfaces-and-contracts).
- Build-time variables (all public, listed in `apps/sandcrew/.env.example`): `PUBLIC_API_ORIGIN`, `PUBLIC_LOGTO_ENDPOINT`, `PUBLIC_LOGTO_APP_ID`, `PUBLIC_LOGTO_SCOPES` and `PUBLIC_SANDCREW_MOCK_API` (`true` or `false`). `rsbuild.config.ts` validates them with the origin rules of the SandRun console and fails the build or development server on an invalid value. The Turbo `dev` task passes them through `passThroughEnv`, and the `build` task declares them as `env`.
- Mock API (recorded decision): `PUBLIC_SANDCREW_MOCK_API=true` runs the console against an in-browser fake control plane (`src/mock/mock-api.ts`, a Connect router transport) and a fake signed-in user whose session lives in `sessionStorage`. It seeds an organization of each role (an `owner` organization with sessions in every state, an `admin` organization at the SandRun session limit and a `member` organization without an allowed runner profile), runner profiles with each `ProfileIneligibility`, a profile ID that SandRun deleted, credentials and a session secret, and a transcript with every event kind. It simulates agent turns, permission requests, pause and resume, and the streams. It applies the [authorization rules](servers-sandcrew-api-foundation.md#authorization) and the ranges of the control plane. Production builds reject the flag, and the minifier removes the separate mock module from builds with the flag disabled. The mock serves the development server, the unit tests and the end-to-end suite.

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

- The organization picker lists the organizations from `OrganizationService.ListOrganizations`, the last used organization first. It links to the SandRun console (`https://sandrun.delino.io`) to create organizations and manage members. SandCrew has no member pages. Account management links to `https://accounts.delino.io`.

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
- Web Push: a "Turn on notifications on this browser" button. The browser permission prompt appears only after the click. The console registers the service worker, subscribes with the VAPID public key of `GetWebPushConfig` and calls `RegisterPushSubscription` with a browser name such as "Firefox on Linux" as the label. The page lists the user's subscriptions and can delete them. A browser without the Push API shows that it does not support notifications, and a browser that blocks notifications for the site shows how to allow them instead of the button.
- Organization settings (`owner` and `admin`): organization vendor credentials, allowed runner profiles (profiles that are not selectable are shown disabled with their reason), pause delay and paused-session retention (with "Unlimited").
- Notification click: opens the chat view of the session from the `organization_id` and `session_id` of the [push payload](servers-sandcrew-api-foundation.md#notifications), or focuses a window that shows it.

### States and accessibility

- Every view has empty, loading, error and permission states. A `not_found` for an organization that the user no longer belongs to returns to the organization picker, which explains that the user no longer has access. An organization route whose organization is not in the caller's list does the same.
- English only. The layout works down to 320 px wide.
- Follows the Toss Design Guidelines. Dialogs close with `Esc`. A form with a single critical input focuses it.

## Storage

- Logto session storage by the Logto browser SDK. The selected organization and the "My sessions" filter in `localStorage`, wrapped in error handling; the console works without them.
- No transcript, credential or secret is stored in the browser beyond the React Query memory cache.

## Security

- The console sends only the user's Logto access token for `https://sandcrew.api.delino.io` (scope `sandcrew:use`) and never receives credential or secret values.
- Agent output is rendered as untrusted content: Markdown is rendered without raw HTML, links open with `rel="noopener noreferrer"`, and raw JSON is shown as text.
- Markdown renderer (recorded decision): the console renders a subset of Markdown itself (`src/markdown.tsx`): paragraphs, headings, fenced code blocks, bullet and numbered lists, block quotes, inline code, bold, italic and links. It builds React elements only and allows only `http`, `https` and `mailto` links; other syntax shows as text. A CommonMark library is a new dependency that needs owner approval and a dependency security review; replace the renderer in the change that adds one.
- Credential and secret forms use password fields (and a file input for the Codex `auth.json`), clear their values after a save and never show a value again. The console checks the `auth.json` file (a JSON object of at most 64 KiB) and the session secret name rules of the control plane before it sends them.
- No PostHog autocapture and no session recording. Product analytics are server-side only.

## Logging

The console sends no logs. Errors appear in the user interface with the Connect error code; no prompt or output reaches a third-party service.

## Build and Test

- `pnpm exec turbo run build lint typecheck test --filter=sandcrew-console` and `pnpm test` from the repository root, with unit tests for the session list, the new-session form, the chat view (every event kind, permission buttons, cancel, mode switch), the settings and the Web Push opt-in.
- Package scripts: `dev` (portless, started as `node node_modules/portless/dist/cli.js run --prefix=` through `scripts/dev/with-infisical.mjs` with the Infisical folder `/apps/sandcrew`), `dev:app` (`rsbuild dev`), `build`, `preview`, `lint` and `typecheck` (`tsc --noEmit`), `test` (`rstest run` with `happy-dom`) and `test:e2e` (`e2e run`).
- Browser end-to-end tests in `apps/sandcrew/e2e/` according to the [e2e contract](repository-e2e-contract.md), against the development server with the mock API: the session list, the new-session form, the chat view, the settings, the Web Push opt-in, and the empty, loading, error and permission states. For the loading states, a test stores a response delay for the mock in `sessionStorage` (`sandcrew.mock-response-delay-ms`), which only mock builds read. The app command uses Node to run the Rsbuild CLI with `dev --no-env --mode development`, `NODE_ENV=production` and a padded `PUBLIC_SANDCREW_MOCK_API=true` value. Headless Chromium blocks notifications and has no push service, so the opt-in test replaces only the permission prompt and `PushManager.subscribe` in the page; the service worker registration is real. The suite does not test a deployed control plane, Logto or a push service.

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
