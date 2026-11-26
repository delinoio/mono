// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { MOCK_IDS } from "../src/mock/mock-api.ts";
import {
  FIXTURE_ORGANIZATION_NAME,
  MIXED_CASE_ORGANIZATION_ID,
} from "./organization-uuid-fixture.ts";

describe("organization switcher UUID case", () => {
  test("selects a mixed-case route's organization when its page arrives", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(screen.getByRole("link", "Acme Robotics Owner")).toBeVisible();
    await browser.evaluate(() => {
      sessionStorage.setItem("ownsand-e2e-organization-uuid-case", "true");
      sessionStorage.setItem("ownsand-e2e-paged-list", "ListOrganizations");
      sessionStorage.setItem("ownsand-e2e-page-error", "hold");
      return null;
    });

    const path = `/orgs/${MIXED_CASE_ORGANIZATION_ID}/sandboxes`;
    await app.open(path);
    await expect(screen.getByRole("heading", "Sandboxes")).toBeVisible();
    const switcher = screen.getByRole("combobox", "Organization");
    await expect(switcher).toHaveText(MIXED_CASE_ORGANIZATION_ID);
    await switcher.tap();
    await expect(
      screen.getByRole("option", "Loading more organizations"),
    ).toBeVisible();
    await browser.evaluate(() => {
      sessionStorage.removeItem("ownsand-e2e-page-error");
      window.dispatchEvent(new Event("ownsand-e2e-release-page"));
      return null;
    });

    await expect(
      screen.getByRole("option", FIXTURE_ORGANIZATION_NAME),
    ).toBeVisible();
    await expect(switcher).toHaveText(FIXTURE_ORGANIZATION_NAME);
    await expect(browser).toHaveURL(path);
    await app.screenshot("mixed-case-organization-selected");
    await screen.getByRole("option", "Northwind Labs").tap();
    await expect(browser).toHaveURL(`/orgs/${MOCK_IDS.northwind}/sandboxes`);
    await expect(switcher).toHaveText("Northwind Labs");
  });
});
