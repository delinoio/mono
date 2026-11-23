// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

// IDs come from src/mock/mock-api.ts. The suite runs against the in-browser
// fake API (see e2e.config.ts).
const ACME = "0193a000-0000-7000-8000-000000000001";

describe("table scroll regions", () => {
  test("the sandbox list takes keyboard focus with a visible ring and scrolls with the arrow keys", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    // 1280 px is narrow enough for the seven sandbox columns to overflow.
    await browser.setViewport({ width: 1280, height: 800 });
    await app.open(`/orgs/${ACME}/sandboxes`);
    await expect(
      screen.getByRole("heading", "Sandboxes", { level: 1 }),
    ).toBeVisible();
    await expect(screen.getByRole("region", "Sandbox list")).toBeVisible();

    // Tab until the region has focus; the page has fewer than 40 stops before it.
    const focused = await browser.evaluate(() => {
      // Start from the document so the first Tab enters the page.
      (document.activeElement as HTMLElement | null)?.blur();
      return true;
    });
    expect(focused).toBe(true);
    let reached = false;
    for (let i = 0; i < 40 && !reached; i += 1) {
      await browser.keyboard.press("Tab");
      reached = await browser.evaluate(
        () =>
          document.activeElement?.getAttribute("aria-label") === "Sandbox list",
      );
    }
    expect(reached, "Tab reaches the Sandbox list region").toBe(true);

    const ring = await browser.evaluate(() => {
      const style = getComputedStyle(document.activeElement as Element);
      return `${style.outlineStyle} ${style.outlineWidth}`;
    });
    expect(ring).toBe("solid 2px");

    // With focus on the region, the arrow keys scroll it sideways.
    expect(
      await browser.evaluate(() => document.activeElement?.scrollLeft ?? -1),
    ).toBe(0);
    await browser.keyboard.press("ArrowRight");
    // Chrome scrolls a key press with an animation, so wait for the first
    // frame that shows movement (at most 600 frames) instead of a fixed time.
    const scrolled = await browser.evaluate(async () => {
      const region = document.activeElement;
      for (let frame = 0; frame < 600; frame += 1) {
        if (region !== null && region.scrollLeft > 0) break;
        await new Promise((resolve) => requestAnimationFrame(resolve));
      }
      return {
        scrollLeft: region?.scrollLeft ?? -1,
        scrollWidth: region?.scrollWidth ?? -1,
        clientWidth: region?.clientWidth ?? -1,
      };
    });
    expect(scrolled.scrollWidth).toBeGreaterThan(scrolled.clientWidth);
    expect(scrolled.scrollLeft).toBeGreaterThan(0);

    // The next Tab goes to the first sandbox link inside the region.
    await browser.keyboard.press("Tab");
    const inside = await browser.evaluate(() => {
      const active = document.activeElement;
      return (
        active?.tagName === "A" &&
        active.closest('[aria-label="Sandbox list"]') !== null
      );
    });
    expect(inside).toBe(true);
  });

  test("a mouse click inside the sandbox list shows no focus ring", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await browser.setViewport({ width: 1280, height: 800 });
    await app.open(`/orgs/${ACME}/sandboxes`);
    await expect(screen.getByRole("region", "Sandbox list")).toBeVisible();

    await screen.getByRole("columnheader", "State").tap();
    const outline = await browser.evaluate(() => {
      const region = document.querySelector('[aria-label="Sandbox list"]');
      if (region === null) return "missing";
      return `${document.activeElement === region} ${getComputedStyle(region).outlineStyle}`;
    });
    expect(outline).toBe("true none");
  });

  test("the device list is a named region", async ({ app, screen }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/devices`);
    await expect(screen.getByRole("region", "Device list")).toBeVisible();
  });
});
