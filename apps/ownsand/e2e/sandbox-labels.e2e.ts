// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

// IDs come from src/mock/mock-api.ts.
const ACME = "0193a000-0000-7000-8000-000000000001";
const UNLABELED_SANDBOX = "0193a000-0000-7000-8000-0000000000a4";

describe("sandbox detail labels", () => {
  test("wraps the full long label inside its cell at 320px", async ({
    app,
    browser,
    screen,
  }) => {
    const longLabel = `purpose=${"W".repeat(255)}`;
    const shortLabel = "team=ci";
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/sandboxes`);
    await screen.getByRole("button", "Create sandbox").tap();
    const dialog = screen.getByRole("dialog", "Create a sandbox");
    await dialog.getByLabel("Labels").fill(`${longLabel}\n${shortLabel}`);
    await dialog.getByRole("button", "Create").tap();
    await expect(browser).toHaveURL(/\/sandboxes\/[0-9a-f-]+$/);

    await browser.setViewport({ width: 320, height: 800 });
    const labels = screen.getByRole("list", "Labels");
    await expect(labels.getByText(longLabel)).toBeVisible();
    await expect(labels.getByText(shortLabel)).toBeVisible();
    const layout = await browser.evaluate(
      async (expectedLabels) => {
        await document.fonts.ready;
        const cell = document.querySelector(".detail dd:has(.labels)");
        if (!cell) throw new Error("The Labels cell is missing");
        const bounds = cell.getBoundingClientRect();
        const badges = Array.from(cell.querySelectorAll(".dui-badge"));
        return {
          pageWidth: document.documentElement.clientWidth,
          scrollWidth: document.documentElement.scrollWidth,
          labels: expectedLabels.map((text) => {
            const badge = badges.find(
              (element) => element.textContent === text,
            );
            if (!badge) throw new Error("The full label is missing");
            const range = document.createRange();
            range.selectNodeContents(badge);
            const fragments = Array.from(range.getClientRects());
            return {
              fits:
                badge.getBoundingClientRect().right <= bounds.right &&
                fragments.every(
                  ({ left, right }) =>
                    left >= bounds.left && right <= bounds.right,
                ),
              lines: new Set(fragments.map(({ top }) => top)).size,
            };
          }),
        };
      },
      [longLabel, shortLabel],
    );

    expect(layout.pageWidth).toBe(320);
    expect(layout.scrollWidth).toBe(320);
    const [long, short] = layout.labels;
    expect(long.fits).toBe(true);
    expect(long.lines).toBeGreaterThan(1);
    expect(short).toEqual({ fits: true, lines: 1 });
  });

  test("shows None when the sandbox has no labels", async ({ app, screen }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/sandboxes/${UNLABELED_SANDBOX}`);
    await expect(screen.getByText("None", { exact: true })).toBeVisible();
  });
});
