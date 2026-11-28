// SPDX-License-Identifier: Apache-2.0

/** One registered language: a BCP 47 tag and its native-language label. */
export interface Locale {
  readonly lang: string;
  readonly label: string;
}

/** The contents of an app's `i18n/locales.json`. */
export interface LocaleRegistry {
  readonly sourceLang: string;
  readonly locales: readonly Locale[];
}

/** A flat text catalog: `i18n/messages/<lang>.json`. */
export type Messages = Readonly<Record<string, string>>;

/** Values for `{name}` placeholders in a catalog text. */
export type FormatValues = Readonly<Record<string, string | number>>;

/** Plural categories that `Intl.PluralRules` can select. */
export enum PluralCategory {
  Zero = "zero",
  One = "one",
  Two = "two",
  Few = "few",
  Many = "many",
  Other = "other",
}

/**
 * The base keys of plural message groups. A group is the keys
 * `<base>.one`, `<base>.other` and other plural categories. Every group must
 * have a `<base>.other` key, which is the fallback.
 */
export type PluralBase<K extends string> = K extends `${infer B}.other`
  ? B
  : never;

const placeholder = /\{([A-Za-z_][\w.]*)\}/gu;

/**
 * Replaces each `{name}` placeholder with its value. A placeholder without a
 * value stays unchanged, so a missing value is visible instead of silently
 * removed.
 */
export function interpolate(template: string, values?: FormatValues): string {
  if (!values) return template;
  return template.replace(placeholder, (match, name: string) =>
    Object.hasOwn(values, name) ? String(values[name]) : match,
  );
}

/**
 * Splits a template into text and placeholder names. Even indexes are text;
 * odd indexes are placeholder names.
 */
export function splitPlaceholders(template: string): string[] {
  return template.split(/\{([A-Za-z_][\w.]*)\}/u);
}

function parse(tag: string): Intl.Locale | undefined {
  try {
    return new Intl.Locale(tag).maximize();
  } catch {
    return undefined;
  }
}

/**
 * Selects the registered language that best matches the requested languages,
 * in order of preference. An exact tag match wins. Otherwise a language with
 * the same base language and script matches, so `pt-PT` selects `pt-BR` and
 * `zh-CN` selects `zh-Hans`, but `zh-TW` (Traditional script) does not select
 * `zh-Hans`. When nothing matches, the source language is selected.
 */
export function matchLanguage(
  registry: LocaleRegistry,
  requested: readonly (string | null | undefined)[],
): string {
  for (const tag of requested) {
    if (!tag) continue;
    const exact = registry.locales.find(
      ({ lang }) => lang.toLowerCase() === tag.toLowerCase(),
    );
    if (exact) return exact.lang;
    const wanted = parse(tag);
    if (!wanted) continue;
    const close = registry.locales.find(({ lang }) => {
      const candidate = parse(lang);
      return (
        candidate?.language === wanted.language &&
        candidate.script === wanted.script
      );
    });
    if (close) return close.lang;
  }
  return registry.sourceLang;
}

/** Returns true when `lang` is a registered language. */
export function isRegistered(registry: LocaleRegistry, lang: string): boolean {
  return registry.locales.some((locale) => locale.lang === lang);
}

/**
 * Looks up and formats catalog texts for one language. A key missing from the
 * target catalog falls back to the source catalog. The offline check
 * (`delino-i18n check`) rejects incomplete catalogs before a build, so the
 * fallback only covers a catalog that is still loading or stale.
 */
export class Translator<K extends string = string> {
  readonly #rules: Intl.PluralRules;

  constructor(
    readonly lang: string,
    readonly messages: Messages,
    readonly source: Messages,
  ) {
    this.#rules = new Intl.PluralRules(lang);
  }

  /** Returns the raw catalog text of `key`. */
  text(key: K): string {
    const value = this.messages[key] ?? this.source[key];
    if (value === undefined) {
      // A missing source key is a programming error; show the key.
      console.error("i18n_missing_key", { key, lang: this.lang });
      return key;
    }
    return value;
  }

  /** Returns the text of `key` with its placeholders replaced. */
  t(key: K, values?: FormatValues): string {
    return interpolate(this.text(key), values);
  }

  /**
   * Returns the text of the plural group `base` for `count`. The `{count}`
   * placeholder receives the count formatted for the language.
   */
  plural(base: PluralBase<K>, count: number, values?: FormatValues): string {
    const category = this.#rules.select(count);
    const key = `${base}.${category}`;
    const fallback = `${base}.${PluralCategory.Other}`;
    const chosen = (
      Object.hasOwn(this.messages, key) || Object.hasOwn(this.source, key)
        ? key
        : fallback
    ) as K;
    return this.t(chosen, {
      count: new Intl.NumberFormat(this.lang).format(count),
      ...values,
    });
  }
}
