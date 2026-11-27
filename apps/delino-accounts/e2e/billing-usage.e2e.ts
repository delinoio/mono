// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

declare global {
  interface Window {
    usageWallTime: number;
  }
}

describe("billing usage month picker", () => {
  test("keeps the selected month through a visible-tab clock correction", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/billing-usage");
    const usage = screen.getByRole("tab", "Usage");
    await expect(usage).toBeVisible();
    await browser.evaluate(() => {
      window.usageWallTime = Date.parse("2026-12-15T12:00:00Z");
      window.Date = new Proxy(Date, {
        construct(target, args) {
          return Reflect.construct(
            target,
            args.length ? args : [window.usageWallTime],
          );
        },
      });
      // Shorten the minute-long recheck for this browser test. The clock
      // correction itself does not advance or dispatch any timers.
      window.setTimeout = new Proxy(window.setTimeout, {
        apply(target, receiver, [handler, delay, ...args]) {
          return Reflect.apply(target, receiver, [
            handler,
            delay === 60_000 ? 1_000 : delay,
            ...args,
          ]);
        },
      });
      return null;
    });
    await usage.click();
    const next = screen.getByRole("button", "Next month");
    await expect(next).toBeDisabled();
    await expect(screen.getByText("December 2026 (UTC)")).toBeVisible();
    const summaryMonths = () =>
      browser.evaluate(() => window.billingUsageFixture.summaryMonths());
    await expect.poll(summaryMonths).toEqual([[2026, 12]]);

    expect(
      await browser.evaluate(() => {
        window.usageWallTime = Date.parse("2027-01-10T08:00:00Z");
        return document.visibilityState;
      }),
    ).toBe("visible");
    await expect(next).toBeEnabled();
    await expect(screen.getByText("December 2026 (UTC)")).toBeVisible();
    expect(await summaryMonths()).toEqual([[2026, 12]]);

    await next.click();
    await expect(screen.getByText("January 2027 (UTC)")).toBeVisible();
    await expect(next).toBeDisabled();
    await expect.poll(summaryMonths).toEqual([
      [2026, 12],
      [2027, 1],
    ]);
  });
});

describe("billing usage pagination", () => {
  test("updates the monthly total when the last row costs more", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/billing-usage");
    await browser.evaluate(() => {
      window.billingUsageFixture.seedPagination();
      return null;
    });
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
    await browser.evaluate(() => {
      window.billingUsageFixture.seedPagination();
      return null;
    });
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
    await browser.evaluate(() => {
      window.billingUsageFixture.seedPagination();
      return null;
    });
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
