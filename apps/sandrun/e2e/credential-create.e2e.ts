// SPDX-License-Identifier: Apache-2.0
import { type Browser, describe, test, type WebFixtures } from "@e2e-dev/web";
import { expect, type TestFixtures } from "e2e";
import {
  CREDENTIAL_FIXTURE,
  FIXTURE_INVITATION,
  FIXTURE_KEY,
} from "./credential-fixture-values.ts";

const KEY_NAME = "deploy-bot";
const apiKey = {
  mode: "key",
  path: "/orgs/0193b000-0000-7000-8000-000000000001/api-keys",
  initialText: "claude-on-laptop · Active",
  button: "Create API key",
  form: "Create a personal API key",
  submit: "Create key",
  result: "Your new personal API key",
  label: "API key",
  value: FIXTURE_KEY,
  row: `${KEY_NAME} · Active`,
};
const invitation = {
  mode: "invitation",
  path: "/orgs/0193b000-0000-7000-8000-000000000004/members",
  initialText: "No pending invitations",
  button: "Invite someone",
  form: "Invite someone",
  submit: "Create link",
  result: "Your invitation link",
  label: "Invitation link",
  value: `/invitations/accept#secret=${FIXTURE_INVITATION}`,
  row: "Member invitation",
};

type Fixtures = WebFixtures & Pick<TestFixtures, "screen">;

async function openPage(
  { app, browser, screen }: Fixtures,
  feature: typeof apiKey,
  mode = feature.mode,
) {
  await app.open("/");
  await screen.getByRole("button", "Sign in with Delino").tap();
  await browser.evaluate(
    ({ key, mode }) => {
      sessionStorage.setItem(key, mode);
      return null;
    },
    { key: CREDENTIAL_FIXTURE, mode },
  );
  await app.open(feature.path);
}

async function openForm(
  { browser, screen }: Fixtures,
  feature: typeof apiKey,
  keyName = KEY_NAME,
) {
  await screen.getByRole("button", feature.button).tap();
  const form = screen.getByRole("dialog", feature.form);
  if (feature.mode === "key") {
    await form.getByLabel("Name").tap();
    await browser.keyboard.type(keyName);
  }
  return form;
}

function pageTokens(browser: Browser) {
  return browser.evaluate(() => window.sandrunCredentialFixture!.pageTokens);
}

async function holdNext(browser: Browser, kind: "list" | "create") {
  await browser.evaluate((kind) => {
    window.sandrunCredentialFixture!.holdNext = kind;
    return null;
  }, kind);
}

async function waitUntilHeld(browser: Browser, kind: "list" | "create") {
  await expect
    .poll(() =>
      browser.evaluate(
        (kind) => window.sandrunCredentialFixture?.release[kind] !== undefined,
        kind,
      ),
    )
    .toBe(true);
}

async function release(
  browser: Browser,
  kind: "list" | "create",
  fail = false,
) {
  await browser.evaluate(
    ({ kind, fail }) => {
      window.sandrunCredentialFixture!.release[kind]!(fail);
      return null;
    },
    { kind, fail },
  );
}

