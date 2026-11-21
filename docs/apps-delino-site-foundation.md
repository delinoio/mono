# apps-delino-site foundation contract

## Scope

This contract owns the `delino-site` marketing site at `apps/delino-site/`:

- `apps/delino-site/rspress.config.ts`: site configuration.
- `apps/delino-site/docs/`: page content and public static assets (`docs/public/`).
- `apps/delino-site/theme/`: theme entry and brand style overrides.
- `apps/delino-site/e2e.config.ts` and `apps/delino-site/e2e/`: end-to-end test config and tests, according to the [e2e contract](repository-e2e-contract.md).

## Runtime and Language

- Toolchain: Rspress `@rspress/core` 2.x on Rsbuild, React 19.
- Language: TypeScript for configuration and theme code, Markdown/MDX for content.
- Runtime: Node.js from root `.nvmrc` for builds. Production output is static HTML, CSS and JavaScript with no server runtime.
- Deployment target: Cloudflare Pages, production domain `delino.io`.

## Users and Operators

- Prospective users and customers who evaluate Delino.
- Maintainers who edit marketing copy and deploy the site.

## Interfaces and Contracts

- Production origin: `https://delino.io`. Rspress `base` is `/`.
- Content language: English (`lang: "en"`).

| Route ID | Path | Purpose                                                                           |
| -------- | ---- | --------------------------------------------------------------------------------- |
| `home`   | `/`  | Landing page with product hero, feature highlights and links to `docs.delino.io`. |

- Build artifacts: `index.html` per route, `404.html`, `sitemap.xml` with absolute `https://delino.io` URLs, `llms.txt` and `llms-full.txt`.
- `docs/public/_headers` sets Cloudflare Pages response headers: `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin` and `X-Frame-Options: DENY`.
- Built-in search is disabled: `search: false` and `themeConfig.search: false` turn off search indexing and declare the intent, and the theme in `theme/index.tsx` exports a `Search` override that renders nothing because the Rspress 2.1.3 default navbar mounts its search button and Ctrl/Cmd+K shortcut regardless of those options. Remove the override once the default theme honours `themeConfig.search`. Documentation search belongs to `delino-docs`, which keeps the built-in search.

### Brand colors

The Rspress theme variables `--rp-c-brand`, `--rp-c-brand-dark`, `--rp-c-brand-darker`, `--rp-c-brand-light`, `--rp-c-brand-lighter` and `--rp-c-brand-tint` are set in `theme/index.css`. The site does not share tokens with `delino-ui`; both Rspress apps keep an identical `theme/index.css`.

- Light mode (`:root`) is the source brand blue `#3b5bfd`. It reaches 5.12:1 on the `#ffffff` page background.
- Dark mode (`html.rp-dark`) overrides `--rp-c-brand` to `#8aa0ff`, `--rp-c-brand-light` to `#a3b4ff`, `--rp-c-brand-lighter` to `#c0ccff` and `--rp-c-brand-tint` to `rgba(138, 160, 255, 0.12)`. `--rp-c-brand-dark` (`#2c47d8`) and `--rp-c-brand-darker` (`#2238b3`) stay unchanged in both modes, because the home hero button gradient is built from them and carries white text (7.00:1).
- Rspress paints text directly with `--rp-c-brand` (navbar item, feature card hover) and through `--rp-c-link` (links). `--rp-c-link` is `--rp-c-brand-dark` in light mode and `--rp-c-brand-light` in dark mode, and Rspress does not use `--rp-c-brand-light` as a light-mode text color. Every such text color must reach 4.5:1 against its rendered background in both modes.
- The dark overrides for the current search suggestion and the overview hover background in `theme/index.css` exist only to keep the file identical to `delino-docs`; this site renders neither.

The dark-mode `--rp-c-brand`, `--rp-c-brand-light` and `--rp-c-brand-lighter` values must each reach 4.5:1 (WCAG 2.1 normal text) against every Rspress dark surface: `#121212`, `#292e37`, `#343a46` and `#000000`. The light-mode brand blue does not meet this on those surfaces, which is why the dark ramp exists.

