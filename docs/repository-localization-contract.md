# Repository Localization Contract

## Scope

This contract owns the languages and the language selection behavior of every app under `apps/`: `delino-site`, `delino-docs`, `delino-accounts`, `ownsand-console` (`apps/ownsand/`), `sandrun-console` (`apps/sandrun/`), `sandcrew-console` (`apps/sandcrew/`) and `delidev` (`apps/delidev/`). The [`delino-i18n` package contract](packages-delino-i18n-foundation.md) owns the catalog format, the translation tool, the ChatGPT integration and the browser runtime.

## Languages

Every app uses English (`en`) as its source language and registers the same target languages, identified by BCP 47 tags, in this menu order: Korean (`ko`), Japanese (`ja`), Simplified Chinese (`zh-Hans`), Spanish (`es`), French (`fr`), German (`de`) and Brazilian Portuguese (`pt-BR`). Right-to-left languages are not registered until the layouts define mirroring. Each app owns its `i18n/locales.json` registry, its `i18n/messages/<lang>.json` catalogs and its `i18n/state.json`.

Keep every user-visible text in the source catalog, not in components: headings, labels, buttons, descriptions, empty states, errors shown to users, accessible names and document titles. These stay unchanged and untranslated: product and company names (Delino, DeliDev, OwnSand, Sandrun, SandCrew, Logto, Paddle, GitHub, ChatGPT and other third-party names), identifiers, code, commands, URLs, enum values sent to APIs, values from the server and user content. Error text that a server returns is shown as received. Prices stay in USD as the [billing contract](repository-billing-contract.md) requires; numbers, dates and currency amounts are formatted with `Intl` for the selected language.

## Language selection

### Rspress apps

`delino-site` and `delino-docs` keep English routes at their existing paths. Other languages use `/<lang>/`, for example `/ko/` and `/zh-Hans/`. Rspress owns the language menu, navigation, search and the HTML `lang` attribute, and does not redirect by browser language. Pages live in `docs/<lang>/`; navigation, sidebar and theme texts come from the app catalog. Links between Delino Site and Delino Docs keep the selected language.

The `theme.*` catalog entries originate from the pinned Rspress theme and keep its MIT license. Keep `i18n/NOTICE` and `i18n/RSPRESS-LICENSE` in each Rspress app.

### React apps

`delino-accounts`, `ownsand-console`, `sandrun-console`, `sandcrew-console` and `delidev` keep their routes unchanged in every language, because sign-in redirect URIs, invitation links and deep links are path-based. Before the first render, the app selects the language stored under the `localStorage` key `delino.language`, otherwise the best match for the browser languages, otherwise English (see `matchLanguage` in the package contract). The app shell shows a language menu that lists each registered language by its native label, on signed-in and signed-out pages. A selection applies immediately, without a page reload, and is kept in the browser. The source catalog is part of the main bundle; each target catalog is a separate chunk that loads only when selected.

## Translation workflow

Run these commands from the repository root:

- `pnpm i18n:login`: continue with ChatGPT in the system browser and authorize ChatGPT plan usage. `pnpm i18n:login --new` adds a registration and `pnpm i18n:login <profile-id>` reauthorizes and selects one; `pnpm i18n:profiles` lists them.
- `pnpm i18n:models`: list the models available to the account.
- `pnpm i18n:translate [lang]`: generate missing or source-changed translations in every app, two apps at a time. Set `DELINO_TRANSLATION_MODEL` to a model slug from `pnpm i18n:models`; otherwise the first listed model is used. Set `DELINO_TRANSLATION_CONCURRENCY` (1 to 8, default 2) to change the number of concurrent requests per app; a higher value finishes sooner but uses the plan's usage limits faster.
- `pnpm i18n:check`: run the offline check in every app.
- `pnpm i18n:logout`: end the session and clear local credentials.

Each app also provides `pnpm i18n:check` and `pnpm i18n:translate [lang]` in its directory, and its `test` script runs the offline check, so CI fails when a translation is missing or stale.

To change text, edit the source catalog or the English page, run `pnpm i18n:translate`, review the generated changes and commit them together. To remove text, remove the key or page from the source and remove its translations and state entries explicitly. To add a language, add its `lang` and native `label` to every app registry, run `pnpm i18n:translate`, review the generated files and run the app tests. Structural validation cannot establish linguistic accuracy; review translations for meaning. A maintainer may correct a generated translation by hand; the tool keeps it until its source text changes.

## Validation

`delino-i18n check` runs in every app `test` script. App unit and e2e tests run in English unless a test selects another language. Local builds do not establish Cloudflare Pages deployment acceptance, and mocked tool tests do not establish real ChatGPT account acceptance; record those checks, the source revision and the commands in pull requests or CI records.

## References

- [delino-i18n project index](project-delino-i18n.md)
- [delino-i18n package foundation](packages-delino-i18n-foundation.md)
- [delino-site foundation](apps-delino-site-foundation.md)
- [delino-docs foundation](apps-delino-docs-foundation.md)
- [delino-accounts web app foundation](apps-delino-accounts-foundation.md)
- [ownsand-console foundation](apps-ownsand-console-foundation.md)
- [sandrun-console foundation](apps-sandrun-console-foundation.md)
- [sandcrew-console foundation](apps-sandcrew-console-foundation.md)
- [delidev foundation](apps-delidev-foundation.md)
- [Repository defaults](repository-defaults.md)
