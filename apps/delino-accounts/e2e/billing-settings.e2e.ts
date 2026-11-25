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

describe("low-balance alert settings", () => {
  for (const failConfirmation of [false, true]) {
    const scenario = failConfirmation
      ? "a failed confirming read and retry"
      : "a successful confirming read";
    test(`keeps newer alert edits through ${scenario}`, async ({
      app,
      browser,
      screen,
    }) => {
      await browser.setViewport({ width: 1280, height: 1000 });
      await app.open("/billing-settings");
      await screen.getByRole("tab", "Settings").click();
      const form = screen.getByRole("form", "Low-balance alert");
      const threshold = form.getByRole("textbox", "Alert threshold (USD)");
      const save = form.getByRole("button", "Save");
      await expect(threshold).toHaveValue("10.00");
      const readsBeforeSave = await browser.evaluate(() =>
        window.alertSettingsFixture.holdSaveAndRead(),
      );

      await threshold.fill("20");
      await save.click();
      await expect(save).toBeDisabled();
      await expect
        .poll(() =>
          browser.evaluate(() =>
            window.alertSettingsFixture.submittedThresholds(),
          ),
        )
        .toEqual(["20000000"]);
      await threshold.fill("25");
      await browser.evaluate(() => window.alertSettingsFixture.releaseSave());
      await expect
        .poll(() =>
          browser.evaluate(() => window.alertSettingsFixture.readCount()),
        )
        .toBe(readsBeforeSave + 1);
      expect(
        await browser.evaluate(() =>
          window.alertSettingsFixture.savedThreshold(),
        ),
      ).toBe("20000000");
      await expect(save).toBeDisabled();
      await expect(threshold).toHaveValue("25");
      await threshold.fill("30");

      await browser.evaluate(
        (fail) => window.alertSettingsFixture.finishRead(fail),
        failConfirmation,
      );
      if (failConfirmation) {
        const retry = screen.getByRole("button", "Try again");
        await expect(retry).toBeVisible();
        await expect(threshold).toHaveValue("30");
        await app.screenshot("alert-confirmation-failed");
        await retry.click();
        await expect(retry).toBeHidden();
      }
      await expect(save).toBeEnabled();
      await expect(threshold).toHaveValue("30");
      await app.screenshot("alert-draft-preserved");

      await save.click();
      await expect(threshold).toHaveValue("30.00");
      await expect(save).toBeEnabled();
      expect(
        await browser.evaluate(() =>
          window.alertSettingsFixture.submittedThresholds(),
        ),
      ).toEqual(["20000000", "30000000"]);
    });
  }

  test("an idle pristine form follows a changed alert threshold", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/billing-settings");
    await screen.getByRole("tab", "Settings").click();
    const threshold = screen.getByRole("textbox", "Alert threshold (USD)");
    await expect(threshold).toHaveValue("10.00");
    await browser.evaluate(() =>
      window.alertSettingsFixture.refreshThreshold("40000000"),
    );
    await expect(threshold).toHaveValue("40.00");
  });
});
