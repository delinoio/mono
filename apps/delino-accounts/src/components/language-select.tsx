// SPDX-License-Identifier: Apache-2.0
import { Select, SelectOption } from "delino-ui/select";
import { useI18n } from "../i18n.ts";

/**
 * The language menu of the top bar. It lists every registered language by
 * its native label, and a selection applies at once and is kept in the
 * browser (docs/repository-localization-contract.md#react-apps).
 */
export function LanguageSelect() {
  const { lang, locales, setLanguage, t } = useI18n();
  return (
    <div className="topbar-language">
      <Select
        // The trigger shows the selected language, so the field label is
        // only the accessible name.
        label={<span className="visually-hidden">{t("common.language")}</span>}
        value={lang}
        onValueChange={(next) => {
          if (next === null || next === lang) return;
          setLanguage(next).catch((error: unknown) => {
            console.error("language_selection_failed", { lang: next, error });
          });
        }}
      >
        {locales.map((locale) => (
          <SelectOption
            key={locale.lang}
            value={locale.lang}
            lang={locale.lang}
          >
            {locale.label}
          </SelectOption>
        ))}
      </Select>
    </div>
  );
}
