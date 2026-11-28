// SPDX-License-Identifier: Apache-2.0
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
