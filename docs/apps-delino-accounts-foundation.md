# apps-delino-accounts foundation contract

## Scope

This contract owns the `delino-accounts-web` component of [`delino-accounts`](project-delino-accounts.md) at `apps/delino-accounts/`:

- `apps/delino-accounts/rsbuild.config.ts`: build and development server configuration.
- `apps/delino-accounts/index.html`: HTML document template that sets `<html lang="en">`.
- `apps/delino-accounts/src/`: application source, entry point and unit tests.
- `apps/delino-accounts/public/`: static files copied to the build root, including the Cloudflare Pages `_headers` file. The directory must not contain a top-level `404.html` or a `_redirects` file with a rule that rewrites to `/index.html`; `src/static-hosting.test.ts` enforces this.
- `apps/delino-accounts/turbo.json`: package-local Turbo task configuration.
- `apps/delino-accounts/e2e.config.ts` and `apps/delino-accounts/e2e/`: end-to-end test config and tests, according to the [e2e contract](repository-e2e-contract.md).

## Runtime and Language

- Toolchain: Rsbuild `@rsbuild/core` 2.x with `@rsbuild/plugin-react`, React 19.
- Language: TypeScript.
- Test runner: Rstest `@rstest/core`.
- Runtime: Node.js from root `.nvmrc` for builds and tests. Production output is a static single-page application with no server runtime.
- Deployment target: Cloudflare Pages, production domain `accounts.delino.io`.
- Deviation from repository defaults: the app uses Rsbuild instead of Rspress because it is an authenticated application. See the [project index](project-delino-accounts.md).

## Users and Operators

- Signed-in Delino users who manage their account and billing information.
- Maintainers who develop and deploy the web app.

## Interfaces and Contracts

- Production origin: `https://accounts.delino.io`. The asset prefix is `/`.
- Content language: English (`lang="en"`).

| Route ID  | Path       | Status    | Purpose                                                            |
| --------- | ---------- | --------- | ------------------------------------------------------------------ |
| `home`    | `/`        | available | Entry page that introduces the account and billing sections.       |
| `account` | `/account` | planned   | Profile and preferences attached to the Logto user.                |
| `billing` | `/billing` | planned   | Payment methods, billing contacts, balances, credits and invoices. |

- Client-side routes load on direct navigation through the Cloudflare Pages single-page-application fallback: when a project has no top-level `404.html`, Pages assumes a single-page application and serves the root (`/`, the `index.html` document) for any path that does not correspond to a static asset. The fallback applies only while the build output has no top-level `404.html`, so the app must not emit a `404.html`. The app must not add a `_redirects` rule that rewrites to `/index.html` (for example `/* /index.html 200`): Cloudflare rejects it as an infinite loop and ignores it, so it provides no routing. Unknown paths return status `200`, so the app renders its own not-found view once routes exist.
- `public/_headers` sets Cloudflare Pages response headers: `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin` and `X-Frame-Options: DENY`.
- API origin: `src/config.ts` resolves the API server origin from the build-time variable `PUBLIC_API_ORIGIN`. An unset value, an empty value and a value made only of tab, line feed, vertical tab, form feed, carriage return and space characters select `https://accounts.api.delino.io`. Unicode white space such as U+00A0, U+FEFF and U+3000 is not ignored and fails the build as an invalid URL, also when the value consists only of such characters. A value made only of other ignored characters, for example U+0001, is not empty and also fails as an invalid URL. The value must be an `http` or `https` origin with no credentials (user name or password), path, query or fragment, including an empty `?` or `#`; the build fails for other values. As the URL parser does, the check ignores leading and trailing C0 control characters (U+0000 to U+001F) and spaces, and tab, carriage return and line feed anywhere in the value, so a value such as `\u0001https://accounts.api.delino.io` resolves to the same origin that the parser reads. Error messages never echo the raw value because it may carry a credential. `readAccountsApiOrigin` is the only reader of the variable: `rsbuild.config.ts` calls it with `process.env` to fail the build and the dev server early, and `src/api-origin.ts` calls it with `import.meta.env` to export the validated `ACCOUNTS_API_ORIGIN` constant, which Rsbuild inlines into the client bundle. `src/index.tsx` sets the `data-api-origin` attribute of `<html>` to that constant so the build-time value is observable in the running app; it adds no visible markup, request or route.
- API calls (`planned`): Connect RPC through `@connectrpc/connect-query` and React Query. The app must not call the API through Tauri-specific or ad-hoc bindings.
- Authentication (`planned`): the app is registered as a Logto single-page application in the shared tenant and requests access tokens for the API resource `https://accounts.api.delino.io`. Sign-in redirect URIs use the `https://accounts.delino.io` origin in production and the loopback development origin locally.
- Monetary values from the API are `bigint` micro-USD amounts. The app formats them for display only and never converts them to `number` for arithmetic.

