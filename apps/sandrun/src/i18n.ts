// SPDX-License-Identifier: Apache-2.0
import { type FormatValues, Translator } from "delino-i18n";
import { createI18n } from "delino-i18n/react";
import registry from "../i18n/locales.json";
import source from "../i18n/messages/en.json";

// See docs/repository-localization-contract.md#react-apps.
export const { I18nProvider, useI18n, loadInitialLanguage } = createI18n({
  registry,
  source,
  storageKey: "delino.language",
  // Each target catalog is a separate chunk that loads only when selected.
  load: async (lang) => (await import(`../i18n/messages/${lang}.json`)).default,
});

/** A key of the source catalog. */
export type MessageKey = Extract<keyof typeof source, string>;

/**
 * Returns the text of a catalog key in the selected language. Modules that
 * produce text outside React (validation, formatting) take this function
 * from `useI18n()` of their caller.
 */
export type Translate = (key: MessageKey, values?: FormatValues) => string;

/** The translation state that text-producing helpers need. */
export interface Localizer {
  /** The selected language, for `Intl` formatting. */
  readonly lang: string;
  readonly t: Translate;
}

const sourceTranslator = new Translator<MessageKey>(
  registry.sourceLang,
  source,
  source,
);

/**
 * The source language translator, for unit tests of helpers that take a
 * `Translate` function. Components use `useI18n()` instead.
 */
export const sourceLocalizer: Localizer = {
  lang: registry.sourceLang,
  t: (key, values) => sourceTranslator.t(key, values),
};
