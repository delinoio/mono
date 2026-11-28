// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from "node:fs";
import * as path from "node:path";
import { defineConfig } from "@rspress/core";
import registry from "./i18n/locales.json" with { type: "json" };
import { localizeCrossSiteHref } from "./theme/cross-site-link";

// `pnpm dev` runs this server through portless (`dev` script), which assigns
// a free loopback port in `PORT` and sets `PORTLESS_URL`. Read `PORT` only in
// that case, so a `PORT` exported by the shell does not move the fixed port of
// `dev:app`. `rspress preview` reads `PORT` and `HOST` itself, before this
// configuration, so the `preview` script runs through
// `scripts/dev/rspress-preview.mjs`, which removes both variables.
const devPort =
  process.env.PORTLESS_URL && process.env.PORT
    ? Number(process.env.PORT)
    : 13600;

type Messages = Record<string, string>;

// Reads `i18n/messages/<lang>.json`, or returns `undefined` when the catalog
// does not exist yet.
const readCatalog = (lang: string): Messages | undefined => {
  try {
    return JSON.parse(
      readFileSync(path.join(__dirname, "i18n", "messages", `${lang}.json`), {
        encoding: "utf8",
      }),
    ) as Messages;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return undefined;
    }
    throw error;
  }
};

const source = readCatalog(registry.sourceLang);
if (source === undefined) {
  throw new Error(
    `Missing source catalog i18n/messages/${registry.sourceLang}.json`,
  );
}

// `delino-i18n translate` generates the target catalogs. A target text that
// is not generated yet falls back to the English source text, so the site
// still builds before generation. The `test` script (`delino-i18n check`)
// fails on every missing or stale translation, so CI does not accept the
// fallback.
const catalogs = registry.locales.map(({ lang, label }) => ({
  lang,
  label,
  messages: { ...source, ...readCatalog(lang) },
}));

const THEME_PREFIX = "theme.";

export default defineConfig({
  root: path.join(__dirname, "docs"),
  // Keep build output in the repository-wide ignored `dist` directory.
  outDir: "dist",
  base: "/",
  builderConfig: {
    server: {
      // Loopback port that `devPort` selects for `rspress dev` and `rspress
      // preview`; fail instead of picking another port on conflict. The
      // Rspress CLI has no strict-port flag, so only this config can express
      // the rule. The e2e suite overrides it with explicit `--host` and
      // `--port` flags.
      host: "127.0.0.1",
      port: devPort,
      strictPort: true,
    },
  },
  // English pages live in `docs/en/` and keep their unprefixed routes; the
  // other registered languages use `docs/<lang>/` and `/<lang>/`.
  lang: registry.sourceLang,
  locales: catalogs.map(({ lang, label }) => ({
    lang,
    label,
    title: "Delino",
    description: "Delino",
  })),
  // Language selection is explicit: never redirect by browser language.
  route: { localeRedirect: "never" },
  // Default theme texts come from the `theme.*` catalog entries, so every
  // registered language gets translated theme UI (Rspress does not ship all
  // of them). A default text without a source catalog entry, or a catalog
  // entry that the pinned Rspress does not define, fails the build, so a
  // Rspress upgrade cannot add untranslated theme text or leave stale keys.
  i18nSource: (defaults) => {
    const names = Object.keys(source)
      .filter((key) => key.startsWith(THEME_PREFIX))
      .map((key) => key.slice(THEME_PREFIX.length));
    for (const name of Object.keys(defaults)) {
      if (!names.includes(name)) {
        throw new Error(
          `Missing theme text ${THEME_PREFIX}${name} in i18n/messages/${registry.sourceLang}.json`,
        );
      }
    }
    for (const name of names) {
      if (!(name in defaults)) {
        throw new Error(
          `Unknown theme text ${THEME_PREFIX}${name} in i18n/messages/${registry.sourceLang}.json`,
        );
      }
      for (const { lang, messages } of catalogs) {
        defaults[name] = {
          ...defaults[name],
          [lang]: messages[`${THEME_PREFIX}${name}`],
        };
      }
    }
    return defaults;
  },
  title: "Delino",
  description: "Delino",
  icon: "/icon.svg",
  logo: "/icon.svg",
  logoText: "Delino",
  // The marketing site has no documentation search; docs.delino.io owns it.
  // This type-valid option still stops Rspress from indexing pages for search,
  // but it does not hide the search UI; see `themeConfig.search` below.
  search: false,
  sitemap: { siteUrl: "https://delino.io" },
  themeConfig: {
    // Documents the intent; the visible search UI is removed by the `Search`
    // override in `theme/index.tsx` until the default theme honours this flag.
    search: false,
    // Navigation per language, with texts from that language's catalog. The
    // Docs link opens Delino Docs in the same language.
    locales: catalogs.map(({ lang, label, messages }) => ({
      lang,
      label,
      title: "Delino",
      description: "Delino",
      nav: [
        {
          text: messages["nav.docs"],
          link: localizeCrossSiteHref("https://docs.delino.io", lang),
        },
        { text: "GitHub", link: "https://github.com/delinoio" },
      ],
    })),
  },
});
