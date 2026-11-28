// SPDX-License-Identifier: Apache-2.0
import { Code } from "@connectrpc/connect";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

const path = `/orgs/${MOCK_IDS.acme}/enrollment-tokens`;
const billingLink = "Manage billing at accounts.delino.io";
const settingsLink = "Link a billing account";

describe("billing banner refresh", () => {
  for (const { phase, code, terminal } of [
    { phase: "blocked", code: Code.NotFound, terminal: true },
    { phase: "blocked", code: Code.PermissionDenied, terminal: true },
    { phase: "blocked", code: Code.Unauthenticated, terminal: true },
    { phase: "suspended", code: Code.NotFound, terminal: true },
    { phase: "blocked", code: Code.Unavailable, terminal: false },
  ]) {
    test(`handles ${Code[code]} after a ${phase} status`, async ({
      app,
      browser,
      screen,
    }) => {
      const setStatusResult = (result: string) =>
        browser.evaluate((value) => {
          sessionStorage.setItem("ownsand-e2e-billing-status-result", value);
          return true;
        }, result);
      const readRefreshes = () =>
        browser.evaluate(() => {
          const value = sessionStorage.getItem(
            "ownsand-e2e-billing-status-refreshes",
          );
          return JSON.parse(value ?? "[]") as string[];
        });
      const submitBlockedMutation = async () => {
        await screen.getByRole("button", "Create token").tap();
        const dialog = screen.getByRole("dialog");
        await dialog.getByRole("button", "Create token").tap();
        await expect(
          dialog.getByRole("link", "https://accounts.delino.io"),
        ).toBeVisible();
        await browser.keyboard.press("Escape");
        await expect(dialog).toBeHidden();
      };

      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await expect(
        screen.getByRole("heading", "Organizations", { level: 1 }),
      ).toBeVisible();
      await browser.evaluate((seed) => {
        sessionStorage.setItem("ownsand-e2e-billing-status-seed", seed);
        return true;
      }, phase);
      await app.open(path);

      const name = `This organization is ${phase}`;
      const banner = screen.getByRole("region", name);
      const heading = screen.getByRole("heading", "Enrollment tokens", {
        level: 1,
      });
      const evidence = `billing-status-${phase}-${Code[code]}`;
      await expect(heading).toBeVisible();
      await expect(banner.getByRole("link", billingLink)).toBeVisible();
      if (phase === "blocked") {
        await expect(banner).toContainText("No billing account is linked");
        await expect(banner.getByRole("link", settingsLink)).toBeVisible();
      } else {
        await expect(banner).toContainText(
          "Payment has been missing for 3 days",
        );
      }
      await app.screenshot(`${evidence}-before`);

      await setStatusResult(String(code));
      await submitBlockedMutation();
      await expect
        .poll(readRefreshes)
        .toEqual(Array.from({ length: terminal ? 1 : 3 }, () => String(code)));
      if (terminal) {
        await expect(banner).toBeHidden();
        await expect(screen.getByRole("link", billingLink)).toBeHidden();
        await expect(screen.getByRole("link", settingsLink)).toBeHidden();
      } else {
        await expect(banner).toContainText("No billing account is linked");
        await expect(banner.getByRole("link", billingLink)).toBeVisible();
        await expect(banner.getByRole("link", settingsLink)).toBeVisible();
      }
      await expect(heading).toBeVisible();
      await expect(
        screen.getByRole("navigation", "Organization"),
      ).toBeVisible();
      await expect(browser).toHaveURL(path);
      await app.screenshot(`${evidence}-after`);

      if (!terminal) {
        await setStatusResult("allowed");
        await submitBlockedMutation();
        await expect(banner).toBeHidden();
        expect(await readRefreshes()).toEqual([
          String(code),
          String(code),
          String(code),
          "allowed",
        ]);
        await expect(heading).toBeVisible();
        await app.screenshot(`${evidence}-recovered`);
      }
    });
  }
});

