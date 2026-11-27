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
