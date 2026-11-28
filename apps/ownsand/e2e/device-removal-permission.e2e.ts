// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { Role } from "@delino/ownsand-protos/v1/common_pb";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

const prefix = "ownsand-e2e-device-removal";
const devices = `/orgs/${MOCK_IDS.acme}/devices`;
const detail = `${devices}/${MOCK_IDS.buildBox}`;

const deviceTest = test.extend<{
  removal: {
    open(): Promise<void>;
    refreshRole(role: Role): Promise<void>;
    countRequests(): Promise<number>;
    hold(): Promise<void>;
    held(): Promise<boolean>;
    release(): Promise<void>;
  };
}>({
  removal: async ({ app, browser, screen }, use) => {
    await use({
      async open() {
        await app.open("/");
        await screen.getByRole("button", "Sign in with Delino").tap();
        await expect(
          screen.getByRole("heading", "Organizations"),
        ).toBeVisible();
        await browser.evaluate(
          ([key, role]) => {
            sessionStorage.setItem(key, role);
            return null;
          },
          [`${prefix}-role`, String(Role.ADMIN)] as const,
        );
        await app.open(detail);
        await screen.getByRole("button", "Remove device").tap();
        await expect(screen.getByRole("alertdialog")).toBeVisible();
      },
      async refreshRole(role) {
        await browser.evaluate(
          async ([key, role]) => {
            sessionStorage.setItem(key, role);
            const now = Date.now;
            // Make the organization query stale for the normal visibility refresh.
            Date.now = () => now() + 11_000;
            try {
              window.dispatchEvent(new Event("visibilitychange"));
              await Promise.resolve();
            } finally {
              Date.now = now;
            }
            return null;
          },
          [`${prefix}-role`, String(role)] as const,
        );
      },
      countRequests() {
        return browser.evaluate(
          (key) =>
            (JSON.parse(sessionStorage.getItem(key) ?? "[]") as string[])
              .length,
          `${prefix}-requests`,
        );
      },
      async hold() {
        await browser.evaluate((key) => {
          sessionStorage.setItem(key, "true");
          return null;
        }, `${prefix}-hold`);
      },
      held() {
        return browser.evaluate(
          (key) => sessionStorage.getItem(key) === "true",
          `${prefix}-held`,
        );
      },
      async release() {
        await browser.evaluate((event) => {
          window.dispatchEvent(new Event(event));
          return null;
        }, `${prefix}-release`);
      },
    });
  },
});

describe("device removal after an organization role refresh", () => {
  deviceTest(
    "closes on demotion and requires a fresh confirmation after recovery",
    async ({ app, browser, screen, removal }) => {
      const dialog = screen.getByRole("alertdialog");
      const opener = screen.getByRole("button", "Remove device");
      await removal.open();
      await removal.refreshRole(Role.MEMBER);
      await expect(screen.getByText("· Member")).toBeVisible();
      await expect(dialog).toHaveCount(0);
      await expect(opener).toHaveCount(0);
      await expect(screen.getByRole("heading", "build-box-1")).toBeVisible();
      await expect(browser).toHaveURL(detail);
      await browser.keyboard.press("Enter");
      expect(await removal.countRequests()).toBe(0);
      await app.screenshot("device-detail-after-demotion");

      await removal.refreshRole(Role.ADMIN);
      await expect(opener).toBeVisible();
      await expect(dialog).toHaveCount(0);
      expect(await removal.countRequests()).toBe(0);
      await opener.tap();
      await expect(dialog).toBeVisible();
    },
  );

  deviceTest(
    "keeps the confirmation open when the refreshed role can remove devices",
    async ({ browser, screen, removal }) => {
      await removal.open();
      await removal.refreshRole(Role.OWNER);
      await expect(screen.getByText("· Owner")).toBeVisible();
      const dialog = screen.getByRole("alertdialog");
      await expect(dialog).toBeVisible();
      await dialog.getByRole("button", "Remove device").tap();
      await expect(browser).toHaveURL(devices);
      expect(await removal.countRequests()).toBe(1);
    },
  );

  deviceTest(
    "refreshes the device without redirecting after a removal started before demotion",
    async ({ browser, screen, removal }) => {
      await removal.open();
      await removal.hold();
      const dialog = screen.getByRole("alertdialog");
      await dialog.getByRole("button", "Remove device").tap();
      await expect.poll(removal.held).toBe(true);

      await removal.refreshRole(Role.MEMBER);
      await expect(screen.getByText("· Member")).toBeVisible();
      await expect(dialog).toHaveCount(0);
      await removal.release();

      await expect(screen.getByText("Removed", { exact: true })).toBeVisible();
      await expect(browser).toHaveURL(detail);
      await expect(dialog).toHaveCount(0);
      expect(await removal.countRequests()).toBe(1);
    },
  );
});
