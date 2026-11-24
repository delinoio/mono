// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

describe("automatic top-up settings", () => {
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
