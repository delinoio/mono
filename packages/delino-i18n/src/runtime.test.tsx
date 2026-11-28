// SPDX-License-Identifier: Apache-2.0
import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, describe, expect, it } from "@rstest/core";
import { createI18n } from "./react.tsx";
import {
  interpolate,
  type LocaleRegistry,
  matchLanguage,
  Translator,
} from "./runtime.ts";

// Tell React that this environment supports `act`.
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });

type Key = keyof typeof source;

const registry: LocaleRegistry = {
  sourceLang: "en",
  locales: [
    { lang: "en", label: "English" },
    { lang: "ko", label: "한국어" },
    { lang: "zh-Hans", label: "简体中文" },
    { lang: "pt-BR", label: "Português (Brasil)" },
    { lang: "fr", label: "Français" },
  ],
};

const source = {
  greeting: "Hello {name}",
  "items.one": "{count} item",
  "items.other": "{count} items",
  terms: "Read the {link} first.",
  termsLink: "terms",
} as const;

const korean = {
  greeting: "안녕하세요 {name}",
  "items.other": "항목 {count}개",
  terms: "먼저 {link}을 읽으세요.",
  termsLink: "약관",
};

describe("matchLanguage", () => {
  it("prefers an exact match, then the same language and script", () => {
    expect(matchLanguage(registry, ["ko-KR"])).toBe("ko");
    expect(matchLanguage(registry, ["ZH-hans"])).toBe("zh-Hans");
    expect(matchLanguage(registry, ["zh-CN"])).toBe("zh-Hans");
    expect(matchLanguage(registry, ["pt-PT"])).toBe("pt-BR");
    expect(matchLanguage(registry, ["fr-CA", "ko"])).toBe("fr");
  });

  it("does not match another script and falls back to the source", () => {
    expect(matchLanguage(registry, ["zh-TW"])).toBe("en");
    expect(matchLanguage(registry, ["zh-TW", "ko"])).toBe("ko");
    expect(matchLanguage(registry, ["not a tag", "", null])).toBe("en");
    expect(matchLanguage(registry, [])).toBe("en");
  });
});

describe("Translator", () => {
  it("replaces known placeholders and keeps unknown ones", () => {
    expect(interpolate("Hi {name} {missing}", { name: "Ada" })).toBe(
      "Hi Ada {missing}",
    );
  });

  it("falls back to the source text for a missing key", () => {
    const translator = new Translator<Key>(
      "ko",
      { greeting: "안녕 {name}" },
      source,
    );
    expect(translator.t("greeting", { name: "Ada" })).toBe("안녕 Ada");
    expect(translator.t("termsLink")).toBe("terms");
  });

  it("selects plural categories and falls back to other", () => {
    const english = new Translator<Key>("en", source, source);
    expect(english.plural("items", 1)).toBe("1 item");
    expect(english.plural("items", 1200)).toBe("1,200 items");
    const french = new Translator<Key>(
      "fr",
      { "items.one": "{count} élément", "items.other": "{count} éléments" },
      source,
    );
    expect(french.plural("items", 0)).toBe("0 élément");
    const korean = new Translator<Key>(
      "ko",
      { "items.other": "{count}개" },
      source,
    );
    expect(korean.plural("items", 1)).toBe("1개");
  });
});

describe("createI18n", () => {
  let container: HTMLDivElement | undefined;
  afterEach(() => {
    container?.remove();
    container = undefined;
    localStorage.clear();
  });

  function setup() {
    const loads: string[] = [];
    const i18n = createI18n({
      registry,
      source,
      storageKey: "test.language",
      load: async (lang) => {
        loads.push(lang);
        if (lang === "ko") return korean;
        throw new Error("missing catalog");
      },
    });
    return { i18n, loads };
  }

  it("loads the stored language before the browser languages", async () => {
    const { i18n, loads } = setup();
    expect((await i18n.loadInitialLanguage(["ko"])).lang).toBe("ko");
    localStorage.setItem("test.language", "ko");
    const initial = await i18n.loadInitialLanguage(["fr"]);
    expect(initial.lang).toBe("ko");
    expect(document.documentElement.lang).toBe("ko");
    localStorage.setItem("test.language", "xx");
    // `fr` fails to load and falls back to the source language.
    expect(await i18n.loadInitialLanguage(["fr"])).toEqual({
      lang: "en",
      messages: source,
    });
    expect(loads).toEqual(["ko", "ko", "fr"]);
  });

  it("renders, switches and keeps the language", async () => {
    const { i18n } = setup();
    let api: ReturnType<typeof i18n.useI18n> | undefined;
    function Probe() {
      api = i18n.useI18n();
      return (
        <p>
          {api.t("greeting", { name: "Ada" })} {api.plural("items", 2)}{" "}
          {api.rich("terms", {
            link: <a href="/terms">{api.t("termsLink")}</a>,
          })}
        </p>
      );
    }
    container = document.createElement("div");
    document.body.append(container);
    const root = createRoot(container);
    await act(async () => {
      root.render(
        <i18n.I18nProvider initial={{ lang: "en", messages: source }}>
          <Probe />
        </i18n.I18nProvider>,
      );
    });
    expect(container.textContent).toBe(
      "Hello Ada 2 items Read the terms first.",
    );
    expect(container.querySelector("a")?.textContent).toBe("terms");

    await act(() => api!.setLanguage("ko"));
    expect(container.textContent).toBe(
      "안녕하세요 Ada 항목 2개 먼저 약관을 읽으세요.",
    );
    expect(localStorage.getItem("test.language")).toBe("ko");
    expect(document.documentElement.lang).toBe("ko");

    await act(async () => {
      await expect(api!.setLanguage("fr")).rejects.toThrow("missing catalog");
    });
    expect(api!.lang).toBe("ko");
    expect(api!.switching).toBe(false);
    await expect(api!.setLanguage("xx")).rejects.toThrow(
      "Unregistered language",
    );
    await act(async () => root.unmount());
  });

  it("uses the source language outside a provider", () => {
    const { i18n } = setup();
    function Probe() {
      return <p>{i18n.useI18n().t("greeting", { name: "Ada" })}</p>;
    }
    container = document.createElement("div");
    const root = createRoot(container);
    act(() => root.render(<Probe />));
    expect(container.textContent).toBe("Hello Ada");
    act(() => root.unmount());
  });
});
