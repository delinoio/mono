# Repository Environment Contract

## Configuration ownership

The public repository has no environment selectors, secret provider integration or production credentials. Each service defines its configuration schema, validation, local defaults and credential boundaries in its domain contract. The [`accounts-api` contract](servers-accounts-api-foundation.md) owns the API server configuration, and the [`delino-accounts` web app contract](apps-delino-accounts-foundation.md) owns the web app build configuration.

Servers under `servers/` read their runtime configuration from environment variables without a project or service prefix, for example `DATABASE_URL`, `LOGTO_ISSUER` and `CORS_ORIGINS`, not `OWNSAND_DATABASE_URL`. Each server runs as its own process with its own environment, so a prefix adds no isolation. This rule applies to the configuration that a deployed server reads. It does not apply to command-line tools, device-side programs, SDKs or web app builds, which share an environment with other programs, or to the variables that opt in to tests.

## Local state

Never commit `.infisical.json`, `.dev-environment/`, real `.env` files or user, production, release and signing credentials. Service-local `.env.example` files may contain names, validation guidance, placeholders and safe loopback defaults only. Do not add a root environment example.

## Process boundaries

Use explicit argument arrays and `shell: false` for child processes. Document any required platform shim. Forward termination signals and wait for owned child processes to exit. Documentation servers must declare a fixed loopback port and fail on conflicts. Application development servers follow the same rule: `delino-accounts` uses `127.0.0.1:3100`, `accounts-api` uses `127.0.0.1:8100`, `ownsand-api` uses `127.0.0.1:8300` for HTTP and `127.0.0.1:3478` for STUN over UDP, the `delidev` frontend uses `127.0.0.1:3200` and the OwnSand web console (`ownsand-console`) uses `127.0.0.1:3300` by default. The `delidev` frontend binds to the address in `TAURI_DEV_HOST` instead of the loopback address when the Tauri CLI sets it for a physical Android or iOS device, according to the [delidev contract](apps-delidev-foundation.md#build-and-test). End-to-end suites start their own development server on a free loopback port, according to the [e2e contract](repository-e2e-contract.md#app-under-test).
