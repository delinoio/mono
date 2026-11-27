// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

describe("billing usage pagination", () => {
  test("updates the monthly total when the last row costs more", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/billing-usage");
    await screen.getByRole("tab", "Usage").click();

    const total = screen.getByLabel("Usage total");
    const rows = browser.locator("table tbody tr");
    const more = screen.getByRole("button", "Show more usage");
    const pageTokens = () =>
      browser.evaluate(() => window.billingUsageFixture.pageTokens());
    await expect(total).toHaveText("$201.00");
    await expect(rows).toHaveCount(200);
    expect(await pageTokens()).toEqual([""]);
    await app.screenshot("usage-first-page");

    await browser.evaluate(() => {
      window.billingUsageFixture.raiseLastRowCost();
      return null;
    });
    await more.click();

    await expect(total).toHaveText("$203.00");
    await expect(rows).toHaveCount(201);
    await expect(rows.last().getByText("usage-org-0201")).toBeVisible();
    await expect(rows.last().getByText("$3.00")).toBeVisible();
    await expect(more).toBeHidden();
    expect(await pageTokens()).toEqual(["", "p200"]);
    await browser.evaluate(() => {
      window.scrollTo(0, 0);
      return null;
    });
    await app.screenshot("usage-updated-total");
  });

  test("keeps the total during a failure and updates it on retry", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/billing-usage");
    await screen.getByRole("tab", "Usage").click();

    const total = screen.getByLabel("Usage total");
    const rows = browser.locator("table tbody tr");
    const more = screen.getByRole("button", "Show more usage");
    const retry = screen.getByRole("button", "Try again");
    await expect(total).toHaveText("$201.00");
    await expect(rows).toHaveCount(200);
    await browser.evaluate(() => {
      window.billingUsageFixture.raiseLastRowCost();
      window.billingUsageFixture.failNextContinuation();
      return null;
    });
    await more.click();

    await expect(retry).toBeVisible();
    await expect(total).toHaveText("$201.00");
    await expect(rows).toHaveCount(200);
    expect(
      await browser.evaluate(() => window.billingUsageFixture.pageTokens()),
    ).toEqual(["", "p200"]);
    await browser.evaluate(() => {
      window.scrollTo(0, 0);
      return null;
    });
    await app.screenshot("usage-failed-continuation");

    await retry.click();

    await expect(total).toHaveText("$203.00");
    await expect(rows).toHaveCount(201);
    await expect(rows.last().getByText("$3.00")).toBeVisible();
    await expect(retry).toBeHidden();
    await expect(more).toBeHidden();
    expect(
      await browser.evaluate(() => window.billingUsageFixture.pageTokens()),
    ).toEqual(["", "p200", "p200"]);
    await app.screenshot("usage-successful-retry");
  });

  test("updates the monthly total on an empty continuation", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/billing-usage");
    await screen.getByRole("tab", "Usage").click();

    const total = screen.getByLabel("Usage total");
    const rows = browser.locator("table tbody tr");
    const more = screen.getByRole("button", "Show more usage");
    await expect(total).toHaveText("$201.00");
    await expect(rows).toHaveCount(200);
    await browser.evaluate(() => {
      window.billingUsageFixture.emptyContinuation();
      return null;
    });
    await more.click();

    await expect(total).toHaveText("$203.00");
    await expect(rows).toHaveCount(200);
    await expect(more).toBeHidden();
    expect(
      await browser.evaluate(() => window.billingUsageFixture.pageTokens()),
    ).toEqual(["", "p200"]);
    await browser.evaluate(() => {
      window.scrollTo(0, 0);
      return null;
    });
    await app.screenshot("usage-empty-continuation");
  });
});