describe("credential creation with a pending list", () => {
  test("starts a new API key first page before the old response arrives", async ({
    app,
    browser,
    screen,
  }) => {
    const fixtures = { app, browser, screen };
    await openPage(fixtures, apiKey, "cold-key");
    await waitUntilHeld(browser, "list");
    expect(await pageTokens(browser)).toEqual([""]);

    const form = await openForm(fixtures, apiKey);
    await form.getByRole("button", apiKey.submit).tap();

    const result = screen.getByRole("dialog", apiKey.result);
    await expect.poll(() => pageTokens(browser)).toEqual(["", ""]);
    await expect(result).toBeVisible();
    await result.getByRole("button", "Done").tap();
    const row = screen.getByRole("listitem").filter({ hasText: apiKey.row });
    await expect(row.getByRole("button", "Revoke")).toBeVisible();

    await release(browser, "list");
    await expect
      .poll(() =>
        browser.evaluate(
          () => window.sandrunCredentialFixture!.oldPageDelivered,
        ),
      )
      .toBe(true);
    await expect(row.getByRole("button", "Revoke")).toBeVisible();
    expect(await pageTokens(browser)).toEqual(["", ""]);
    await app.screenshot("api-key-after-late-first-page");
  });

  for (const feature of [apiKey, invitation]) {
    test(`shows the ${feature.mode} result during a refresh and keeps it on refresh failure`, async ({
      app,
      browser,
      screen,
    }) => {
      const fixtures = { app, browser, screen };
      await openPage(fixtures, feature);
      await expect(screen.getByText(feature.initialText)).toBeVisible();
      await holdNext(browser, "list");
      const form = await openForm(fixtures, feature);
      await form.getByRole("button", feature.submit).tap();
      await waitUntilHeld(browser, "list");

      const result = screen.getByRole("dialog", feature.result);
      const value = browser.locator(`code[aria-label="${feature.label}"]`);
      const expected =
        feature.mode === "invitation"
          ? app.baseUrl + feature.value
          : feature.value;
      await expect(form).toBeHidden();
      await expect(result.getByRole("button", "Copy")).toBeVisible();
      await expect(value).toHaveText(expected);
      await app.screenshot(`${feature.mode}-result-during-refresh`);

      await release(browser, "list", true);
      await expect(screen.getByText(/The last refresh failed/)).toBeVisible();
      await expect(value).toHaveText(expected);
      await result.getByRole("button", "Done").tap();
      await expect(result).toBeHidden();
      await screen.getByRole("button", "Try again").tap();
      await expect(
        screen.getByText(feature.row, { exact: false }),
      ).toBeVisible();
      await expect(screen.getByText(/The last refresh failed/)).toBeHidden();
    });

    test(`keeps a failed ${feature.mode} creation retryable without refreshing`, async ({
      app,
      browser,
      screen,
    }) => {
      const fixtures = { app, browser, screen };
      await openPage(fixtures, feature);
      await expect(screen.getByText(feature.initialText)).toBeVisible();
      const initialRequests = await pageTokens(browser);
      await holdNext(browser, "create");
      const form = await openForm(fixtures, feature);
      const submit = form.getByRole("button", feature.submit);
      await submit.tap();
      await waitUntilHeld(browser, "create");
      await release(browser, "create", true);

      await expect(form.getByRole("alert")).toContainText(
        "Fixture create failed",
      );
      await expect(submit).toBeEnabled();
      await expect(screen.getByRole("dialog", feature.result)).toBeHidden();
      expect(await pageTokens(browser)).toEqual(initialRequests);

      await submit.tap();
      await expect(screen.getByRole("dialog", feature.result)).toBeVisible();
      await expect
        .poll(() => pageTokens(browser))
        .toEqual([...initialRequests, ""]);
    });

    test(`refreshes after a dismissed ${feature.mode} creation without showing its result`, async ({
      app,
      browser,
      screen,
    }) => {
      const fixtures = { app, browser, screen };
      await openPage(fixtures, feature);
      await expect(screen.getByText(feature.initialText)).toBeVisible();
      const initialRequests = await pageTokens(browser);
      await holdNext(browser, "create");
      const form = await openForm(fixtures, feature);
      await form.getByRole("button", feature.submit).tap();
      await waitUntilHeld(browser, "create");
      await browser.keyboard.press("Escape");
      await expect(form).toBeHidden();
      await openForm(fixtures, feature, "retained-draft");
      await release(browser, "create");

      await expect
        .poll(() => pageTokens(browser))
        .toEqual([...initialRequests, ""]);
      await expect(form.getByRole("button", feature.submit)).toBeEnabled();
      await expect(screen.getByRole("dialog", feature.result)).toBeHidden();
      if (feature.mode === "key") {
        await expect(form.getByLabel("Name")).toHaveValue("retained-draft");
      }
      await browser.keyboard.press("Escape");
      await expect(
        screen.getByText(feature.row, { exact: false }),
      ).toBeVisible();
      await expect(screen.getByRole("dialog", feature.result)).toBeHidden();
    });
  }
});
