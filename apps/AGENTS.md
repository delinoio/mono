# apps working instructions

- Follow the root instruction-update policy and the nearest parent instructions.
- This file covers `apps/` and its descendants unless a more specific instruction file applies.
- Read the owning contracts below before changing behavior, including affected cross-domain consumers.

## Development and validation

- Follow root `AGENTS.md` and project-specific docs before adding or changing app code.

- When you change an app with an e2e suite, run `pnpm exec turbo run test:e2e --filter=./apps/<app>` and update its tests in `e2e/`. Follow the [e2e contract](../docs/repository-e2e-contract.md).

- Each deployed static site declares a `deploy` field in its `package.json`. Follow [the deployment workflow](../docs/repository-workflow-contract.md#deployment-workflow).

- The web app development servers of `delino-accounts`, `ownsand-console`, `delino-docs` and `delino-site` run through portless: the `dev` script runs `portless run --prefix=` (behind `scripts/dev/with-infisical.mjs` for `delino-accounts` and `ownsand-console`), so the URL does not depend on the checkout, the real server command is the `dev:app` script, and the app's Turbo `dev` task passes `PORTLESS` and `PORTLESS_*` through. The `delidev` frontend `dev` script runs `rsbuild dev` directly. Follow [Frontend development URLs](../docs/repository-environment-contract.md#frontend-development-urls).

## Owning contracts

- [Repository defaults](../docs/repository-defaults.md)
- [Repository workflow](../docs/repository-workflow-contract.md)
- [Authentication](../docs/repository-auth-contract.md)
- [Billing currency and precision](../docs/repository-billing-contract.md)
- [End-to-end testing](../docs/repository-e2e-contract.md)
- `apps/delino-site/`: [project index](../docs/project-delino-site.md) and [foundation contract](../docs/apps-delino-site-foundation.md)
- `apps/delino-docs/`: [project index](../docs/project-delino-docs.md) and [foundation contract](../docs/apps-delino-docs-foundation.md)
- `apps/delino-accounts/`: [project index](../docs/project-delino-accounts.md) and [web app foundation contract](../docs/apps-delino-accounts-foundation.md)
- `apps/delidev/`: [project index](../docs/project-delidev.md) and [foundation contract](../docs/apps-delidev-foundation.md)
- `apps/ownsand/`: [project index](../docs/project-ownsand.md) and [web console foundation contract](../docs/apps-ownsand-console-foundation.md)
- `apps/tide/` (`planned`): [project index](../docs/project-tide.md) and [web console foundation contract](../docs/apps-tide-console-foundation.md)
- Register each new project and its domain contracts in [the documentation catalog](../docs/README.md) before implementation.
