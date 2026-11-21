# apps-delino-docs foundation contract

## Scope

This contract owns the `delino-docs` documentation site at `apps/delino-docs/`:

- `apps/delino-docs/rspress.config.ts`: site configuration.
- `apps/delino-docs/docs/`: page content, navbar data (`docs/_nav.json`), per-directory sidebar order (`_meta.json`) and public static assets (`docs/public/`).
- `apps/delino-docs/theme/`: theme entry and brand style overrides.
- `apps/delino-docs/e2e.config.ts` and `apps/delino-docs/e2e/`: end-to-end test config and tests, according to the [e2e contract](repository-e2e-contract.md).

## Runtime and Language

- Toolchain: Rspress `@rspress/core` 2.x on Rsbuild, React 19.
- Language: TypeScript for configuration and theme code, Markdown/MDX for content.
- Runtime: Node.js from root `.nvmrc` for builds. Production output is static HTML, CSS and JavaScript with no server runtime.
- Deployment target: Cloudflare Pages, production domain `docs.delino.io`.

## Users and Operators

- Delino users who need setup guides and references.
- Maintainers who author public documentation pages and deploy the site.

## Interfaces and Contracts

- Production origin: `https://docs.delino.io`. Rspress `base` is `/`.
- Content language: English (`lang: "en"`).

| Route ID                | Path                     | Purpose                     |
| ----------------------- | ------------------------ | --------------------------- |
| `home`                  | `/`                      | Documentation landing page. |
| `guide-introduction`    | `/guide/introduction`    | Product introduction.       |
| `guide-getting-started` | `/guide/getting-started` | First steps for new users.  |

Sidebar order for `/guide/` is owned by `docs/guide/_meta.json`. The navbar entries (`Guide` and `delino.io`) are owned by `docs/_nav.json`. `rspress.config.ts` must not set `themeConfig.nav` or `themeConfig.sidebar`, because either one disables the automatic navbar and sidebar generation from `_nav.json` and `_meta.json`. Use a directory-level `_meta.json` for any future section that needs an explicit sidebar.

- Build artifacts, relative to `apps/delino-docs/dist/`: one HTML file per route (`/` as `index.html`, `/guide/introduction` as `guide/introduction.html` and `/guide/getting-started` as `guide/getting-started.html`; the build does not emit `<route>/index.html` for the guide routes), `404.html`, `sitemap.xml` with absolute `https://docs.delino.io` URLs, `llms.txt` and `llms-full.txt`.
- `docs/public/_headers` sets Cloudflare Pages response headers: `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin` and `X-Frame-Options: DENY`.
- Rspress built-in full-text search is enabled. The index is generated at build time and is served as static assets; no AI search backend is used yet. If AI search is added, it must default to Cloudflare AI Search per repository defaults.

### Brand colors

The Rspress theme variables `--rp-c-brand`, `--rp-c-brand-dark`, `--rp-c-brand-darker`, `--rp-c-brand-light`, `--rp-c-brand-lighter` and `--rp-c-brand-tint` are set in `theme/index.css`. The site does not share tokens with `delino-ui`; both Rspress apps keep an identical `theme/index.css`.

- Light mode (`:root`) is the source brand blue `#3b5bfd`. It reaches 5.12:1 on the `#ffffff` page background.
- Dark mode (`html.rp-dark`) overrides `--rp-c-brand` to `#8aa0ff`, `--rp-c-brand-light` to `#a3b4ff`, `--rp-c-brand-lighter` to `#c0ccff` and `--rp-c-brand-tint` to `rgba(138, 160, 255, 0.12)`. `--rp-c-brand-dark` (`#2c47d8`) and `--rp-c-brand-darker` (`#2238b3`) stay unchanged in both modes, because the home hero button gradient is built from them and carries white text (7.00:1).
- Rspress paints text directly with `--rp-c-brand` (navbar item, heading anchors, search panel, feature card hover, overview links) and through `--rp-c-link` (links, the active sidebar item and the active table-of-contents item). `--rp-c-link` is `--rp-c-brand-dark` in light mode and `--rp-c-brand-light` in dark mode, and Rspress does not use `--rp-c-brand-light` as a light-mode text color. Every such text color must reach 4.5:1 against its rendered background in both modes.
- Rspress 2.1.3 paints the current search suggestion with `--rp-c-brand` as the background and hard-coded white text. The dark override keeps that background at `#3b5bfd` (white text 5.12:1), and the dark overview hover background mixes `--rp-c-brand-dark` instead of `--rp-c-brand-lighter` (brand text 4.72:1, against 2.65:1 with the brighter ramp). Remove both overrides when Rspress derives these colors from the brand variables.

The dark-mode `--rp-c-brand`, `--rp-c-brand-light` and `--rp-c-brand-lighter` values must each reach 4.5:1 (WCAG 2.1 normal text) against every Rspress dark surface: `#121212`, `#292e37`, `#343a46` and `#000000`. The light-mode brand blue does not meet this on those surfaces, which is why the dark ramp exists.

The e2e suite checks this minimum on the resolved Rspress surface variables (`--rp-c-bg`, `--rp-c-bg-soft`, `--rp-c-bg-mute` and `--rp-c-bg-alt`) after adding the `rp-dark` class to `html`. It also checks that the light-mode brand and the hero gradient colors do not change. It does not measure the light-mode link color `--rp-c-brand-dark`, which reaches 6.20:1 on the darkest light surface (`--rp-c-bg-mute`) and is protected only by the check that it does not change between modes.

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
- `pnpm --filter delino-docs dev`: development server at `http://127.0.0.1:3500`. The server fails when the port is in use and does not start on another port. `PORT` does not change the port.
- `pnpm exec turbo run build --filter=delino-docs`: production build into `apps/delino-docs/dist/`.
- `pnpm exec turbo run typecheck --filter=delino-docs`: TypeScript type check, including the e2e config and tests.
- `pnpm exec turbo run test:e2e --filter=delino-docs`: browser end-to-end tests against the development server on a free loopback port. See the [e2e contract](repository-e2e-contract.md).
- `pnpm --filter delino-docs preview`: serve the production build at `http://127.0.0.1:3500`. Preview uses the same port as the development server and fails when the port is in use, so the development server and preview of one site cannot run together.

Remove `apps/delino-docs/dist/` after validation. CI runs the e2e suite in the `e2e` job. CI does not build this site. The [deployment workflow](repository-workflow-contract.md#deployment-workflow) builds and deploys it when a pending changeset releases it.

## Dependencies and Integrations

- `@rspress/core`, `react` and `react-dom`, pinned in `apps/delino-docs/package.json`.
- `e2e`, `@e2e-dev/web` and `ai` for end-to-end tests, pinned in `apps/delino-docs/package.json`.
- Cloudflare Pages: static hosting for `docs.delino.io`. The `deploy` field of `apps/delino-docs/package.json` names the Direct Upload Pages project `delino-docs`. The deployment workflow builds with `pnpm exec turbo run build --filter=delino-docs` and uploads `apps/delino-docs/dist`.
- [`delino-site`](project-delino-site.md): linked by absolute URL `https://delino.io`.

## Change Triggers

- Update [project-delino-docs.md](project-delino-docs.md) when paths, the production origin or deployables change.
- Update [project-delino-site.md](project-delino-site.md) and its foundation contract when cross-site links change.
- Update [repository-workflow-contract.md](repository-workflow-contract.md) when this site enters CI or a deployment workflow.
- Update `apps/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-delino-docs.md](project-delino-docs.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [Rspress documentation](https://rspress.rs/)
