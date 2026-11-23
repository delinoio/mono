// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

// IDs from src/mock/mock-api.ts. The unknown organization is not seeded.
const ACME = "0193a000-0000-7000-8000-000000000001";
const NORTHWIND = "0193a000-0000-7000-8000-000000000002";
const UNKNOWN_ORG = "0193a000-0000-7000-8000-0000000000ff";
const UNKNOWN_DEVICE = "0193a000-0000-7000-8000-0000000000fe";

describe("failed pages", () => {
  test("an unknown organization offers a way out and the switcher placeholder", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(
      screen.getByRole("heading", "Organizations", { level: 1 }),
    ).toBeVisible();

    // Client-side navigation, as the routing test does.
    await browser.evaluate((path: string) => {
      window.history.pushState(null, "", path);
      window.dispatchEvent(new PopStateEvent("popstate"));
      return true;
    }, `/orgs/${UNKNOWN_ORG}/sandboxes`);

    await expect(
      screen.getByText("It was not found", { exact: false }),
    ).toBeVisible();
    await expect(screen.getByRole("button", "Try again")).toBeHidden();
    await expect(screen.getByText("Choose an organization")).toBeVisible();

    await screen.getByRole("link", "Go to your organizations").tap();
    await expect(browser).toHaveURL("/");
    await expect(
      screen.getByRole("heading", "Organizations", { level: 1 }),
    ).toBeVisible();
  });

  test("an unknown device shows no Try again, and the switcher keeps the section", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(
      screen.getByRole("heading", "Organizations", { level: 1 }),
    ).toBeVisible();

    await browser.evaluate((path: string) => {
      window.history.pushState(null, "", path);
      window.dispatchEvent(new PopStateEvent("popstate"));
      return true;
    }, `/orgs/${ACME}/devices/${UNKNOWN_DEVICE}`);

    await expect(
      screen.getByText("It was not found", { exact: false }),
    ).toBeVisible();
    await expect(screen.getByRole("button", "Try again")).toBeHidden();

    await screen.getByRole("combobox", "Organization").tap();
    await screen.getByRole("option", "Northwind Labs").tap();
    await expect(browser).toHaveURL(`/orgs/${NORTHWIND}/devices`);
  });
});
