// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

describe("automatic top-up settings", () => {
  test("shows the limits that apply to the current switch state", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/auto-top-up.html");
    await screen.getByRole("tab", "Settings").click();
    const form = screen.getByRole("form", "Automatic top-up");
    const amount = form.getByRole("textbox", "Top-up amount (USD)");
    const threshold = form.getByRole("textbox", "Threshold (USD)");
    const cap = form.getByRole("textbox", "Monthly cap (USD)");
    const toggle = form.getByRole("switch", "Automatic top-up");
    const save = form.getByRole("button", "Save");
    const saves = () => browser.evaluate(() => window.autoTopUpFixture.saves());
    const disabledGuidance = form.getByText(
      "Enter amounts of $0.00 or more in whole cents. Blank fields save as $0.00. Saved values are available when you turn automatic top-up on.",
    );
    const enabledGuidance = [
      form.getByText("At least $10.00.", { exact: true }),
      form.getByText("$25.00 to $10,000.00. Tax is added on top.", {
        exact: true,
      }),
      form.getByText("Required. At least the top-up amount.", { exact: true }),
    ];

    await toggle.click();
    await expect(disabledGuidance).toBeVisible();
    await expect(
      form.getByText("Tax is added on top.", { exact: true }),
    ).toBeVisible();
    for (const guidance of enabledGuidance) {
      await expect(guidance).toBeHidden();
    }

    await threshold.fill("");
    await amount.fill("5");
    await cap.fill("");
    await save.click();
    await expect(amount).toHaveValue("5.00");
    await expect(save).toBeEnabled();

    await amount.fill("15000");
    await save.click();
    await expect(amount).toHaveValue("15000.00");
    await expect(save).toBeEnabled();
    expect(await saves()).toEqual([
      { enabled: false, threshold: "0", amount: "5000000", cap: "0" },
      { enabled: false, threshold: "0", amount: "15000000000", cap: "0" },
    ]);

    await toggle.click();
    await expect(disabledGuidance).toBeHidden();
    for (const guidance of enabledGuidance) {
      await expect(guidance).toBeVisible();
    }
    await save.click();
    await expect(form.getByText("Enter at most $10,000.00.")).toBeVisible();
    expect(await saves()).toHaveLength(2);

    await threshold.fill("10");
    await amount.fill("25");
    await save.click();
    await expect(form.getByText("Enter an amount.")).toBeVisible();
    expect(await saves()).toHaveLength(2);

    await cap.fill("24.99");
    await save.click();
    await expect(
      form.getByText("Enter at least the top-up amount ($25.00)."),
    ).toBeVisible();
    expect(await saves()).toHaveLength(2);

    await cap.fill("25");
    await save.click();
    await expect(amount).toHaveValue("25.00");
    expect(await saves()).toHaveLength(3);
    expect((await saves())[2]).toEqual({
      enabled: true,
      threshold: "10000000",
      amount: "25000000",
      cap: "25000000",
    });
  });

  test("keeps newer edits through the save and confirming read", async ({
    app,
    browser,
    screen,
  }) => {
    await browser.setViewport({ width: 1280, height: 1000 });
    await app.open("/auto-top-up.html");
    await screen.getByRole("tab", "Settings").click();
    const form = screen.getByRole("form", "Automatic top-up");
    const amount = form.getByRole("textbox", "Top-up amount (USD)");
    const threshold = form.getByRole("textbox", "Threshold (USD)");
    const cap = form.getByRole("textbox", "Monthly cap (USD)");
    const toggle = form.getByRole("switch", "Automatic top-up");
    const save = form.getByRole("button", "Save");

    await amount.fill("60");
    const readsBeforeSave = await browser.evaluate(() =>
      window.autoTopUpFixture.holdSave(true),
    );
    await save.click();
    await expect(save).toBeDisabled();
    await expect
      .poll(() => browser.evaluate(() => window.autoTopUpFixture.saves()))
      .toEqual([
        {
          enabled: true,
          threshold: "10000000",
          amount: "60000000",
          cap: "200000000",
        },
      ]);

    await threshold.fill("15");
    await amount.fill("75");
    await cap.fill("250");
    await toggle.click();
    await expect(toggle).toHaveAttribute("aria-checked", "false");
    await toggle.click();
    await expect(toggle).toHaveAttribute("aria-checked", "true");
    await browser.evaluate(() => window.autoTopUpFixture.releaseSave());
    await expect
      .poll(() => browser.evaluate(() => window.autoTopUpFixture.readCount()))
      .toBe(readsBeforeSave + 1);
    await expect(save).toBeDisabled();
    await expect(threshold).toHaveValue("15");
    await expect(amount).toHaveValue("75");
    await expect(cap).toHaveValue("250");
    await amount.fill("80");
    await app.screenshot("automatic-top-up-confirmation-pending");

    await browser.evaluate(() => window.autoTopUpFixture.releaseConfirmation());
    await expect(save).toBeEnabled();
    await expect(threshold).toHaveValue("15");
    await expect(amount).toHaveValue("80");
    await expect(cap).toHaveValue("250");
    await app.screenshot("automatic-top-up-draft-preserved");

    await save.click();
    await expect(amount).toHaveValue("80.00");
    const saves = await browser.evaluate(() => window.autoTopUpFixture.saves());
    expect(saves).toHaveLength(2);
    expect(saves[1]).toEqual({
      enabled: true,
      threshold: "15000000",
      amount: "80000000",
      cap: "250000000",
    });
  });

  test("canonicalizes an equivalent save when the draft is still current", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/auto-top-up.html");
    await screen.getByRole("tab", "Settings").click();
    const form = screen.getByRole("form", "Automatic top-up");
    const amount = form.getByRole("textbox", "Top-up amount (USD)");
    const toggle = form.getByRole("switch", "Automatic top-up");
    const save = form.getByRole("button", "Save");

    await amount.fill("50");
    await save.click();
    await expect(amount).toHaveValue("50.00");
    await expect(save).toBeEnabled();

    await amount.fill("50");
    await browser.evaluate(() => window.autoTopUpFixture.holdSave());
    await save.click();
    await toggle.click();
    await browser.evaluate(() => window.autoTopUpFixture.releaseSave());
    await expect(save).toBeEnabled();
    await expect(amount).toHaveValue("50");
    await expect(toggle).toHaveAttribute("aria-checked", "false");
    await save.click();
    await expect(amount).toHaveValue("50.00");
    const saves = await browser.evaluate(() => window.autoTopUpFixture.saves());
    expect(saves).toHaveLength(3);
    expect(saves[2]?.enabled).toBe(false);
  });

  test("keeps the newer draft available to retry after a failure", async ({
    app,
    browser,
    screen,
  }) => {
    await browser.setViewport({ width: 1280, height: 1000 });
    await app.open("/auto-top-up.html");
    await screen.getByRole("tab", "Settings").click();
    const form = screen.getByRole("form", "Automatic top-up");
    const amount = form.getByRole("textbox", "Top-up amount (USD)");
    const save = form.getByRole("button", "Save");

    await amount.fill("60");
    await browser.evaluate(() => window.autoTopUpFixture.holdSave());
    await save.click();
    await amount.fill("75");
    await browser.evaluate(() => window.autoTopUpFixture.rejectSave());
    await expect(
      form.getByText(/Delino Accounts is not available/),
    ).toBeVisible();
    await expect(amount).toHaveValue("75");
    await app.screenshot("automatic-top-up-save-failed");
    await save.click();
    await expect(amount).toHaveValue("75.00");
    const saves = await browser.evaluate(() => window.autoTopUpFixture.saves());
    expect(saves.map((request) => request?.amount)).toEqual([
      "60000000",
      "75000000",
    ]);
  });

  test("resets the draft only when an independent refresh changes editable values", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/auto-top-up.html");
    await screen.getByRole("tab", "Settings").click();
    const form = screen.getByRole("form", "Automatic top-up");
    const amount = form.getByRole("textbox", "Top-up amount (USD)");

    await amount.fill("75");
    await browser.evaluate(() => window.autoTopUpFixture.refreshCounters());
    await expect(form.getByText("$30.00")).toBeVisible();
    await expect(form.getByText("1", { exact: true })).toBeVisible();
    await expect(amount).toHaveValue("75");

    await browser.evaluate(() => window.autoTopUpFixture.refreshAmount());
    await expect(amount).toHaveValue("40.00");
  });
});
