// SPDX-License-Identifier: Apache-2.0
import { type Browser, test, type WebFixtures } from "@e2e-dev/web";
import { expect, type TestFixtures } from "e2e";
import { MOCK_IDS } from "../src/mock/mock-api.ts";
import { PROFILE_INTEGRATION_FIXTURE } from "./profile-integration-fixture.ts";

const missingIntegration =
  "You cannot save this profile because its agent integration is missing.";

type Fixtures = WebFixtures & Pick<TestFixtures, "screen">;

async function openProfile(
  { app, browser, screen }: Fixtures,
  mode: "claude" | "missing",
) {
  await app.open("/");
  await screen.getByRole("button", "Sign in with Delino").tap();
  await browser.evaluate(
    ({ key, mode }) => {
      sessionStorage.setItem(key, mode);
      return null;
    },
    { key: PROFILE_INTEGRATION_FIXTURE, mode },
  );
  await app.open(`/orgs/${MOCK_IDS.acme}/profiles`);
  const row = screen
    .getByRole("listitem")
    .filter({ hasText: "claude-linux ·" });
  await row.getByRole("button", "Edit").tap();
  return screen.getByRole("dialog", "Edit claude-linux");
}

async function releasePage(browser: Browser, index: number, fail = false) {
  await expect
    .poll(() =>
      browser.evaluate(
        () =>
          window.sandrunProfileIntegrationFixture?.continuations.length ?? 0,
      ),
    )
    .toBe(index + 1);
  await browser.evaluate(
    ({ index, fail }) => {
      window.sandrunProfileIntegrationFixture!.continuations[index]!(fail);
      return null;
    },
    { index, fail },
  );
}

async function submitForm(browser: Browser) {
  await browser.evaluate(() => {
    const form = document.querySelector('[role="dialog"] form');
    if (!(form instanceof HTMLFormElement))
      throw new Error("Expected the edit form");
    form.requestSubmit();
    return null;
  });
}

const updateCount = (browser: Browser) =>
  browser.evaluate(
    () => window.sandrunProfileIntegrationFixture!.updates.length,
  );

test("blocks profile updates until a failed integration page is retried", async ({
  app,
  browser,
  screen,
}) => {
  const dialog = await openProfile({ app, browser, screen }, "claude");
  const save = dialog.getByRole("button", "Save changes");
  const name = dialog.getByLabel("Name");
  await name.fill("claude-retry");
  await expect(save).toBeDisabled();
  await submitForm(browser);
  expect(await updateCount(browser)).toBe(0);

  await releasePage(browser, 0, true);
  const retry = dialog.getByRole("button", "Try again");
  await expect(retry).toBeVisible();
  await expect(dialog.getByText(missingIntegration)).toBeHidden();
  await expect(save).toBeDisabled();
  await submitForm(browser);
  expect(await updateCount(browser)).toBe(0);
  await app.screenshot("profile-integration-continuation-retry");

  await retry.tap();
  await releasePage(browser, 1);
  await expect(name).toHaveValue("claude-retry");
  await expect(save).toBeEnabled();
  await save.tap();
  await expect(
    screen.getByText("claude-retry · Agent session · Linux"),
  ).toBeVisible();
  expect(await updateCount(browser)).toBe(1);
});

test("explains why a profile with a missing saved integration cannot be saved", async ({
  app,
  browser,
  screen,
}) => {
  const dialog = await openProfile({ app, browser, screen }, "missing");
  const notice = dialog.getByRole("alert").getByText(missingIntegration);
  const save = dialog.getByRole("button", "Save changes");
  await expect(save).toBeDisabled();
  await expect(notice).toBeHidden();

  await releasePage(browser, 0);
  await expect(notice).toBeVisible();
  await expect(save).toBeDisabled();
  await submitForm(browser);
  expect(await updateCount(browser)).toBe(0);
  await app.screenshot("profile-saved-integration-missing");
});
