// SPDX-License-Identifier: Apache-2.0
import type { Locale } from "delino-i18n";
import { Select, SelectOption } from "delino-ui/select";
import { toast } from "delino-ui/toast";
import { useI18n } from "../i18n.ts";

export interface LanguageMenuProps {
  /** The selected language. */
  lang: string;
  /** The registered languages, in menu order. */
  locales: readonly Locale[];
  /** Loads and selects a language (`useI18n().setLanguage`). */
  setLanguage: (lang: string) => Promise<void>;
  /** The accessible name of the menu. */
  label: string;
  /** The toast text when the catalog of the selected language fails to load. */
  failedMessage: string;
  className?: string;
}

/**
 * The language menu: every registered language by its native label. A
 * selection applies at once, without a page load, and `delino-i18n` keeps it
 * in the browser. See docs/repository-localization-contract.md#react-apps.
 */
export function LanguageMenu({
  lang,
  locales,
  setLanguage,
  label,
  failedMessage,
  className,
}: LanguageMenuProps) {
  return (
    <div
      className={className ? `language-select ${className}` : "language-select"}
    >
      <Select
        label={label}
        value={lang}
        onValueChange={(value) => {
          if (value === null || value === lang) return;
          setLanguage(value).catch((error: unknown) => {
            // The current language stays; the catalog of the selected one
            // did not load (for example, the network is down).
            console.error("language_selection_failed", { lang: value, error });
            toast.error(failedMessage);
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

/** The language menu of the app shell, bound to the console's catalogs. */
export function LanguageSelect({ className }: { className?: string }) {
  const { lang, locales, setLanguage, t } = useI18n();
  return (
    <LanguageMenu
      lang={lang}
      locales={locales}
      setLanguage={setLanguage}
      label={t("common.language")}
      failedMessage={t("language.failed")}
      className={className}
    />
  );
}
