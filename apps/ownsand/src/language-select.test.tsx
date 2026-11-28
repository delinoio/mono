// SPDX-License-Identifier: Apache-2.0
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  rs,
  test,
} from "@rstest/core";
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
import { LanguageMenu, LanguageSelect } from "./components/language-select.tsx";
import { API_RESOURCE, AuthMode } from "./config.ts";
import {
  formatByteCount,
  formatTime,
  formatUsageCostUsdMicros,
  formatUsdMicros,
} from "./format.ts";
import { renderConsole } from "./test-support.tsx";

const STORAGE_KEY = "delino.language";

const source = {
  "common.language": "Language",
  "language.failed": "The language could not be loaded. Try again.",
  greeting: "Hello",
};

const korean = {
  "common.language": "언어",
  "language.failed": "언어를 불러오지 못했습니다.",
  greeting: "안녕하세요",
};

/** A provider with a fake catalog loader: Korean loads, French fails. */
function testI18n() {
  return createI18n({
    registry: {
      sourceLang: "en",
      locales: [
        { lang: "en", label: "English" },
        { lang: "ko", label: "한국어" },
        { lang: "fr", label: "Français" },
      ],
    },
    source,
    storageKey: STORAGE_KEY,
    load: async (lang) => {
      if (lang === "ko") return korean;
      throw new Error(`no catalog for ${lang}`);
    },
  });
}

async function renderMenu() {
  const { I18nProvider, useI18n, loadInitialLanguage } = testI18n();
  function Harness() {
    const { lang, locales, setLanguage, t } = useI18n();
    return (
      <>
        <p>{t("greeting")}</p>
        <LanguageMenu
          lang={lang}
          locales={locales}
          setLanguage={setLanguage}
          label={t("common.language")}
          failedMessage={t("language.failed")}
        />
      </>
    );
  }
  const initial = await loadInitialLanguage(["en-US"]);
  render(
    <I18nProvider initial={initial}>
      <Harness />
    </I18nProvider>,
  );
}

async function choose(menuName: string, optionName: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("combobox", { name: menuName }));
  });
  const option = await screen.findByRole("option", { name: optionName });
  await act(async () => {
    fireEvent.click(option);
  });
}

describe("language menu", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  afterEach(() => {
    cleanup();
    localStorage.clear();
    document.documentElement.lang = "en";
    rs.restoreAllMocks();
  });

  test("switches the text without a page load and keeps the selection", async () => {
    await renderMenu();
    expect(screen.getByText("Hello")).toBeTruthy();

    await choose("Language", "한국어");

    await waitFor(() => expect(screen.getByText("안녕하세요")).toBeTruthy());
    expect(screen.queryByText("Hello")).toBeNull();
    expect(screen.getByRole("combobox", { name: "언어" })).toBeTruthy();
    expect(localStorage.getItem(STORAGE_KEY)).toBe("ko");
    expect(document.documentElement.lang).toBe("ko");
  });

  test("keeps the current language and logs when a catalog fails to load", async () => {
    const logged = rs.spyOn(console, "error").mockImplementation(() => {});
    await renderMenu();

    await choose("Language", "Français");

    await waitFor(() =>
      expect(logged).toHaveBeenCalledWith(
        "language_selection_failed",
        expect.objectContaining({ lang: "fr" }),
      ),
    );
    expect(screen.getByText("Hello")).toBeTruthy();
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull();
  });

  test("lists every registered language by its native label", async () => {
    // Outside a provider the console shows the source language.
    render(<LanguageSelect />);
    await act(async () => {
      fireEvent.click(screen.getByRole("combobox", { name: "Language" }));
    });
    const options = await screen.findAllByRole("option");
    expect(options.map((option) => option.textContent)).toEqual(
      registry.locales.map((locale) => locale.label),
    );
    expect(options.map((option) => option.getAttribute("lang"))).toEqual(
      registry.locales.map((locale) => locale.lang),
    );
  });
});

describe("language menu in the app shell", () => {
  afterEach(() => {
    cleanup();
  });

  test("shows on the signed-out page", () => {
    window.history.replaceState(null, "", "/");
    render(
      <App
        config={{
          apiOrigin: API_RESOURCE,
          authMode: AuthMode.Logto,
          logto: undefined,
        }}
      />,
    );
    expect(screen.getByRole("combobox", { name: "Language" })).toBeTruthy();
  });

  test("shows in the top bar of signed-in pages", async () => {
    renderConsole("/");
    expect(
      await screen.findByRole("combobox", { name: "Language" }),
    ).toBeTruthy();
  });
});

describe("formatting for the selected language", () => {
  test("formats USD amounts, byte counts and times with Intl", () => {
    const usd = (lang: string, digits: number) =>
      new Intl.NumberFormat(lang, {
        style: "currency",
        currency: "USD",
        minimumFractionDigits: 2,
        maximumFractionDigits: digits,
      });
    expect(formatUsdMicros(1_234_567_890n, "de")).toBe(
      usd("de", 2).format(1234.57),
    );
    expect(formatUsageCostUsdMicros(5_000n, "fr")).toBe(
      usd("fr", 6).format(0.005),
    );
    expect(formatByteCount(1_536n, "de")).toBe("1,50 KiB");
    const time = { seconds: 0n, nanos: 0 } as Parameters<typeof formatTime>[0];
    expect(formatTime(time, "ko")).toBe(
      new Date(0).toLocaleString("ko", {
        dateStyle: "medium",
        timeStyle: "short",
      }),
    );
  });
});