The e2e suite checks this minimum on the resolved Rspress surface variables (`--rp-c-bg`, `--rp-c-bg-soft`, `--rp-c-bg-mute` and `--rp-c-bg-alt`) after adding the `rp-dark` class to `html`. It fails when any checked variable is undefined, because an undefined variable would otherwise resolve to the inherited text color and pass. It pins the dark `--rp-c-brand`, `--rp-c-brand-light` and `--rp-c-brand-lighter` values (`#8aa0ff`, `#a3b4ff` and `#c0ccff`). It also checks that the light-mode brand and the hero gradient colors do not change. It does not measure the light-mode link color `--rp-c-brand-dark`, which reaches 6.20:1 on the darkest light surface (`--rp-c-bg-mute`) and is protected only by the check that it does not change between modes.

## Storage

The site has no persistent storage, user uploads or server-side cache. All content is committed source. Static assets of at least 512 KiB require Git LFS according to the repository workflow contract.

## Security

- The site is public. Content must not include secrets, internal hostnames, repository-internal paths or operational internals.
- The build requires no secrets or environment variables.
- Cloudflare Pages deployment credentials are secrets of the `static-sites` GitHub environment. They are not stored in the repository.

## Logging

The site has no runtime server logs. Build diagnostics come from the Rspress CLI output in local runs and CI logs. Cloudflare Pages owns request analytics.

## Build and Test

Run from the repository root:

- `pnpm install --frozen-lockfile`
- `pnpm --filter delino-site dev`: development server at `http://127.0.0.1:3600`. The server fails when the port is in use and does not start on another port. `PORT` does not change the port.
- `pnpm exec turbo run build --filter=delino-site`: production build into `apps/delino-site/dist/`.
- `pnpm exec turbo run typecheck --filter=delino-site`: TypeScript type check, including the e2e config and tests.
- `pnpm exec turbo run test:e2e --filter=delino-site`: browser end-to-end tests against the development server on a free loopback port. The suite also starts `rspress dev` without a port flag while a loopback listener holds port 3600, and with an explicit port that a listener holds, and expects a non-zero exit with the `Port <n> is occupied` message. See the [e2e contract](repository-e2e-contract.md).
- `pnpm --filter delino-site preview`: serve the production build at `http://127.0.0.1:3600`. Preview uses the same port as the development server and fails when the port is in use, so the development server and preview of one site cannot run together.

Remove `apps/delino-site/dist/` after validation. CI runs the e2e suite in the `e2e` job. CI builds and type-checks this site in the `workspace-packages` job of the [workflow contract](repository-workflow-contract.md#validation) but does not deploy it. The [deployment workflow](repository-workflow-contract.md#deployment-workflow) builds and deploys it when a pending changeset releases it.

## Dependencies and Integrations

- `@rspress/core`, `react` and `react-dom`, pinned in `apps/delino-site/package.json`.
- `e2e`, `@e2e-dev/web` and `ai` for end-to-end tests, pinned in `apps/delino-site/package.json`.
- Cloudflare Pages: static hosting for `delino.io`. The `deploy` field of `apps/delino-site/package.json` names the Direct Upload Pages project `delino-site`. The deployment workflow builds with `pnpm exec turbo run build --filter=delino-site` and uploads `apps/delino-site/dist`.
- [`delino-docs`](project-delino-docs.md): linked by absolute URL `https://docs.delino.io`.

## Change Triggers

- Update [project-delino-site.md](project-delino-site.md) when paths, the production origin or deployables change.
- Update [project-delino-docs.md](project-delino-docs.md) and its foundation contract when cross-site links change.
- Update [repository-workflow-contract.md](repository-workflow-contract.md) when this site enters CI or a deployment workflow.
- Update `apps/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-delino-site.md](project-delino-site.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [Rspress documentation](https://rspress.rs/)
