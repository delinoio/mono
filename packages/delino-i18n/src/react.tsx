// SPDX-License-Identifier: Apache-2.0
import {
  createContext,
  Fragment,
  type ReactNode,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
} from "react";
import {
  type FormatValues,
  isRegistered,
  type Locale,
  type LocaleRegistry,
  matchLanguage,
  type Messages,
  type PluralBase,
  splitPlaceholders,
  Translator,
} from "./runtime.ts";

export interface I18nOptions<M extends Messages> {
  /** The app's `i18n/locales.json`. */
  readonly registry: LocaleRegistry;
  /** The source catalog, imported statically so it is always available. */
  readonly source: M;
  /**
   * Loads the catalog of a target language. Apps import each catalog
   * dynamically, so a page downloads only the selected language.
   */
  readonly load: (lang: string) => Promise<Messages>;
  /** The `localStorage` key that keeps an explicit language selection. */
  readonly storageKey: string;
}

/** The language state that the app loads before its first render. */
export interface InitialLanguage {
  readonly lang: string;
  readonly messages: Messages;
}

export interface I18n<K extends string> {
  /** The selected language. */
  readonly lang: string;
  /** The registered languages, in registry order. */
  readonly locales: readonly Locale[];
  /** True while the catalog of a newly selected language loads. */
  readonly switching: boolean;
  /** Loads and selects a language, and keeps the selection. */
  setLanguage(lang: string): Promise<void>;
  /** Returns the text of `key` with its placeholders replaced. */
  t(key: K, values?: FormatValues): string;
  /** Returns the text of the plural group `base` for `count`. */
  plural(base: PluralBase<K>, count: number, values?: FormatValues): string;
  /**
   * Returns the text of `key` with placeholders replaced by React nodes, for
   * sentences that contain links, code or emphasis. Translate the text of an
   * embedded element with a separate key.
   */
  rich(key: K, values: Readonly<Record<string, ReactNode>>): ReactNode;
}

function readStoredLanguage(storageKey: string): string | null {
  try {
    return globalThis.localStorage?.getItem(storageKey) ?? null;
  } catch {
    // Storage can be unavailable, for example in a private window.
    return null;
  }
}

function storeLanguage(storageKey: string, lang: string) {
  try {
    globalThis.localStorage?.setItem(storageKey, lang);
  } catch {
    // The selection still applies to this page; it is not kept.
  }
}

function applyDocumentLanguage(lang: string) {
  if (typeof document !== "undefined") document.documentElement.lang = lang;
}

/**
 * Creates the language provider and hook of one app. Keys are typed by the
 * source catalog, so an unknown key fails the type check.
 */
export function createI18n<M extends Messages>(options: I18nOptions<M>) {
  type K = Extract<keyof M, string>;
  const { registry, source, load, storageKey } = options;

  function bind(
    current: InitialLanguage,
    switching: boolean,
    setLanguage: (lang: string) => Promise<void>,
  ): I18n<K> {
    const translator = new Translator<K>(
      current.lang,
      current.messages,
      source,
    );
    return {
      lang: current.lang,
      locales: registry.locales,
      switching,
      setLanguage,
      t: (key, values) => translator.t(key, values),
      plural: (base, count, values) => translator.plural(base, count, values),
      rich: (key, values) =>
        splitPlaceholders(translator.text(key)).map((part, index) =>
          index % 2 === 0 ? (
            part
          ) : (
            <Fragment key={index}>
              {Object.hasOwn(values, part) ? values[part] : `{${part}}`}
            </Fragment>
          ),
        ),
    };
  }

  // Outside a provider, for example in a unit test that renders one
  // component, components use the source language and cannot switch.
  const Context = createContext<I18n<K>>(
    bind({ lang: registry.sourceLang, messages: source }, false, async () => {
      throw new Error("Language selection requires I18nProvider.");
    }),
  );

  async function catalog(lang: string): Promise<Messages> {
    return lang === registry.sourceLang ? source : load(lang);
  }

  /**
   * Selects the stored language, otherwise the best match for the browser
   * languages, and loads its catalog. When the catalog cannot be loaded, the
   * source language is used.
   */
  async function loadInitialLanguage(
    requested: readonly string[] = typeof navigator === "undefined"
      ? []
      : navigator.languages,
  ): Promise<InitialLanguage> {
    const stored = readStoredLanguage(storageKey);
    const lang =
      stored && isRegistered(registry, stored)
        ? stored
        : matchLanguage(registry, requested);
    try {
      const messages = await catalog(lang);
      applyDocumentLanguage(lang);
      return { lang, messages };
    } catch (error) {
      console.error("i18n_catalog_load_failed", { lang, error });
      applyDocumentLanguage(registry.sourceLang);
      return { lang: registry.sourceLang, messages: source };
    }
  }

  function I18nProvider({
    initial,
    children,
  }: {
    initial: InitialLanguage;
    children?: ReactNode;
  }) {
    const [current, setCurrent] = useState(initial);
    const [switching, setSwitching] = useState(false);
    const latest = useRef(0);

    const setLanguage = useCallback(async (lang: string) => {
      if (!isRegistered(registry, lang))
        throw new Error(`Unregistered language: ${lang}`);
      const request = ++latest.current;
      setSwitching(true);
      try {
        const messages = await catalog(lang);
        // A later selection wins over a slower earlier load.
        if (request !== latest.current) return;
        storeLanguage(storageKey, lang);
        applyDocumentLanguage(lang);
        setCurrent({ lang, messages });
      } finally {
        if (request === latest.current) setSwitching(false);
      }
    }, []);

    const value = useMemo(
      () => bind(current, switching, setLanguage),
      [current, switching, setLanguage],
    );

    return <Context value={value}>{children}</Context>;
  }

  function useI18n(): I18n<K> {
    return useContext(Context);
  }

  return { I18nProvider, useI18n, loadInitialLanguage };
}