## Storage

The app has no server-side storage. All account and billing state belongs to the [API server](servers-accounts-api-foundation.md). Browser storage is limited to the Logto SDK session data and per-user display preferences. Static assets of at least 512 KiB require Git LFS according to the repository workflow contract.

## Security

- Do not embed secrets in the build. Every `PUBLIC_` variable is public and is inlined into the client bundle.
- Logto application IDs and the tenant endpoint are configuration, not secrets. The app must not use a Logto application secret.
- Billing input that contains payment card data must go directly to the payment provider's hosted fields. The app and the API server must not receive raw card numbers.
- The page must not be framed (`X-Frame-Options: DENY`).

## Logging

The app has no server logs. Build diagnostics come from the Rsbuild CLI output in local runs and CI logs. Cloudflare Pages owns request analytics. Client-side logs must not contain access tokens, ID tokens or payment details.

## Build and Test

Run from the repository root:

- `pnpm install --frozen-lockfile`
- `pnpm --filter delino-accounts dev`: development server at `http://127.0.0.1:3100`. The server fails when the port is in use.
- `pnpm exec turbo run build --filter=delino-accounts`: production build into `apps/delino-accounts/dist/`.
- `pnpm exec turbo run typecheck --filter=delino-accounts`: TypeScript type check, including the e2e config and tests.
- `pnpm exec turbo run test --filter=delino-accounts`: Rstest unit tests.
- `pnpm exec turbo run test:e2e --filter=delino-accounts`: browser end-to-end tests against the development server on a free loopback port. See the [e2e contract](repository-e2e-contract.md). `e2e.config.ts` passes `PUBLIC_API_ORIGIN=http://127.0.0.1:18100` to the app command, and `e2e/home.e2e.ts` asserts that the `data-api-origin` attribute of `<html>` equals it without a model key, so the test fails when the build-time variable does not reach the client bundle.
- `pnpm --filter delino-accounts preview`: serve the production build locally.

`turbo.json` declares `PUBLIC_API_ORIGIN` as a `build` environment variable and local `.env*` files as `build` inputs. It also declares `PUBLIC_API_ORIGIN` as a `dev` pass-through variable, so root `pnpm dev` and `pnpm exec turbo run dev --filter=delino-accounts` honor a value provided by the shell and validate it at startup, like `pnpm --filter delino-accounts dev`. Remove `apps/delino-accounts/dist/` after validation. CI runs the e2e suite in the `e2e` job. CI does not build or unit test this app yet; add those checks to the workflow and its planner when the app enters CI. The [deployment workflow](repository-workflow-contract.md#deployment-workflow) builds and deploys it when a pending changeset releases it; `PUBLIC_API_ORIGIN` is not set there, so the build uses the production API origin.

## Dependencies and Integrations

- `@rsbuild/core`, `@rsbuild/plugin-react`, `react`, `react-dom` and `@rstest/core`, pinned in `apps/delino-accounts/package.json`.
- `e2e`, `@e2e-dev/web` and `ai` for end-to-end tests, pinned in `apps/delino-accounts/package.json`.
- [`accounts-api`](servers-accounts-api-foundation.md) at `https://accounts.api.delino.io`.
- Logto shared tenant, according to the [authentication contract](repository-auth-contract.md).
- Cloudflare Pages: static hosting for `accounts.delino.io`. The `deploy` field of `apps/delino-accounts/package.json` names the Direct Upload Pages project `delino-accounts`. The deployment workflow builds with `pnpm exec turbo run build --filter=delino-accounts` and uploads `apps/delino-accounts/dist`.

## Change Triggers

- Update [project-delino-accounts.md](project-delino-accounts.md) when paths, the production origin or the web-to-API boundary change.
- Update [servers-accounts-api-foundation.md](servers-accounts-api-foundation.md) when the app needs a new API capability or a new allowed browser origin.
- Update [repository-workflow-contract.md](repository-workflow-contract.md) when this app enters CI or a deployment workflow.
- Update `apps/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-delino-accounts.md](project-delino-accounts.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [repository-billing-contract.md](repository-billing-contract.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [Rsbuild documentation](https://rsbuild.rs/)
- [Rstest documentation](https://rstest.rs/)
- [Connect-Query](https://github.com/connectrpc/connect-query-es)
