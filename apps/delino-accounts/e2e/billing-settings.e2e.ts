// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

describe("automatic top-up settings", () => {
  test("shows cap usage and refreshes it without discarding the amount draft", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/billing-settings");
    await screen.getByRole("tab", "Settings").click();
    const capUsage = browser.locator('dt:text-is("Monthly cap usage") + dd');
    await expect(capUsage).toHaveText("$25.00");
    await expect(
      screen.getByText(/Automatic top-ups in progress count toward the cap\./),
    ).toBeVisible();
    await expect(screen.getByText("Charged this month")).toBeHidden();

    const amount = screen.getByRole("textbox", "Top-up amount (USD)");
    await amount.fill("75");
    await browser.evaluate(() =>
      window.dispatchEvent(new Event("billing-fixture:refresh-cap-usage")),
    );
    await expect(capUsage).toHaveText("$30.00");
    await expect(
      browser.locator('dt:text-is("Failed top-ups in a row") + dd'),
    ).toHaveText("1");
    await expect(amount).toHaveValue("75");
  });
});
