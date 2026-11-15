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
- Rspress paints text directly with `--rp-c-brand` (navbar item, feature card hover) and with `--rp-c-brand-light` (links). Every such text color must reach 4.5:1 against its rendered background in both modes.
- The dark overrides for the current search suggestion and the overview hover background in `theme/index.css` exist only to keep the file identical to `delino-docs`; this site renders neither.

Measured WCAG 2.1 contrast ratios of the dark-mode text colors on the Rspress dark surfaces:

| Dark text color                  | `#121212` | `#292e37` | `#343a46` | `#000000` |
| -------------------------------- | --------- | --------- | --------- | --------- |
| Before: `#3b5bfd`                | 3.66      | 2.66      | 2.23      | 4.10      |
| `--rp-c-brand` `#8aa0ff`         | 7.63      | 5.55      | 4.65      | 8.55      |
| `--rp-c-brand-light` `#a3b4ff`   | 9.39      | 6.84      | 5.72      | 10.53     |
| `--rp-c-brand-lighter` `#c0ccff` | 11.88     | 8.65      | 7.24      | 13.32     |

The e2e suite checks these minimums on the resolved Rspress surface variables (`--rp-c-bg`, `--rp-c-bg-soft`, `--rp-c-bg-mute` and `--rp-c-bg-alt`) after adding the `rp-dark` class to `html`. A rendered check in Chromium measured 7.63:1 (7.15:1 at the lightest end of the card gradient) for the hovered feature card title on the home page.

## Storage

The site has no persistent storage, user uploads or server-side cache. All content is committed source. Static assets of at least 512 KiB require Git LFS according to the repository workflow contract.

## Security

- The site is public. Content must not include secrets, internal hostnames, repository-internal paths or operational internals.
- The build requires no secrets or environment variables.
- Cloudflare Pages deployment credentials are not stored in the repository.

## Logging

The site has no runtime server logs. Build diagnostics come from the Rspress CLI output in local runs and CI logs. Cloudflare Pages owns request analytics.

## Build and Test

Run from the repository root:

- `pnpm install --frozen-lockfile`
- `pnpm --filter delino-site dev`: local development server.
- `pnpm exec turbo run build --filter=delino-site`: production build into `apps/delino-site/dist/`.
- `pnpm exec turbo run typecheck --filter=delino-site`: TypeScript type check, including the e2e config and tests.
- `pnpm exec turbo run test:e2e --filter=delino-site`: browser end-to-end tests against the development server on a free loopback port. See the [e2e contract](repository-e2e-contract.md).
- `pnpm --filter delino-site preview`: serve the production build locally.

Remove `apps/delino-site/dist/` after validation. CI runs the e2e suite in the `e2e` job. CI does not build or deploy this site yet; add the build check to the workflow and its planner when the site enters CI.

## Dependencies and Integrations

- `@rspress/core`, `react` and `react-dom`, pinned in `apps/delino-site/package.json`.
- `e2e`, `@e2e-dev/web` and `ai` for end-to-end tests, pinned in `apps/delino-site/package.json`.
- Cloudflare Pages: static hosting for `delino.io`. Configure the Pages project with build command `pnpm exec turbo run build --filter=delino-site` and output directory `apps/delino-site/dist`.
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