async function relinkAndRefreshBillingUsage() {
  sessionStorage.setItem("ownsand-e2e-billing-relink", "true");
  sessionStorage.setItem("ownsand-e2e-billing-usage-result", "hold");
  const now = Date.now;
  // Make the cached queries stale for the normal tab-visibility refresh.
  Date.now = () => now() + 11_000;
  try {
    window.dispatchEvent(new Event("visibilitychange"));
    await Promise.resolve();
  } finally {
    Date.now = now;
  }
  return true;
}

describe("billing usage refresh", () => {
  test("hides previous usage through a relink and failed refresh", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(
      screen.getByRole("heading", "Organizations", { level: 1 }),
    ).toBeVisible();
    await browser.evaluate(() => {
      sessionStorage.setItem("ownsand-e2e-billing-usage-role", "owner");
      return true;
    });
    await app.open(`/orgs/${MOCK_IDS.acme}/settings`);

    const billing = screen.getByRole("region", "Billing", { exact: true });
    const table = billing.getByRole("table", "Usage this month");
    const unlink = billing.getByRole("button", "Unlink billing account");
    const retry = billing.getByRole("button", "Try again");
    const oldDetails = ["Acme Robotics billing", "$42.52", "$1.17"];
    const expectPreviousUsageHidden = async () => {
      for (const detail of oldDetails) {
        await expect(billing.getByText(detail, { exact: true })).toBeHidden();
      }
      await expect(table).toBeHidden();
    };
    for (const detail of oldDetails) {
      await expect(billing.getByText(detail, { exact: true })).toBeVisible();
    }
    await expect(table).toBeVisible();

    await unlink.tap();
    const opening = screen.getByRole(
      "alertdialog",
      "Unlink Acme Robotics billing?",
    );
    await expect(opening).toBeVisible();
    await browser.evaluate(relinkAndRefreshBillingUsage);
    await expect(
      billing.getByRole("status", "Loading the billing usage"),
    ).toBeVisible();
    await expectPreviousUsageHidden();
    await expect(opening).toBeVisible();
    await app.screenshot("billing-relink-usage-loading");

    await browser.evaluate(() => {
      const key = "ownsand-e2e-billing-usage-result";
      sessionStorage.setItem(key, "fail");
      window.dispatchEvent(new Event(`${key}-release`));
      return true;
    });
    const error = billing.getByText(
      "OwnSand is not available right now. Try again in a moment.",
      { exact: true },
    );
    await expect(error).toBeVisible({ timeout: 10_000 });
    await expectPreviousUsageHidden();
    await opening.getByRole("button", "Unlink billing account").tap();
    await expect(opening).toBeHidden({ timeout: 10_000 });
    await expect(browser.locator(".dui-dialog-backdrop")).toHaveCount(0);
    expect(
      await browser.evaluate(() =>
        JSON.parse(
          sessionStorage.getItem("ownsand-e2e-billing-unlink-requests") ?? "[]",
        ),
      ),
    ).toEqual([MOCK_IDS.acmeBillingAccount]);
    await expect(retry).toBeVisible();
    await expectPreviousUsageHidden();
    await app.screenshot("billing-relink-usage-error");

    await unlink.tap();
    const nextOpening = screen.getByRole(
      "alertdialog",
      "Unlink the billing account?",
    );
    await expect(nextOpening).toBeVisible();
    await nextOpening.getByRole("button", "Cancel").tap();
    await expect(nextOpening).toBeHidden();

    await browser.evaluate(() => {
      sessionStorage.removeItem("ownsand-e2e-billing-usage-result");
      return true;
    });
    await retry.tap();
    await expect(billing.getByText("Research lab credits")).toBeVisible();
    await expect(billing.getByText("-$1.23 (debt)")).toBeVisible();
    await expect(billing.getByText("$3.66", { exact: true })).toBeVisible();
    await expect(table.getByText("9,999 calls")).toBeVisible();
    await expect(retry).toBeHidden();
    await app.screenshot("billing-relink-usage-recovered");
  });
});
