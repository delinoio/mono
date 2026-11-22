# Repository Defaults

## Purpose

This document defines default technology choices and repository workflow defaults for new repository work when a more specific project or domain contract does not already choose a different approach.

## License

Repository-owned source, package metadata and future release artifacts use Apache-2.0. Backend services under `servers/`, the OwnSand device agent (`cmds/ownsand-agent/`) and the OwnSand guest helper (`cmds/ownsand-guest/`) use FSL-1.1-ALv2. Preserve the original licenses and copyright notices of imported code and bundled fonts. See `docs/repository-license-contract.md`.

## Default Technology Choices

- New persisted entities should use UUID v7 identifiers by default. Use another identifier shape only when there is a documented compatibility, storage, protocol, or product reason.
- AI-based search should use Cloudflare AI Search by default. Use another search backend only when the project contract documents the reason and migration boundary.
- When a new project does not specify its primary language, default to Golang.
- Build tooling should prefer the Rspack family when it fits the runtime and deployment target, including Rsbuild and Rspress for app and documentation surfaces.
- Static sites under `apps/` should use Rsbuild/Rspress-style toolchains and deploy to Cloudflare Pages by default. Any exception must be documented in its project contract.
- Backend services under `servers/` should deploy to the shared DigitalOcean Droplet by default, as containers behind its Caddy reverse proxy. Any exception must be documented in its project contract.
- Persistent relational databases should use PlanetScale Postgres by default. Any exception must be documented in its project contract.
- File handling should use Cloudflare R2 for object storage plus signed URLs for upload and download access by default. Use another storage or access pattern only when the project contract documents the reason, trust boundary, and migration considerations.
- Product analytics should use PostHog Cloud by default. Use another product analytics service only when the project contract documents the reason.
- Observability data (metrics, logs and traces), dashboards and alerts should use Grafana Cloud by default. Use another observability backend only when the project contract documents the reason.

## Repository-Wide Rules

These rules are not defaults; project and domain contracts must not deviate from them.

- Authentication uses one shared Logto tenant for all services. A project may accept another credential for programs, devices or short-lived capabilities that it issues after validating another accepted credential only when the documented exceptions in `docs/repository-auth-contract.md` list it. See `docs/repository-auth-contract.md`.
- User charges use USD with 0.000001 USD precision. See `docs/repository-billing-contract.md`.
- Account management and billing for every service use `delino-accounts`. See `docs/repository-auth-contract.md`, `docs/repository-billing-contract.md` and `docs/project-delino-accounts.md`.
- Metrics, logs and traces are instrumented with the OpenTelemetry SDK. Go code keeps `log/slog` and Rust code keeps `tracing` as the logging API, and sends the log records to the OpenTelemetry SDK through its log bridge. The observability backend that receives this data is a default; see [Default Technology Choices](#default-technology-choices).

## Documentation Requirements

- Project index docs must record deviations from these defaults in `Cross-Domain Invariants` or `Change Policy`.
- Domain contract docs must record deviations in the relevant `Runtime and Language`, `Storage`, `Security`, `Build and Test`, or `Dependencies and Integrations` sections.
- Repository and domain `AGENTS.md` files must stay aligned with this document when these defaults change.

Instruction-file updates in this requirement apply only to changes in development procedures, directory ownership or repository/domain development rules under the [instruction-update policy](README.md#instruction-update-policy); ordinary behavior and validation changes update the owning contracts and validation records.

## Repository Workflow Defaults

- Newly created pull requests must use Conventional Commit-style titles with a required scope: `<type>(<scope>): <description>`.
- Pull request title scopes should use stable lowercase project, component, domain, or tooling identifiers from repository contracts when one applies.
- Pull request titles must not omit the scope and must not use bracket-style project prefixes.
- Root `pnpm install` must support linked worktrees when the effective Git hooks path resolves to the repository's shared common-directory hooks directory, without overriding an unrelated custom hooks path. Source archives and other workspaces without Git metadata skip hook installation without running project preparation.
- The registered projects are the static sites `delino-site` and `delino-docs`, the account and billing project `delino-accounts`, the shared React UI component package `delino-ui`, the desktop and mobile app `delidev`, the device sandbox cloud `ownsand`, which is in implementation, and the CI and agent runner service `tide`, which is in its documentation-only phase. The public documentation application is `delino-docs`. `.github/workflows/deploy.yml` deploys the servers and static sites selected by pending changesets; see `docs/repository-workflow-contract.md`. New projects must define their own development, validation and publication contracts before implementation.

## References

- `docs/README.md`
- `docs/repository-auth-contract.md`
- `docs/repository-billing-contract.md`
- `docs/project-template.md`
- `docs/domain-template.md`
- `AGENTS.md`
- `apps/AGENTS.md`
