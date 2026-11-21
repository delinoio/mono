# Repository Environment Contract

## Configuration ownership

The public repository has no environment selectors, secret provider integration or production credentials. Each service defines its configuration schema, validation, local defaults and credential boundaries in its domain contract. The [`accounts-api` contract](servers-accounts-api-foundation.md) owns the API server configuration, and the [`delino-accounts` web app contract](apps-delino-accounts-foundation.md) owns the web app build configuration.

Servers under `servers/` read their runtime configuration from environment variables without a project or service prefix, for example `DATABASE_URL`, `LOGTO_ISSUER` and `CORS_ORIGINS`, not `OWNSAND_DATABASE_URL`. Each server runs as its own process with its own environment, so a prefix adds no isolation. This rule applies to the configuration that a deployed server reads. It does not apply to command-line tools, device-side programs, SDKs or web app builds, which share an environment with other programs, or to the variables that opt in to tests.

## Local state

Never commit `.infisical.json`, `.dev-environment/`, real `.env` files or user, production, release and signing credentials. Service-local `.env.example` files may contain names, validation guidance, placeholders and safe loopback defaults only. Do not add a root environment example.

## Process boundaries

Use explicit argument arrays and `shell: false` for child processes. Document any required platform shim. Forward termination signals and wait for owned child processes to exit. Documentation servers must declare a fixed loopback port and fail on conflicts. Application development servers follow the same rule: `delino-docs` uses `127.0.0.1:13500`, `delino-site` uses `127.0.0.1:13600`, `delino-accounts` uses `127.0.0.1:13100`, `accounts-api` uses `127.0.0.1:18100`, `ownsand-api` uses `127.0.0.1:18300` for HTTP and `127.0.0.1:13478` for STUN over UDP, the `delidev` frontend uses `127.0.0.1:13200` and the OwnSand web console (`ownsand-console`) uses `127.0.0.1:13300` by default. The `delidev` frontend binds to the address in `TAURI_DEV_HOST` instead of the loopback address when the Tauri CLI sets it for a physical Android or iOS device, according to the [delidev contract](apps-delidev-foundation.md#build-and-test). End-to-end suites start their own development server on a free loopback port, according to the [e2e contract](repository-e2e-contract.md#app-under-test). The fixed ports of the four frontend apps in [Frontend development URLs](#frontend-development-urls) apply to their `dev:app` scripts and to `preview`.

### Frontend development URLs

The `dev` script of `delino-accounts`, `ownsand-console`, `delino-docs` and `delino-site` runs [portless](https://github.com/vercel-labs/portless), which each app pins as the devDependency `portless` `0.18.1`. portless starts the app's `dev:app` script, which holds the real development server command, on a free loopback port from 4000 to 4999. It passes that port in `PORT`, sets `HOST=127.0.0.1` and `PORTLESS_URL`, and serves the app through a local reverse proxy at a stable name:

| App               | portless name     | URL with the default proxy          | `dev:app` and `PORTLESS=0` |
| ----------------- | ----------------- | ----------------------------------- | -------------------------- |
| `delino-accounts` | `accounts.delino` | `https://accounts.delino.localhost` | `http://127.0.0.1:13100`   |
| `ownsand-console` | `ownsand.delino`  | `https://ownsand.delino.localhost`  | `http://127.0.0.1:13300`   |
| `delino-docs`     | `docs.delino`     | `https://docs.delino.localhost`     | `http://127.0.0.1:13500`   |
| `delino-site`     | `delino`          | `https://delino.localhost`          | `http://127.0.0.1:13600`   |

- Each app's `package.json` sets the name and the script in its `portless` key. Do not use a root `portless.json` or a root `portless` script: root `pnpm dev` stays `turbo run dev`, and Turbo runs `portless` in each app package.
- In a linked Git worktree, portless adds the last `/` segment of the branch to the name as a label, for example `https://fix-ui.docs.delino.localhost`. The same app can then run in several worktrees at the same time. `portless get <name>` in the app directory prints the URL of the current checkout.
- The development server still binds only to `127.0.0.1` and keeps `strictPort`. portless adds `--port` and `--host` flags to `rsbuild dev`. Rspress gets no flags, so `rspress.config.ts` reads `PORT` only when `PORTLESS_URL` is also set; a `PORT` that the shell exports does not move the fixed port of `dev:app` or `preview`.
- `PORTLESS=0` makes `dev` run `dev:app` directly on the fixed port. The `dev` task of each of these apps passes `PORTLESS` and `PORTLESS_*` through Turbo strict mode and no `PORT`, so portless settings from the shell reach portless also through root `pnpm dev`.
- The proxy and its state (`~/.portless` by default) belong to the developer's machine, not to the repository. With the default proxy (HTTPS with HTTP/2 on port 443), the first start creates a local certificate authority, adds it to the system trust store and can ask for `sudo`. Start it once from an interactive terminal before root `pnpm dev`, for example with `pnpm --filter delino-docs exec portless proxy start`, because a task without a terminal cannot answer the prompt. `PORTLESS_UNPRIVILEGED=1` selects a proxy that never asks for `sudo` and serves plain HTTP (on Linux it needs `net.ipv4.ip_unprivileged_port_start` at or below the proxy port, or a `PORTLESS_PORT` of 1024 or more). portless refuses a state directory that a group or other users can write to, or that is under such a directory.
- Termination: portless runs `dev:app` in its own process group, forwards SIGINT, SIGTERM and SIGHUP to it and waits for it to exit. Ctrl-C on root `pnpm dev` or on `pnpm --filter <app> dev` stops the development server. Known limitation: when Turbo itself receives SIGTERM, which happens on Ctrl-C of `pnpm exec turbo run dev`, Turbo sends SIGKILL to the task process group immediately after SIGINT. portless is then stopped before it forwards the signal, and the development server keeps running outside the task group. Run `pnpm --filter delino-docs exec portless prune` (any of these apps works) to stop it.
- `delidev` does not use portless: the Tauri `build.devUrl` and physical Android and iOS devices (`TAURI_DEV_HOST`) need its fixed address. The server packages do not use portless either: `ownsand-api` needs HTTP/2 without TLS (h2c) and UDP for STUN, and the portless proxy connects to backends over HTTP/1.1 and serves HTTP only.
