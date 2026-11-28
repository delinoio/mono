# apps-delino-site foundation contract

## Scope

This contract owns the `delino-site` marketing site at `apps/delino-site/`:

- `apps/delino-site/rspress.config.ts`: site configuration.
- `apps/delino-site/docs/`: English source pages (`docs/en/`), generated translated pages (`docs/<lang>/`) and public static assets (`docs/public/`).
- `apps/delino-site/i18n/`: language registry (`locales.json`), text catalogs (`messages/<lang>.json`), translation state (`state.json`), `NOTICE` and `RSPRESS-LICENSE`. See [Localization](#localization).
- `apps/delino-site/theme/`: theme entry, cross-site link localization (`theme/cross-site-link.ts`) and brand style overrides.
- `apps/delino-site/e2e.config.ts` and `apps/delino-site/e2e/`: end-to-end test config and tests, according to the [e2e contract](repository-e2e-contract.md).

## Runtime and Language

- Toolchain: Rspress `@rspress/core` 2.x on Rsbuild, React 19.
- Language: TypeScript for configuration and theme code, Markdown (`.md`) for content. The translation tool rejects MDX source pages.
- Runtime: Node.js from root `.nvmrc` for builds. Production output is static HTML, CSS and JavaScript with no server runtime.
- Deployment target: Cloudflare Pages, production domain `delino.io`.

## Users and Operators

- Prospective users and customers who evaluate Delino.
- Maintainers who edit marketing copy and deploy the site.

## Interfaces and Contracts

- Production origin: `https://delino.io`. Rspress `base` is `/`.
- Content languages: English (`lang: "en"`) is the source language. The other languages of the [localization contract](repository-localization-contract.md) use the `/<lang>/` route prefix. See [Localization](#localization).

| Route ID | Path               | Purpose                                                                           |
| -------- | ------------------ | --------------------------------------------------------------------------------- |
| `home`   | `/` and `/<lang>/` | Landing page with product hero, feature highlights and links to `docs.delino.io`. |

- The `home` feature cards in `docs/en/index.md` keep distinct titles, because Rspress uses each title as the React key of its card; a repeated title makes the development console report duplicate keys. The e2e suite checks that the page renders three cards with distinct titles.
- Build artifacts: `index.html` per route (`<lang>/index.html` for the other languages), `404.html`, `sitemap.xml` with absolute `https://delino.io` URLs, and `llms.txt` and `llms-full.txt` at the root and in each `<lang>/` directory.
- `docs/public/_headers` sets Cloudflare Pages response headers: `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin` and `X-Frame-Options: DENY`.
- Built-in search is disabled: `search: false` and `themeConfig.search: false` turn off search indexing and declare the intent, and the theme in `theme/index.tsx` exports a `Search` override that renders nothing because the Rspress 2.1.3 default navbar mounts its search button and Ctrl/Cmd+K shortcut regardless of those options. Remove the override once the default theme honours `themeConfig.search`. Documentation search belongs to `delino-docs`, which keeps the built-in search.

### Localization

The site follows the Rspress rules of the [localization contract](repository-localization-contract.md#rspress-apps). The [`delino-i18n` package contract](packages-delino-i18n-foundation.md) owns the catalog format and the translation tool.

- English pages live in `docs/en/` and keep their unprefixed routes. `delino-i18n translate` generates the pages of the other languages in `docs/<lang>/`; do not edit or remove them by hand except to correct a translation. The Rspress `lang` is the registry `sourceLang`, and Rspress `locales` and `themeConfig.locales` list every registered language with its native label in registry order. `route.localeRedirect` is `"never"`, so Rspress does not redirect by browser language. Rspress owns the language menu and the HTML `lang` attribute.
- `i18n/messages/en.json` holds the navigation text (`nav.docs`) and one `theme.<key>` entry for each default theme text of the pinned `@rspress/core` (35 keys for 2.1.3, for example `theme.notFoundText`). The `i18nSource` function in `rspress.config.ts` replaces the Rspress default texts of every registered language with these entries. The build fails when a default theme text has no `theme.*` entry in the source catalog or when the source catalog has a `theme.*` entry that Rspress does not define. After an `@rspress/core` upgrade, update the `theme.*` entries from the default texts of the new version, update `i18n/NOTICE` and run `pnpm i18n:translate`.
- A text that is missing from a target catalog falls back to the English source text, so the site builds before translations are generated. The `test` script (`delino-i18n check`) fails on every missing or stale translation, so CI does not accept the fallback.
- The product names `Delino` (title and logo text) and `GitHub` stay unchanged and are not catalog entries.
- Links to Delino Docs keep the selected language. `localizeCrossSiteHref` in `theme/cross-site-link.ts` adds `/<lang>` to a `https://docs.delino.io` URL for every registered language other than English and keeps other URLs and already prefixed URLs unchanged; English pages link to the unprefixed URL. `rspress.config.ts` applies it to the per-language navbar `Docs` item. The theme `Link` export in `theme/index.tsx` applies it with the language of the current page to every link that the default theme renders, including the home hero actions and Markdown links. Page frontmatter therefore keeps `https://docs.delino.io` in every language: the translation tool requires external URLs in translated pages to stay unchanged, so a translated page cannot carry a language-specific URL.
- `i18n/NOTICE` records that the `theme.*` entries and their translations originate from the default theme texts of `@rspress/core` 2.1.3 and stay under its MIT License, whose text is in `i18n/RSPRESS-LICENSE`; the other catalog entries and translated pages use Apache-2.0. Keep both files in the source tree. The deployed static site does not carry them, because static site hosting falls outside the distribution requirements of the [license contract](repository-license-contract.md#distribution).

### Brand colors

The Rspress theme variables `--rp-c-brand`, `--rp-c-brand-dark`, `--rp-c-brand-darker`, `--rp-c-brand-light`, `--rp-c-brand-lighter` and `--rp-c-brand-tint` are set in `theme/index.css`. The site does not share tokens with `delino-ui`; both Rspress apps keep an identical `theme/index.css`.

- Light mode (`:root`) is the source brand blue `#3b5bfd`. It reaches 5.12:1 on the `#ffffff` page background.
- Dark mode (`html.rp-dark`) overrides `--rp-c-brand` to `#8aa0ff`, `--rp-c-brand-light` to `#a3b4ff`, `--rp-c-brand-lighter` to `#c0ccff` and `--rp-c-brand-tint` to `rgba(138, 160, 255, 0.12)`. `--rp-c-brand-dark` (`#2c47d8`) and `--rp-c-brand-darker` (`#2238b3`) stay unchanged in both modes, because the home hero button gradient and the light-mode wordmark gradient are built from them. The white button label reaches 7.00:1 at the `--rp-c-brand-dark` stops.
- Rspress paints text directly with `--rp-c-brand` (navbar item, feature card hover) and through `--rp-c-link` (links). `--rp-c-link` is `--rp-c-brand-dark` in light mode and `--rp-c-brand-light` in dark mode, and Rspress does not use `--rp-c-brand-light` as a light-mode text color. Every such text color must reach 4.5:1 against its rendered background in both modes.
- This site keeps the Delino Docs search and overview hover overrides in `theme/index.css` to preserve the identical stylesheets. The site has no page with an overview group. The light-mode search override applies to unselected destination titles. Remove it from both themes when Rspress provides at least 4.5:1 for this state. The e2e suite still checks the overview hover with a minimal overview group that it adds to the page: the light override mixes 4% of `--rp-c-brand` into the hover background, so the hovered title text reaches 4.5:1 on `--rp-c-bg` and `--rp-c-bg-soft`.
- Rspress ends the hero gradient of the brand button and of the wordmark at `--rp-home-hero-secondary-color` (`#a673ff` by default), which carries the white button label at only 3.22:1. `theme/index.css` sets it to `#5b68e6` in `:root`, so the white label reaches 4.5:1 or more along the whole gradient at rest in both modes (4.60:1 at the end stop). In dark mode Rspress starts the wordmark gradient at `--rp-c-brand-dark`, which reaches 2.68:1 on `--rp-c-bg` and 2.27:1 on the hero glow `rgb(33, 25, 81)`; the dark override starts it at `--rp-c-brand` and ends it at `--rp-c-brand-lighter`, and `--rp-home-hero-title-bg` (the button gradient) stays unchanged. Every wordmark stop reaches 3:1 (large text) on `--rp-c-bg` and on the hero glow in both modes (the light-mode glow is `rgb(231, 232, 255)`). The e2e suite reads the computed gradient stops of the hero button and wordmark and checks these ratios in both modes. Remove the overrides when Rspress derives the gradient from the brand variables.
- In both color modes, the home hero brand action stays fully opaque and keeps the white label at 4.5:1 or more across the gradient. This applies at rest, while the pointer enters or leaves, and while it stays over the action. Rspress 2.1.3 fades the whole button on hover. The theme transitions only the shadow for hover feedback and restores the resting shadow when the pointer leaves. The e2e suite checks these states in both modes. Remove this override when Rspress provides a hover treatment that preserves the contrast.
- Rspress paints the hovered 404 "Take me home" link (text and border) with `--rp-c-brand-dark`, which reaches only 2.68:1 on the dark page background. The dark override uses `--rp-c-brand-light` (`#a3b4ff`) for both, so the hovered text reaches 4.5:1 and the border 3:1 against `--rp-c-bg`; light mode keeps `--rp-c-brand-dark`. The e2e suite hovers the link in both modes and checks these values. Remove the override when Rspress derives this hover color from a variable that the dark brand ramp controls.

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
- `pnpm --filter delino-site dev`: development server through portless at `https://delino.localhost`, in every checkout. See [Frontend development URLs](repository-environment-contract.md#frontend-development-urls).
- `pnpm --filter delino-site dev:app`, or `dev` with `PORTLESS=0`: development server at `http://127.0.0.1:13600`. The server fails when the port is in use and does not start on another port. `PORT` changes the port only when portless also sets `PORTLESS_URL`.
- `pnpm exec turbo run build --filter=delino-site`: production build into `apps/delino-site/dist/`.
- `pnpm exec turbo run typecheck --filter=delino-site`: TypeScript type check, including the e2e config and tests.
- `pnpm --filter delino-site test`: offline translation check (`delino-i18n check`). It fails when a translated page or catalog entry is missing, stale or structurally invalid.
- `pnpm --filter delino-site i18n:translate [lang]`: generate missing or source-changed translations; see the [translation workflow](repository-localization-contract.md#translation-workflow).
- `pnpm exec turbo run test:e2e --filter=delino-site`: browser end-to-end tests against the development server on a free loopback port. The suite also starts `rspress dev` without a port flag while a loopback listener holds port 13600, and with an explicit port that a listener holds, and expects a non-zero exit with the `Port <n> is occupied` message. On `/ko/` it checks `<html lang="ko">`, the language menu and the `https://docs.delino.io/ko/` hero action link; these checks need the generated Korean pages. See the [e2e contract](repository-e2e-contract.md).
- `pnpm --filter delino-site preview`: serve the production build at `http://127.0.0.1:13600`. Preview uses the same port as the development server and fails when the port is in use, so the development server and preview of one site cannot run together. The script runs `node ../../scripts/dev/rspress-preview.mjs`, which removes `PORT` and `HOST` from the environment of `rspress preview` because Rspress reads them before its flags and configuration; arguments after `preview` reach `rspress preview`. See [Frontend development URLs](repository-environment-contract.md#frontend-development-urls).

Remove `apps/delino-site/dist/` after validation. CI runs the e2e suite in the `e2e` job. CI builds and type-checks this site in the `workspace-packages` job of the [workflow contract](repository-workflow-contract.md#validation) but does not deploy it. The [deployment workflow](repository-workflow-contract.md#deployment-workflow) builds and deploys it when a pending changeset releases it.

## Dependencies and Integrations

- `@rspress/core`, `react` and `react-dom`, pinned in `apps/delino-site/package.json`.
- `e2e`, `@e2e-dev/web` and `ai` for end-to-end tests, pinned in `apps/delino-site/package.json`.
- [`delino-i18n`](project-delino-i18n.md) for the translation check and generation, a workspace devDependency.
- Cloudflare Pages: static hosting for `delino.io`. The `deploy` field of `apps/delino-site/package.json` names the Direct Upload Pages project `delino-site`. The deployment workflow builds with `pnpm exec turbo run build --filter=delino-site` and uploads `apps/delino-site/dist`.
- [`delino-docs`](project-delino-docs.md): linked by absolute URL `https://docs.delino.io`, or `https://docs.delino.io/<lang>/` from a page in another registered language.

## Change Triggers

- Update [project-delino-site.md](project-delino-site.md) when paths, the production origin or deployables change.
- Update [project-delino-docs.md](project-delino-docs.md) and its foundation contract when cross-site links change.
- Update [repository-workflow-contract.md](repository-workflow-contract.md) when this site enters CI or a deployment workflow.
- Update the [localization contract](repository-localization-contract.md) when language routes or language selection change, and the `theme.*` catalog entries and `i18n/NOTICE` when the pinned `@rspress/core` changes.
- Update `apps/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-delino-site.md](project-delino-site.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [repository-localization-contract.md](repository-localization-contract.md)
- [Rspress documentation](https://rspress.rs/)
