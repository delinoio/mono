// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { createI18n } from "delino-i18n/react";
import registry from "../i18n/locales.json";
import { App } from "./App.tsx";
import { LanguageMenu } from "./components/language-select.tsx";
import { AuthMode, DEFAULT_API_ORIGIN } from "./config.ts";
import { labels } from "./format.ts";
import sourceCatalog from "../i18n/messages/en.json";

const STORAGE_KEY = "delino.language";

// A small fake catalog pair, so the test does not depend on generated
// translations.
const source = {
  "common.language": "Language",
  "sample.greeting": "Welcome",
};
const korean = {
  "common.language": "언어",
  "sample.greeting": "환영합니다",
};

function setup() {
  const loads: string[] = [];
  const { I18nProvider, useI18n, loadInitialLanguage } = createI18n({
    registry,
    source,
    storageKey: STORAGE_KEY,
    load: async (lang) => {
      loads.push(lang);
      if (lang === "ko") return korean;
      throw new Error(`No fake catalog for ${lang}`);
    },
  });
  function Page() {
    const i18n = useI18n();
    return (
      <>
        <p>{i18n.t("sample.greeting")}</p>
        <LanguageMenu i18n={i18n} />
      </>
    );
  }
  return { I18nProvider, Page, loadInitialLanguage, loads };
}

async function click(element: HTMLElement) {
  await act(async () => {
    fireEvent.click(element);
  });
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  cleanup();
  localStorage.clear();
});

describe("language menu", () => {
  test("lists every registered language by its native label", async () => {
    const { I18nProvider, Page, loadInitialLanguage } = setup();
    const initial = await loadInitialLanguage(["en-US"]);
    render(
      <I18nProvider initial={initial}>
        <Page />
      </I18nProvider>,
    );
    const menu = screen.getByRole("combobox", { name: "Language" });
    expect(menu.textContent).toContain("English");
    await click(menu);
    for (const locale of registry.locales) {
      const option = await screen.findByRole("option", { name: locale.label });
      expect(option.getAttribute("lang")).toBe(locale.lang);
    }
  });

  test("applies a selection without a reload and keeps it", async () => {
    const { I18nProvider, Page, loadInitialLanguage, loads } = setup();
    const initial = await loadInitialLanguage(["en-US"]);
    render(
      <I18nProvider initial={initial}>
        <Page />
      </I18nProvider>,
    );
    expect(screen.getByText("Welcome")).toBeTruthy();

    await click(screen.getByRole("combobox", { name: "Language" }));
    await click(await screen.findByRole("option", { name: "한국어" }));

    await waitFor(() => expect(screen.getByText("환영합니다")).toBeTruthy());
    const menu = screen.getByRole("combobox", { name: "언어" });
    expect(menu.textContent).toContain("한국어");
    expect(loads).toEqual(["ko"]);
    expect(localStorage.getItem(STORAGE_KEY)).toBe("ko");
    expect(document.documentElement.lang).toBe("ko");
  });

  test("keeps the current language when a catalog cannot load", async () => {
    const { I18nProvider, Page, loadInitialLanguage } = setup();
    const initial = await loadInitialLanguage(["en-US"]);
    const errors: unknown[][] = [];
    const consoleError = console.error;
    console.error = (...args: unknown[]) => {
      errors.push(args);
    };
    try {
      render(
        <I18nProvider initial={initial}>
          <Page />
        </I18nProvider>,
      );
      await click(screen.getByRole("combobox", { name: "Language" }));
      await click(await screen.findByRole("option", { name: "日本語" }));
      await waitFor(() =>
        expect(
          errors.some(([event]) => event === "language_selection_failed"),
        ).toBe(true),
      );
    } finally {
      console.error = consoleError;
    }
    expect(screen.getByText("Welcome")).toBeTruthy();
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull();
  });

  test("appears on the signed-out page", () => {
    render(
      <App
        config={{
          apiOrigin: DEFAULT_API_ORIGIN,
          authMode: AuthMode.Logto,
          logto: undefined,
        }}
      />,
    );
    expect(screen.getByRole("heading", { name: "SandCrew" })).toBeTruthy();
    expect(
      screen.getByRole("combobox", { name: "Language" }).textContent,
    ).toContain("English");
  });
});

describe("source catalog", () => {
  test("has the text of every enum label", () => {
    for (const group of Object.values(labels)) {
      for (const key of Object.values(group)) {
        expect(sourceCatalog).toHaveProperty([key]);
      }
    }
  });
});
