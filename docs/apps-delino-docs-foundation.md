# apps-delino-docs foundation contract

## Scope

This contract owns the `delino-docs` documentation site at `apps/delino-docs/`:

- `apps/delino-docs/rspress.config.ts`: site configuration, including the navbar and sidebar of every language.
- `apps/delino-docs/docs/`: English source pages (`docs/en/`), generated translated pages (`docs/<lang>/`) and public static assets (`docs/public/`).
- `apps/delino-docs/i18n/`: language registry (`locales.json`), text catalogs (`messages/<lang>.json`), translation state (`state.json`), `NOTICE` and `RSPRESS-LICENSE`. See [Localization](#localization).
- `apps/delino-docs/theme/`: theme entry, cross-site link localization (`theme/cross-site-link.ts`) and brand style overrides.
- `apps/delino-docs/e2e.config.ts` and `apps/delino-docs/e2e/`: end-to-end test config and tests, according to the [e2e contract](repository-e2e-contract.md).

## Runtime and Language

- Toolchain: Rspress `@rspress/core` 2.x on Rsbuild, React 19.
- Language: TypeScript for configuration and theme code, Markdown (`.md`) for content. The translation tool rejects MDX source pages.
- Runtime: Node.js from root `.nvmrc` for builds. Production output is static HTML, CSS and JavaScript with no server runtime.
- Deployment target: Cloudflare Pages, production domain `docs.delino.io`.

## Users and Operators

- Delino users who need setup guides and references.
- Maintainers who author public documentation pages and deploy the site.

## Interfaces and Contracts

- Production origin: `https://docs.delino.io`. Rspress `base` is `/`.
- Content languages: English (`lang: "en"`) is the source language. The other languages of the [localization contract](repository-localization-contract.md) use the `/<lang>/` route prefix. See [Localization](#localization).

| Route ID                | Path                     | Source page                        | Purpose                     |
| ----------------------- | ------------------------ | ---------------------------------- | --------------------------- |
| `home`                  | `/`                      | `docs/en/index.md`                 | Documentation landing page. |
| `guide-introduction`    | `/guide/introduction`    | `docs/en/guide/introduction.md`    | Product introduction.       |
| `guide-getting-started` | `/guide/getting-started` | `docs/en/guide/getting-started.md` | First steps for new users.  |

Each route also exists at `/<lang>` followed by its path in every other registered language, for example `/ko/` and `/ko/guide/introduction`, from `docs/<lang>/` and the same relative page path.

`themeConfig.locales` in `rspress.config.ts` owns the navbar and the sidebar of every language, built from that language's catalog with the `/<lang>` route prefix: the navbar entries are `Guide` (`nav.guide`, linking to `/guide/introduction` and active under `/guide/`) and `delino.io`, and the `/guide/` sidebar lists `Introduction` (`sidebar.introduction`) and then `Getting started` (`sidebar.gettingStarted`). The site does not use `_nav.json` or `_meta.json` files. Add the navbar and sidebar entries of a new page to `rspress.config.ts` and their texts to `i18n/messages/en.json`.

- Build artifacts, relative to `apps/delino-docs/dist/`: one HTML file per route (`/` as `index.html`, `/guide/introduction` as `guide/introduction.html` and `/guide/getting-started` as `guide/getting-started.html`; the build does not emit `<route>/index.html` for the guide routes), the same files under `<lang>/` for the other languages, `404.html`, `sitemap.xml` with absolute `https://docs.delino.io` URLs, and `llms.txt` and `llms-full.txt` at the root and in each `<lang>/` directory.
- `docs/public/_headers` sets Cloudflare Pages response headers: `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin` and `X-Frame-Options: DENY`.
- Rspress built-in full-text search is enabled. The index is generated at build time and is served as static assets; no AI search backend is used yet. If AI search is added, it must default to Cloudflare AI Search per repository defaults.

### Localization

The site follows the Rspress rules of the [localization contract](repository-localization-contract.md#rspress-apps). The [`delino-i18n` package contract](packages-delino-i18n-foundation.md) owns the catalog format and the translation tool.

- English pages live in `docs/en/` and keep their unprefixed routes. `delino-i18n translate` generates the pages of the other languages in `docs/<lang>/` and prefixes internal absolute links, including frontmatter `link:` values such as the home hero action `/guide/introduction`, with `/<lang>`; do not edit or remove the generated pages by hand except to correct a translation. The Rspress `lang` is the registry `sourceLang`, and Rspress `locales` and `themeConfig.locales` list every registered language with its native label in registry order. `route.localeRedirect` is `"never"`, so Rspress does not redirect by browser language. Rspress owns the language menu, the per-language full-text search and the HTML `lang` attribute.
- `i18n/messages/en.json` holds the site description (`description`), the navbar and sidebar texts (`nav.guide`, `sidebar.introduction` and `sidebar.gettingStarted`) and one `theme.<key>` entry for each default theme text of the pinned `@rspress/core` (35 keys for 2.1.3, for example `theme.searchPlaceholderText`). The `i18nSource` function in `rspress.config.ts` replaces the Rspress default texts of every registered language with these entries. The build fails when a default theme text has no `theme.*` entry in the source catalog or when the source catalog has a `theme.*` entry that Rspress does not define. After an `@rspress/core` upgrade, update the `theme.*` entries from the default texts of the new version, update `i18n/NOTICE` and run `pnpm i18n:translate`.
- A text that is missing from a target catalog falls back to the English source text, so the site builds before translations are generated. The `test` script (`delino-i18n check`) fails on every missing or stale translation, so CI does not accept the fallback.
- The product names `Delino Docs` (title and logo text) and the `delino.io` navbar label stay unchanged and are not catalog entries.
- Links to Delino Site keep the selected language. `localizeCrossSiteHref` in `theme/cross-site-link.ts` adds `/<lang>` to a `https://delino.io` URL for every registered language other than English and keeps other URLs and already prefixed URLs unchanged; English pages link to the unprefixed URL. `rspress.config.ts` applies it to the per-language navbar `delino.io` item. The theme `Link` export in `theme/index.tsx` applies it with the language of the current page to every link that the default theme renders, including the home hero actions and Markdown links. Page frontmatter therefore keeps `https://delino.io` in every language: the translation tool requires external URLs in translated pages to stay unchanged, so a translated page cannot carry a language-specific URL.
- `i18n/NOTICE` records that the `theme.*` entries and their translations originate from the default theme texts of `@rspress/core` 2.1.3 and stay under its MIT License, whose text is in `i18n/RSPRESS-LICENSE`; the other catalog entries and translated pages use Apache-2.0. Keep both files in the source tree. The deployed static site does not carry them, because static site hosting falls outside the distribution requirements of the [license contract](repository-license-contract.md#distribution).

### Brand colors

The Rspress theme variables `--rp-c-brand`, `--rp-c-brand-dark`, `--rp-c-brand-darker`, `--rp-c-brand-light`, `--rp-c-brand-lighter` and `--rp-c-brand-tint` are set in `theme/index.css`. The site does not share tokens with `delino-ui`; both Rspress apps keep an identical `theme/index.css`.

- Light mode (`:root`) is the source brand blue `#3b5bfd`. It reaches 5.12:1 on the `#ffffff` page background.
- Dark mode (`html.rp-dark`) overrides `--rp-c-brand` to `#8aa0ff`, `--rp-c-brand-light` to `#a3b4ff`, `--rp-c-brand-lighter` to `#c0ccff` and `--rp-c-brand-tint` to `rgba(138, 160, 255, 0.12)`. `--rp-c-brand-dark` (`#2c47d8`) and `--rp-c-brand-darker` (`#2238b3`) stay unchanged in both modes, because the home hero button gradient and the light-mode wordmark gradient are built from them. The white button label reaches 7.00:1 at the `--rp-c-brand-dark` stops.
- Rspress paints text directly with `--rp-c-brand` (navbar item, heading anchors, search panel, feature card hover, overview links) and through `--rp-c-link` (links, the active sidebar item and the active table-of-contents item). `--rp-c-link` is `--rp-c-brand-dark` in light mode and `--rp-c-brand-light` in dark mode, and Rspress does not use `--rp-c-brand-light` as a light-mode text color. Every such text color must reach 4.5:1 against its rendered background in both modes.
- In light mode, the destination title in each unselected full-text search result must reach 4.5:1 against the row background. Rspress 2.1.3 uses a gray that reaches only 2.22:1. The theme uses `--rp-c-text-2` for these titles. The e2e suite checks the rendered contrast before and after moving selection with ArrowDown. Remove the override when Rspress provides at least 4.5:1 for this state.
- Rspress 2.1.3 paints the current search suggestion with `--rp-c-brand` as the background and hard-coded white text. The dark override keeps that background at `#3b5bfd` (white text 5.12:1), and the dark overview hover background mixes `--rp-c-brand-dark` instead of `--rp-c-brand-lighter` (brand text 4.72:1, against 2.65:1 with the brighter ramp). Remove both overrides when Rspress derives these colors from the brand variables.
- Rspress mixes 30% of `--rp-c-brand-lighter` into the hover background of an overview group title and paints the hovered title text with `--rp-c-brand`, which leaves 3.91:1 on `--rp-c-bg` and 3.74:1 on `--rp-c-bg-soft` in light mode. The light override mixes 4% of `--rp-c-brand` instead, so the hovered title text reaches 4.5:1 on both surfaces; the dark override above is unchanged. Remove the override when Rspress derives this hover background from a variable that keeps brand text at 4.5:1 in light mode. No page of either app renders an overview group yet, so the e2e suite adds a minimal overview group with the Rspress class names and checks the hovered title in both modes.
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
- `pnpm --filter delino-docs dev`: development server through portless at `https://docs.delino.localhost`, in every checkout. Portless assigns the backend a free loopback port. See [Frontend development URLs](repository-environment-contract.md#frontend-development-urls).
- `pnpm --filter delino-docs dev:app`, or `dev` with Portless bypassed (`PORTLESS=0`, `PORTLESS=false`, or `PORTLESS=skip`): development server at `http://127.0.0.1:13500`. The server fails when the port is in use and does not start on another port. `PORT` changes the port only when portless also sets `PORTLESS_URL`.
- `pnpm exec turbo run build --filter=delino-docs`: production build into `apps/delino-docs/dist/`.
- `pnpm exec turbo run typecheck --filter=delino-docs`: TypeScript type check, including the e2e config and tests.
- `pnpm --filter delino-docs test`: offline translation check (`delino-i18n check`). It fails when a translated page or catalog entry is missing, stale or structurally invalid.
- `pnpm --filter delino-docs i18n:translate [lang]`: generate missing or source-changed translations; see the [translation workflow](repository-localization-contract.md#translation-workflow).
- `pnpm exec turbo run test:e2e --filter=delino-docs`: browser end-to-end tests against the development server on a free loopback port. Free `127.0.0.1:13500` before running the full suite so the no-flag fixture can reserve it. The explicit-port fixture reserves a separate ephemeral loopback port. Both fixtures start `rspress dev` and expect a non-zero exit with the `Port <n> is occupied` message. On `/ko/` it checks `<html lang="ko">`, the language menu and the `https://delino.io/ko/` hero action link; these checks need the generated Korean pages. See the [e2e contract](repository-e2e-contract.md).
- `pnpm --filter delino-docs preview`: serve an existing production build at `http://127.0.0.1:13500` with the default arguments. Default preview can run alongside default Portless development if port 13500 is free when preview starts. A direct `dev:app` launch or a Portless bypass uses the same port as default preview, so the second command fails to bind. The script runs `node ../../scripts/dev/rspress-preview.mjs`, which removes `PORT` and `HOST` from the environment of `rspress preview` because Rspress reads them before its flags and configuration; arguments after `preview` reach `rspress preview`. See [Frontend development URLs](repository-environment-contract.md#frontend-development-urls).

Remove `apps/delino-docs/dist/` after validation. CI runs the e2e suite in the `e2e` job. CI builds and type-checks this site in the `workspace-packages` job of the [workflow contract](repository-workflow-contract.md#validation) but does not deploy it. The [deployment workflow](repository-workflow-contract.md#deployment-workflow) builds and deploys it when a pending changeset releases it.

## Dependencies and Integrations

- `@rspress/core`, `react` and `react-dom`, pinned in `apps/delino-docs/package.json`.
- `e2e`, `@e2e-dev/web` and `ai` for end-to-end tests, pinned in `apps/delino-docs/package.json`.
- [`delino-i18n`](project-delino-i18n.md) for the translation check and generation, a workspace devDependency.
- Cloudflare Pages: static hosting for `docs.delino.io`. The `deploy` field of `apps/delino-docs/package.json` names the Direct Upload Pages project `delino-docs`. The deployment workflow builds with `pnpm exec turbo run build --filter=delino-docs` and uploads `apps/delino-docs/dist`.
- [`delino-site`](project-delino-site.md): linked by absolute URL `https://delino.io`, or `https://delino.io/<lang>/` from a page in another registered language.

## Change Triggers

- Update [project-delino-docs.md](project-delino-docs.md) when paths, the production origin or deployables change.
- Update [project-delino-site.md](project-delino-site.md) and its foundation contract when cross-site links change.
- Update [repository-workflow-contract.md](repository-workflow-contract.md) when this site enters CI or a deployment workflow.
- Update the [localization contract](repository-localization-contract.md) when language routes or language selection change, and the `theme.*` catalog entries and `i18n/NOTICE` when the pinned `@rspress/core` changes.
- Update `apps/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-delino-docs.md](project-delino-docs.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [repository-localization-contract.md](repository-localization-contract.md)
- [Rspress documentation](https://rspress.rs/)
