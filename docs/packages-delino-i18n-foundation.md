# packages-delino-i18n foundation contract

## Scope

This contract owns the [`delino-i18n`](project-delino-i18n.md) package at `packages/delino-i18n/`:

- `packages/delino-i18n/package.json`: package manifest, subpath exports, the `delino-i18n` bin and pinned dependencies.
- `packages/delino-i18n/src/`: the browser runtime and its unit tests.
- `packages/delino-i18n/cli/`: the maintainer command-line tool, the ChatGPT client and their `node:test` tests.
- `packages/delino-i18n/rstest.config.ts` and `packages/delino-i18n/tsconfig.json`: test and type-check configuration.

The [localization contract](repository-localization-contract.md) owns the registered languages and the behavior of each app.

## Runtime and Language

- Runtime (`src/`): TypeScript with React 19 in the browser of the consuming app. Consumers compile the TypeScript source with Rsbuild; the package has no build script and writes no `dist` directory.
- Tool (`cli/`): JavaScript modules for Node.js from root `.nvmrc`. `jose` verifies ChatGPT ID tokens and `js-yaml` reads Markdown frontmatter.
- Tests: Rstest with `happy-dom` for `src/`, `node --test` for `cli/`.
- Deviation from repository defaults: TypeScript and JavaScript instead of Go; see the [project index](project-delino-i18n.md).

## Users and Operators

- Maintainers of Delino apps, who add source text, run the tool and review translations.
- Users of the apps, who select a language.

## Interfaces and Contracts

### Catalog files

Each participating app owns these files:

- `i18n/locales.json`: `sourceLang` (`en`), `locales` (a list of `{ "lang", "label" }` with a BCP 47 tag and the native-language label, in menu order, including the source language) and an optional `context`, a short English description of the app that the tool gives to the model with every request. The registry drives language menus and routes; adding a language does not require a code change in the runtime.
- `i18n/messages/<lang>.json`: a flat object of stable keys (`^[A-Za-z][\w.]*$`) and nonempty string values. The source catalog `en.json` is written by maintainers. Target catalogs are generated.
- `i18n/state.json`: the SHA-256 fingerprint of the source text of each translated unit, per language. The tool writes it.
- `docs/<lang>/`: for Rspress apps, the Markdown pages. `docs/en/` is the source. Target directories are generated.

Placeholders use `{name}`. A plural group is a set of keys `<base>.<category>`, where the category is an `Intl.PluralRules` category (`zero`, `one`, `two`, `few`, `many`, `other`); the source catalog defines the categories that English needs (`one` and `other`) and `<base>.other` is required. The tool translates each key of a group as the form for that grammatical number.

The registry can declare English-only coverage with `sourceOnly`: `pages` (exact Markdown paths relative to `docs/en/`), `messages` (exact source keys) and `sections` (exact Markdown paths mapped to section IDs that are delimited by standalone `<!-- i18n:source-only <id> -->` and `<!-- i18n:end -->` lines). Use it only when the owning product contract requires English-only content. Unknown, duplicate or removed declarations fail. With `sourceOnly`, the tool validates absolute document links against the discovered routes.

### Command

The package installs the `delino-i18n` bin. Apps call it from package scripts; account commands also run from the root. Every command prints structured JSON events, and failures print one `localization_failed` event and exit with code 1.

- `delino-i18n check`: validates catalog keys, document coverage, protected syntax and source fingerprints of the app in the current directory. It needs no network access and no credentials, and it never writes.
- `delino-i18n translate [lang]`: generates missing, invalid or source-changed translations of the app in the current directory, for all target languages or for `lang`. `DELINO_TRANSLATION_MODEL` selects a model slug from the account catalog; otherwise the first listed model is used.
- `delino-i18n login [profile-id|--new]`: continues with ChatGPT in the system browser and authorizes ChatGPT plan usage. `--new` adds a separate registration; a profile ID reauthorizes and selects that registration.
- `delino-i18n logout`: revokes the refresh token, clears the local credentials and keeps the host and registration identifiers.
- `delino-i18n models`: lists the models available to the account.
- `delino-i18n profiles`: lists the saved registrations and marks the active one.

The tool treats each catalog value and each Markdown page as a translation unit. One request translates up to 40 catalog values or one Markdown page. Each app runs up to two requests concurrently, and each finished group of requests is saved before the next group starts, so a rerun resumes the remaining work. A failed request stops later groups of that app. A per-app lock directory in the OS temporary directory rejects overlapping generation for one checkout; its error names the lock to remove after an interrupted process has stopped.

The tool keeps unchanged translations, including human-edited ones. It replaces a changed unit only through `translate`. New source text, missing output, obsolete units and invalid translations fail `check`; remove obsolete keys, pages and their state entries explicitly. MDX source pages fail, because the tool cannot protect their executable content. Code spans, fenced code, placeholders, Markdown link destinations, heading levels and non-text frontmatter values must stay unchanged in a translation. The translatable frontmatter fields are `title`, `description`, `name`, `text`, `tagline`, `footer`, `details` and `alt`. The tool prefixes internal absolute document links with `/<lang>` after translation.

### Runtime

