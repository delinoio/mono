// SPDX-License-Identifier: Apache-2.0
import { Translator } from "delino-i18n";
import type { I18n } from "delino-i18n/react";
import registry from "../i18n/locales.json";
import source from "../i18n/messages/en.json";

/** A key of the source catalog `i18n/messages/en.json`. */
export type MessageKey = Extract<keyof typeof source, string>;

/**
 * The translation functions that modules outside components use, such as
 * formatters and form checks. A component passes `useI18n()`; other callers
 * and unit tests use `sourceText`, the source language.
 */
export type Text = Pick<I18n<MessageKey>, "lang" | "t" | "plural">;

/** The source language tag (`en`). */
export const SOURCE_LANG = registry.sourceLang;

const sourceTranslator = new Translator<MessageKey>(
  SOURCE_LANG,
  source,
  source,
);

/** The texts of the source language, for callers outside a component. */
export const sourceText: Text = {
  lang: SOURCE_LANG,
  t: (key, values) => sourceTranslator.t(key, values),
  plural: (base, count, values) => sourceTranslator.plural(base, count, values),
};

/**
 * The text of an enum value from a map of message keys. A value that the
 * map does not know (a newer server enum number) shows the text of the
 * unspecified value 0, which every map has.
 */
export function enumText<E extends number>(
  text: Pick<Text, "t">,
  map: Readonly<Record<E, MessageKey>>,
  value: E,
): string {
  return text.t(map[value] ?? map[0 as E]);
}
