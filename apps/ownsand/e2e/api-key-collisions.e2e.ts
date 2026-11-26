// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import {
  FIRST_KEY_ID,
  KEY_PREFIX,
  SECOND_KEY_ID,
} from "./api-key-collision-fixture.ts";

const ACME = "0193a000-0000-7000-8000-000000000001";
const revokeLabel = `Revoke API key ci-key (${KEY_PREFIX})`;

const collisionTest = test.extend<{
  openKeys(holdContinuation?: boolean): Promise<void>;
  revokeRequests(): Promise<unknown>;
}>({
  openKeys: async ({ app, browser, screen }, use) => {
    await use(async (holdContinuation = false) => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await expect(
        screen.getByRole("link", "Acme Robotics Owner"),
      ).toBeVisible();
      await browser.evaluate((hold) => {
        sessionStorage.setItem("ownsand-e2e-api-key-collision", "true");
        if (hold) {
          sessionStorage.setItem("ownsand-e2e-api-key-continuation", "hold");
        }
        return null;
      }, holdContinuation);
      await app.open(`/orgs/${ACME}/api-keys`);
    });
  },
  revokeRequests: async ({ browser }, use) => {
    await use(() =>
      browser.evaluate(() =>
        JSON.parse(
          sessionStorage.getItem("ownsand-e2e-api-key-revokes") ?? "[]",
        ),
      ),
    );
  },
});

describe("API key revoke identity", () => {
  collisionTest(
    "identifies each key and revokes only the confirmed key",
    async ({ app, browser, openKeys, revokeRequests, screen }) => {
      await openKeys();
      await expect(screen.getByText(FIRST_KEY_ID)).toBeVisible();
      await expect(screen.getByText(SECOND_KEY_ID)).toBeVisible();
      await app.screenshot("api-key-collision-rows");

      const dialog = screen.getByRole("alertdialog", "Revoke ci-key?");
      await screen
        .getByRole("button", `${revokeLabel}, key ID ${FIRST_KEY_ID}`)
        .tap();
      await expect(dialog.getByText(FIRST_KEY_ID)).toBeVisible();
      await expect(dialog.getByText(KEY_PREFIX)).toBeVisible();
      await app.screenshot("api-key-collision-first-confirmation");
      await dialog.getByRole("button", "Cancel").tap();
      await expect(browser.locator(".dui-dialog-backdrop")).toHaveCount(0);
      expect(await revokeRequests()).toEqual([]);

      await screen
        .getByRole("button", `${revokeLabel}, key ID ${SECOND_KEY_ID}`)
        .tap();
      await expect(dialog.getByText(SECOND_KEY_ID)).toBeVisible();
      await app.screenshot("api-key-collision-second-confirmation");
      await dialog.getByRole("button", "Revoke key").tap();
      await expect(screen.getByText("ci-key · Revoked")).toBeVisible();
      await expect(screen.getByRole("button", revokeLabel)).toHaveCount(1);
      expect(await revokeRequests()).toEqual([
        { organizationId: ACME, apiKeyId: SECOND_KEY_ID },
      ]);
      await app.screenshot("api-key-collision-after-revoke");
    },
  );

  collisionTest(
    "updates an open confirmation when the next page adds a collision",
    async ({ app, browser, openKeys, revokeRequests, screen }) => {
      await openKeys(true);
      await screen.getByRole("button", revokeLabel).tap();
      const dialog = screen.getByRole("alertdialog", "Revoke ci-key?");
      await expect(dialog.getByText(KEY_PREFIX)).toBeVisible();
      await expect(dialog.getByText(FIRST_KEY_ID)).toHaveCount(0);
      await app.screenshot("api-key-collision-before-continuation");

      await browser.evaluate(() => {
        const key = "ownsand-e2e-api-key-continuation";
        sessionStorage.setItem(key, "released");
        window.dispatchEvent(new Event(`${key}-release`));
        return null;
      });
      await expect(dialog.getByText(FIRST_KEY_ID)).toBeVisible();
      await expect(dialog.getByText(SECOND_KEY_ID)).toHaveCount(0);
      await app.screenshot("api-key-collision-after-continuation");
      await dialog.getByRole("button", "Revoke key").tap();
      await expect(screen.getByText("ci-key · Revoked")).toBeVisible();
      expect(await revokeRequests()).toEqual([
        { organizationId: ACME, apiKeyId: FIRST_KEY_ID },
      ]);
    },
  );
});
