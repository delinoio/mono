// SPDX-License-Identifier: Apache-2.0
import type { I18n } from "delino-i18n/react";
import { Select, SelectOption } from "delino-ui/select";
import { useI18n } from "../i18n.ts";

/** The language state that the menu reads and changes. */
export type LanguageMenuState = Pick<
  I18n<"common.language">,
  "lang" | "locales" | "switching" | "setLanguage" | "t"
>;

/**
 * Lists each registered language by its native label. A selection applies
 * without a page reload and is kept in the browser
 * (docs/repository-localization-contract.md#react-apps).
 */
export function LanguageMenu({ i18n }: { i18n: LanguageMenuState }) {
  const { lang, locales, switching, setLanguage, t } = i18n;
  return (
    <div className="language-select">
      <Select
        label={t("common.language")}
        value={lang}
        disabled={switching}
        onValueChange={(value) => {
          if (value === null || value === lang) return;
          setLanguage(value).catch((error: unknown) => {
            console.error("language_selection_failed", { lang: value, error });
          });
        }}
      >
        {locales.map((locale) => (
          <SelectOption
            key={locale.lang}
            value={locale.lang}
            label={locale.label}
            lang={locale.lang}
          >
            {locale.label}
          </SelectOption>
        ))}
      </Select>
    </div>
  );
}

/** The language menu of the top bars, on signed-in and signed-out pages. */
export function LanguageSelect() {
  return <LanguageMenu i18n={useI18n()} />;
}