- `delino-i18n` exports `Locale`, `LocaleRegistry`, `Messages`, `FormatValues`, `PluralCategory`, `PluralBase`, `interpolate`, `splitPlaceholders`, `matchLanguage`, `isRegistered` and `Translator`.
  - `matchLanguage(registry, requested)` returns the first requested language that matches a registered tag exactly (case-insensitive) or, after `Intl.Locale` maximization, has the same base language and script. For example, `pt-PT` selects `pt-BR` and `zh-CN` selects `zh-Hans`, but `zh-TW` does not select `zh-Hans`. Without a match it returns the source language.
  - `Translator.t(key, values)` returns the catalog text with placeholders replaced; a placeholder without a value stays visible. A key missing from the target catalog falls back to the source catalog. `Translator.plural(base, count, values)` selects the plural category of `count` for the language, falls back to `<base>.other`, and sets `{count}` to the count formatted with `Intl.NumberFormat` unless `values` sets it.
- `delino-i18n/react` exports `createI18n({ registry, source, load, storageKey })`, which returns `I18nProvider`, `useI18n` and `loadInitialLanguage`. Keys are typed by the source catalog.
  - `loadInitialLanguage()` selects the language stored under `storageKey` in `localStorage` when it is registered, otherwise `matchLanguage` with `navigator.languages`. It loads the catalog through `load` and sets `document.documentElement.lang`. When the catalog cannot be loaded, it logs `i18n_catalog_load_failed` and uses the source language. Apps await it before their first render, so the first render shows the selected language.
  - `useI18n()` returns `lang`, `locales`, `switching`, `setLanguage(lang)`, `t`, `plural` and `rich`. `rich(key, values)` replaces placeholders with React nodes for sentences with embedded links or code; the text of an embedded element uses its own key. `setLanguage` loads the catalog, then selects the language, stores it and updates `document.documentElement.lang`; a later selection wins over a slower earlier load, and a failed load keeps the current language and rejects. Outside `I18nProvider`, `useI18n()` returns the source language and `setLanguage` rejects, so unit tests can render one component without the provider.

## Storage

- Committed catalog files in each app (see [Catalog files](#catalog-files)).
- ChatGPT credentials in `~/.config/delino-translations/auth.json`, outside every checkout, written atomically with mode `0600` in a directory with mode `0700`. The file holds a stable host ID and one entry per registration.
- The browser keeps only the selected language tag in `localStorage`. Storage that is unavailable does not prevent language selection for the current page.

## Security

The tool follows the official Sign in with ChatGPT [registration](https://developers.openai.com/siwc/token-sharing-open-source/sign-in), [session](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions) and [inference](https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference) contracts. It requires an eligible ChatGPT plan and the granted `chatgpt.tokens.use.direct` permission; identity sign-in alone is insufficient.

- Sign-in uses a stable installation host ID, dynamic client registration, PKCE (`S256`), a random state and nonce, and a callback listener bound to `127.0.0.1`. A returning sign-in reuses the issued client ID.
- The tool verifies the ID-token signature, issuer (`https://auth.openai.com`), audience, expiration and nonce with `jose`, and rejects a different account for a saved registration.
- A lock directory serializes local account operations, so concurrent processes cannot reuse a rotated refresh token. Translation requests take the lock only to reload the session and refresh the access token; they wait up to 30 seconds. Login, logout and profile operations fail immediately on a conflict. Inference does not keep credentials in a long-lived client.
- Credentials never enter source, browser bundles, logs or CI artifacts. The tool does not read Codex credentials. `check` and app builds never read credentials.
- Inference uses the Responses API with `store: false` and `stream: true`. The tool accepts output only after `response.completed`, and fails without recording the unit on a refusal, an incomplete response, a usage limit or an interrupted stream. The input is the source text only; the tool never sends repository contracts or other files. The instructions tell the model to treat the input as content, never as instructions.
- `translate` shows that generation uses the ChatGPT plan and links `https://chatgpt.com/settings/usage`. No API key, copied OAuth token or hosted translation service is used.

## Logging

The tool prints one JSON object per event: `translations_updated` (`language`, `units`) after each saved batch, `translations_checked` after a passing check and `localization_failed` (`message`) on failure. Messages never contain tokens. The runtime logs `i18n_catalog_load_failed` and `i18n_missing_key` with `console.error`.

## Build and Test

- `pnpm --filter=delino-i18n test`: Rstest runtime tests, then `node --test cli/*.test.mjs` with mocked authentication and inference.
- `pnpm --filter=delino-i18n typecheck`.
- Each participating app runs `delino-i18n check` in its `test` script.

Mocked tests do not establish real sign-in, consent, account model discovery or completed plan-backed inference; record that account acceptance separately.

## Dependencies and Integrations

- Upstream: OpenAI Sign in with ChatGPT (`https://auth.openai.com`) and the Responses and models endpoints under `https://api.openai.com/v1`, called only by the local tool.
- Downstream: the apps listed in the [localization contract](repository-localization-contract.md).

## Change Triggers

Update the [project index](project-delino-i18n.md) when command IDs, catalog paths or invariants change, and the [localization contract](repository-localization-contract.md) when app behavior changes. Update `AGENTS.md` only when development procedures, directory ownership or repository/domain development rules change; follow the [instruction-update policy](README.md#instruction-update-policy).

## References

- [Project index](project-delino-i18n.md)
- [Localization contract](repository-localization-contract.md)
- [Repository defaults](repository-defaults.md)
- [Licensing](repository-license-contract.md)
