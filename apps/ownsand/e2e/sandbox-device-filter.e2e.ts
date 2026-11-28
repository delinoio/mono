// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

describe("sandbox device filter", () => {
  test("waits for the complete inventory before marking the selected device absent", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(screen.getByRole("link", "Acme Robotics Owner")).toBeVisible();
    await browser.evaluate(() => {
      sessionStorage.setItem("ownsand-e2e-device-filter", "true");
      return true;
    });
    await app.open(`/orgs/${MOCK_IDS.acme}/sandboxes`);
    await expect(screen.getByRole("cell", "build-box-1").first()).toBeVisible();
    const filter = screen.getByRole("combobox", "Device");
    await filter.tap();
    await screen.getByRole("option", "build-box-1").tap();
    await expect(filter).toHaveText("build-box-1");
    await expect
      .poll(() =>
        browser.evaluate(
          () =>
            window.sandboxDeviceFilterFixture.sandboxRequests.at(-1)
              ?.deviceId ?? null,
        ),
      )
      .toBe(MOCK_IDS.buildBox);

    const missingText =
      "build-box-1 is no longer in the device inventory. Select All devices to clear the filter.";
    const missing = screen.getByText(missingText);
    await browser.evaluate(async () => {
      await window.sandboxDeviceFilterFixture.removeDevice();
      const now = Date.now;
      Date.now = () => now() + 11_000;
      try {
        window.dispatchEvent(new Event("visibilitychange"));
        await Promise.resolve();
      } finally {
        Date.now = now;
      }
      return true;
    });
    const tokens = () =>
      browser.evaluate(() => window.sandboxDeviceFilterFixture.inventoryTokens);
    await expect
      .poll(() =>
        browser.evaluate(
          () => window.sandboxDeviceFilterFixture.refreshingAfterRemoval,
        ),
      )
      .toBe(true);
    await expect(missing).toHaveCount(0);

    await browser.evaluate(() => {
      window.sandboxDeviceFilterFixture.releaseFirstPage();
      return true;
    });
    await expect(screen.getByText("Loading more devices")).toBeVisible();
    await expect(missing).toHaveCount(0);
    const cursor = (await tokens()).at(-1);
    expect(cursor).toBeTruthy();
    await browser.evaluate(() => {
      window.sandboxDeviceFilterFixture.failContinuation();
      return true;
    });
    await expect(
      screen.getByText("Some devices are not shown yet."),
    ).toBeVisible();
    await expect(missing).toHaveCount(0);
    await filter.tap();
    await expect(screen.getByRole("option", "studio-mac")).toBeVisible();
    await browser.keyboard.press("Escape");

    await browser.evaluate(() => {
      window.sandboxDeviceFilterFixture.allowRetry();
      return true;
    });
    await screen.getByRole("button", "Try again").tap();
    await expect(missing).toBeVisible();
    await expect(missing).toHaveAttribute("role", "status");
    expect((await tokens()).at(-1)).toBe(cursor);

    await screen.getByRole("checkbox", "Show ended sandboxes").tap();
    await expect(screen.getByRole("cell", "Deleted").first()).toBeVisible();
    const latestRequest = () =>
      browser.evaluate(
        () => window.sandboxDeviceFilterFixture.sandboxRequests.at(-1) ?? null,
      );
    await expect
      .poll(latestRequest)
      .toEqual({ deviceId: MOCK_IDS.buildBox, includeEnded: true });
    await app.screenshot("missing-device-with-ended-sandboxes");

    await filter.tap();
    await screen.getByRole("option", "studio-mac").tap();
    await expect(missing).toHaveCount(0);
    await expect
      .poll(latestRequest)
      .toEqual({ deviceId: MOCK_IDS.studioMac, includeEnded: true });
  });
});
