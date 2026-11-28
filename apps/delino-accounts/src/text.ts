// SPDX-License-Identifier: Apache-2.0
import { type PluralBase, Translator } from "delino-i18n";
import source from "../i18n/messages/en.json";

/** A key of the source catalog `i18n/messages/en.json`. */
export type MessageKey = Extract<keyof typeof source, string>;

/** The base key of a plural group of the source catalog. */
export type PluralKey = PluralBase<MessageKey>;

/**
 * The part of `useI18n()` that text helpers outside components use: the
 * selected language and the catalog lookups. Components pass the value of
 * `useI18n()`; the default of each helper is `englishText`.
 */
export type Text = Pick<Translator<MessageKey>, "lang" | "t" | "plural">;

/**
 * The source language. Helpers use it when no language is passed, for
 * example in unit tests, so their output stays the English text.
 */
export const englishText: Text = new Translator<MessageKey>(
  "en",
  source,
  source,
);
