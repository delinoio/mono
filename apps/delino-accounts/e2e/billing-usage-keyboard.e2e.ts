// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

const tableName = "Usage by service, organization and meter";

describe("billing usage table keyboard scrolling", () => {
  test("scrolls to Cost and back with the arrow keys at 360 px", async ({
    app,
    browser,
    screen,
  }) => {
    await browser.setViewport({ width: 360, height: 844 });
    await app.open("/billing-usage");
    await browser.evaluate(() => {
      window.billingUsageFixture.seedKeyboardScroll();
      return null;
    });
    await screen.getByRole("tab", "Usage").click();

    const region = screen.getByRole("region", tableName, { exact: true });
    const previous = screen.getByRole("button", "Previous month");
    const olderEntries = screen.getByRole("button", "Show older entries");
    await expect(region).toHaveAttribute("tabindex", "0");
    await expect(olderEntries).toBeEnabled();

    await browser.keyboard.press("Tab");
    await expect(previous).toBeFocused();
    await browser.keyboard.press("Tab");
    await expect(region).toBeFocused();

    const readScroll = () =>
      browser.evaluate(() => {
        const wrapper = document.activeElement!;
        const cost = wrapper
          .querySelector("tbody td:last-child")!
          .getBoundingClientRect();
        const style = getComputedStyle(wrapper);
        const left = wrapper.getBoundingClientRect().left + wrapper.clientLeft;
        return {
          overflow: wrapper.scrollWidth - wrapper.clientWidth,
          scrollLeft: wrapper.scrollLeft,
          ring: `${style.outlineStyle} ${style.outlineWidth}`,
          costInView:
            cost.left >= left - 1 &&
            cost.right <= left + wrapper.clientWidth + 1,
        };
      });

    const initial = await readScroll();
    expect(initial.overflow).toBeGreaterThan(0);
    expect(initial.scrollLeft).toBe(0);
    expect(initial.ring).toBe("solid 2px");
    expect(initial.costInView).toBe(false);
    await app.screenshot("usage-keyboard-focus");

    await expect
      .poll(async () => {
        if ((await readScroll()).costInView) return true;
        await browser.keyboard.press("ArrowRight");
        return false;
      })
      .toBe(true);
    expect((await readScroll()).scrollLeft).toBeGreaterThan(0);
    await app.screenshot("usage-keyboard-cost");

    await expect
      .poll(async () => {
        if ((await readScroll()).scrollLeft === initial.scrollLeft) return true;
        await browser.keyboard.press("ArrowLeft");
        return false;
      })
      .toBe(true);
    await expect(region).toBeFocused();

    await browser.keyboard.press("Tab");
    await expect(olderEntries).toBeFocused();
    await browser.keyboard.press("Shift+Tab");
    await expect(region).toBeFocused();
    await browser.keyboard.press("Shift+Tab");
    await expect(previous).toBeFocused();
  });

  test("keeps the native table on desktop and omits it for an empty month", async ({
    app,
    browser,
    screen,
  }) => {
    await browser.setViewport({ width: 1280, height: 900 });
    await app.open("/billing-usage");
    await browser.evaluate(() => {
      window.billingUsageFixture.seedKeyboardScroll();
      return null;
    });
    await screen.getByRole("tab", "Usage").click();

    const region = screen.getByRole("region", tableName, { exact: true });
    const table = region.getByRole("table", tableName, { exact: true });
    await expect(browser.locator("table > caption")).toHaveText(tableName);
    await expect(table.getByRole("columnheader")).toHaveCount(5);
    await expect(table.getByRole("columnheader", "Quantity")).toBeVisible();
    await expect(table.getByRole("columnheader", "Cost")).toBeVisible();
    await expect(table.getByRole("cell", "1,048,576 bytes")).toBeVisible();
    await expect(table.getByRole("cell", "$0.004567")).toBeVisible();
    await expect(screen.getByLabel("Usage total")).toHaveText("$0.004567");

    await screen.getByRole("button", "Previous month").click();
    await expect(screen.getByText("No usage in this month")).toBeVisible();
    await expect(region).toHaveCount(0);
    await expect(screen.getByRole("table", tableName)).toHaveCount(0);
  });
});
