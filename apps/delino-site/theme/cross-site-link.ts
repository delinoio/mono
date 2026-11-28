// SPDX-License-Identifier: Apache-2.0
import registry from "../i18n/locales.json" with { type: "json" };

// The Delino Docs origin. Delino Docs registers the same languages as this
// site at the same `/<lang>/` route prefixes (see
// `docs/repository-localization-contract.md`), so a link to it keeps the
// language of the current page.
const SIBLING_ORIGINS: readonly string[] = ["https://docs.delino.io"];

const isRegistered = (lang: string): boolean =>
  registry.locales.some((locale) => locale.lang === lang);

// Returns `href` with the `/<lang>` route prefix when it points to a sibling
// Delino site and `lang` is a registered target language. English hrefs,
// other hrefs and hrefs that already start with a language prefix stay
// unchanged, so applying the function twice gives the same result.
//
// Page frontmatter keeps external URLs byte-for-byte in every translation
// (the translation tool rejects a changed external URL), so a translated page
// cannot carry its own sibling-site URL. `rspress.config.ts` applies this to
// the per-language navigation, and the theme `Link` applies it to every other
// link, for example the home hero actions.
export function localizeCrossSiteHref(href: string, lang: string): string {
  if (lang === registry.sourceLang || !isRegistered(lang)) {
    return href;
  }
  let url: URL;
  try {
    url = new URL(href);
  } catch {
    // A relative or internal href is not a cross-site link.
    return href;
  }
  if (!SIBLING_ORIGINS.includes(url.origin)) {
    return href;
  }
  const [, first = ""] = url.pathname.split("/");
  if (isRegistered(first)) {
    return href;
  }
  url.pathname = `/${lang}${url.pathname}`;
  return url.toString();
}
