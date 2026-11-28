# Project: delino-i18n

## Goal

Provide one localization toolchain for Delino apps. Each app keeps its English source text in a text catalog. A maintainer generates the translations locally with the maintainer's ChatGPT plan through [Sign in with ChatGPT](https://developers.openai.com/cookbook/articles/sign-in-with-chatgpt), reviews them and commits them. An offline check keeps the committed translations complete and current without network access or credentials. React apps use the package runtime to select a language and show the catalog text.

## Project ID

`delino-i18n`

## Domain Ownership Map

- `packages/delino-i18n/`: workspace package `delino-i18n` with the browser runtime (`src/`) and the maintainer command-line tool (`cli/`).

The project is a library and a local maintainer tool, not a deployable. It has no `apps`, `cmds`, `servers`, `crates` or `protos` paths. The apps that use it own their catalogs; see the [localization contract](repository-localization-contract.md).

## Domain Contract Documents

- [packages-delino-i18n-foundation.md](packages-delino-i18n-foundation.md)

## Cross-Domain Invariants

- Command IDs of the `delino-i18n` command: `check`, `translate`, `login`, `logout`, `models`, `profiles`.
- Catalog paths in each app: `i18n/locales.json`, `i18n/messages/<lang>.json`, `i18n/state.json`, and for Rspress apps `docs/<lang>/`.
- Language tags are BCP 47 tags. The source language is `en`.
- Placeholders in catalog texts use `{name}`. Plural groups use the key suffixes `.zero`, `.one`, `.two`, `.few`, `.many` and `.other`; `.other` is required.
- Browser bundles never contain ChatGPT credentials or call ChatGPT. Translation is a local maintainer operation.
- Deviation from repository defaults: TypeScript and JavaScript instead of Go, because the runtime runs in React web apps and the tool runs in the Node.js workspace toolchain.

## Change Policy

Update the [package foundation contract](packages-delino-i18n-foundation.md) with any change to the runtime API, the command, the catalog format or the ChatGPT integration. Update the [localization contract](repository-localization-contract.md) when the registered languages, the participating apps or the language selection behavior change. Update `AGENTS.md` only when development procedures, directory ownership or repository/domain development rules change; follow the [instruction-update policy](README.md#instruction-update-policy). Record new implementation status, validation results and unresolved limits in pull requests, Linear issues and CI logs/artifacts.

## References

- [Localization contract](repository-localization-contract.md)
- [Package foundation contract](packages-delino-i18n-foundation.md)
- [Repository defaults](repository-defaults.md)
- [Repository workflow](repository-workflow-contract.md)
