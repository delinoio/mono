// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

// Expected origin after the app removes the trailing slash from the value
// that `e2e.config.ts` passes as PUBLIC_API_ORIGIN.
const E2E_API_ORIGIN = "http://127.0.0.1:28100";

describe("home", () => {
  test("shows the account and billing sections", async ({ app, screen }) => {
    await app.open("/");

    await expect(
      screen.getByRole("heading", "Delino Accounts", { level: 1 }),
    ).toBeVisible();
    await expect(
      screen.getByRole("heading", "Account", { level: 2 }),
    ).toBeVisible();
    await expect(
      screen.getByRole("heading", "Billing", { level: 2 }),
    ).toBeVisible();
  });

  test("exposes the normalized API origin on the document", async ({
    app,
    browser,
  }) => {
    await app.open("/");

    await expect(browser.locator("html")).toHaveAttribute(
      "data-api-origin",
      E2E_API_ORIGIN,
    );
  });

  test("links the billing section to the billing pages", async ({
    app,
    screen,
  }) => {
    await app.open("/");

    await expect(screen.getByRole("link", "Manage billing")).toHaveAttribute(
      "href",
      "/billing",
    );
  });

  test("offers every registered language in the top bar while signed out", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");

    await expect(browser.locator("html")).toHaveAttribute("lang", "en");
    const menu = screen.getByRole("combobox", "Language");
    await expect(menu).toHaveText("English");
    await menu.click();
    for (const [lang, label] of [
      ["en", "English"],
      ["ko", "한국어"],
      ["ja", "日本語"],
      ["zh-Hans", "简体中文"],
      ["es", "Español"],
      ["fr", "Français"],
      ["de", "Deutsch"],
      ["pt-BR", "Português (Brasil)"],
    ]) {
      await expect(screen.getByRole("option", label)).toHaveAttribute(
        "lang",
        lang!,
      );
    }
  });

  test(
    "marks only the Account section as coming soon",
    { tags: ["agent"] },
    async ({ app, agent }) => {
      await app.open("/");

      await agent.assert(
        "the Account section is marked as coming soon, and the Billing section offers a link to manage billing instead",
      );
    },
  );
});

describe("billing", () => {
  test("offers sign-in to a signed-out user on a direct billing route", async ({
    app,
    screen,
  }) => {
    await app.open("/billing");

    await expect(
      screen.getByRole("heading", "Sign in to manage billing", { level: 1 }),
    ).toBeVisible();
    await expect(
      screen.getByRole("button", "Sign in with Delino"),
    ).toBeVisible();
  });
});
