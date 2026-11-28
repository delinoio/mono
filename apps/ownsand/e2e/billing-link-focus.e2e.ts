// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

async function refreshBillingList({
  key,
  value,
}: {
  key: string;
  value: string;
}) {
  sessionStorage.setItem(`ownsand-e2e-billing-list-${key}`, value);
  const now = Date.now;
  // React Query refetches stale queries when the tab becomes visible.
  Date.now = () => now() + 11_000;
  try {
    window.dispatchEvent(new Event("visibilitychange"));
    await Promise.resolve();
  } finally {
    Date.now = now;
  }
  return true;
}

describe("billing link focus", () => {
  test("focuses the first option after loading and on cached reopen", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await browser.evaluate(() => {
      sessionStorage.setItem("ownsand-e2e-billing-status-seed", "blocked");
      sessionStorage.setItem("ownsand-e2e-billing-list-initial", "hold");
      return true;
    });
    await app.open(`/orgs/${MOCK_IDS.acme}/settings`);
    const opener = screen.getByRole("button", "Link a billing account");
    await opener.tap();
    const dialog = screen.getByRole("dialog", "Link a billing account");
    const first = dialog.getByRole("radio", "Acme Robotics billing · Owner");
    await expect(first).toBeHidden();
    await expect(dialog.getByRole("button", "Link")).toHaveAttribute(
      "disabled",
      "",
    );
    await expect
      .poll(() =>
        browser.evaluate(() =>
          sessionStorage.getItem("ownsand-e2e-billing-list-initial"),
        ),
      )
      .toEqual("held");

    await browser.evaluate(() =>
      window.dispatchEvent(
        new Event("ownsand-e2e-billing-list-initial-release"),
      ),
    );
    await expect(first).toBeFocused();
    await app.screenshot("billing-link-focus-after-loading");
    await dialog.getByRole("radio", "Research lab credits · Admin").tap();
    await browser.keyboard.press("Escape");
    await expect(dialog).toBeHidden();
    await opener.tap();
    await expect(first).toBeFocused();
    await expect(first).toHaveAttribute("aria-checked", "false");
    await app.screenshot("billing-link-focus-cached-reopen");
  });

  test("keeps focus on retained controls and moves focus when its option disappears", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await browser.evaluate(() => {
      sessionStorage.setItem("ownsand-e2e-billing-status-seed", "blocked");
      return true;
    });
    await app.open(`/orgs/${MOCK_IDS.acme}/settings`);
    await screen.getByRole("button", "Link a billing account").tap();
    const dialog = screen.getByRole("dialog", "Link a billing account");
    const first = dialog.getByRole("radio", "Acme Robotics billing · Owner");
    const lab = dialog.getByRole("radio", "Research lab credits · Admin");
    const cancel = dialog.getByRole("button", "Cancel");
    const link = dialog.getByRole("button", "Link");
    await expect(first).toBeFocused();
    await browser.keyboard.press("Space");
    await expect(first).toHaveAttribute("aria-checked", "true");

    for (const [target, name, screenshot] of [
      [cancel, "Acme billing renamed", "cancel"],
      [link, "Acme billing updated", "link"],
    ] as const) {
      await browser.keyboard.press("Tab");
      await expect(target).toBeFocused();
      await browser.evaluate(refreshBillingList, {
        key: "rename-first",
        value: name,
      });
      await expect(
        dialog.getByRole("radio", `${name} · Owner`),
      ).toHaveAttribute("aria-checked", "true");
      await expect(target).toBeFocused();
      await app.screenshot(`billing-link-focus-${screenshot}`);
    }

    // Move focus independently of selection so a jump to the checked radio fails the test.
    await browser.evaluate(() => {
      document
        .querySelectorAll<HTMLElement>('[role="dialog"] [role="radio"]')[1]
        ?.focus();
      return true;
    });
    await expect(lab).toBeFocused();
    await browser.evaluate(refreshBillingList, {
      key: "rename-first",
      value: "Acme billing refreshed",
    });
    const renamed = dialog.getByRole("radio", "Acme billing refreshed · Owner");
    await expect(renamed).toHaveAttribute("aria-checked", "true");
    await expect(lab).toBeFocused();
    await app.screenshot("billing-link-focus-retained-option");

    await renamed.tap();
    await expect(renamed).toBeFocused();
    await browser.evaluate(refreshBillingList, {
      key: "remove-first",
      value: "true",
    });
    await expect(renamed).toBeHidden();
    await expect(lab).toHaveAttribute("aria-checked", "false");
    await expect(lab).toBeFocused();
    await app.screenshot("billing-link-focus-removed-option");
  });
});
