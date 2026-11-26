// SPDX-License-Identifier: Apache-2.0
import { describe, test, type Browser } from "@e2e-dev/web";
import { expect } from "e2e";

// ID of the organization the signed-in mock user owns.
const ACME = "0193a000-0000-7000-8000-000000000001";

function credentialState(browser: Browser, name: string) {
  return browser.evaluate(
    (key) => sessionStorage.getItem(`ownsand-e2e-credential-${key}`),
    name,
  );
}

function releaseCredentialRequest(
  browser: Browser,
  name: "create" | "list" | "refresh",
) {
  return browser.evaluate((request) => {
    const key = `ownsand-e2e-credential-${request}`;
    sessionStorage.setItem(key, "release");
    window.dispatchEvent(new Event(`${key}-release`));
    return null;
  }, name);
}

const credentialTest = test.extend<{
  openCredentialPage(
    method: string,
    path: string,
    mode?: "failed-create" | "held-refresh" | "held-first",
  ): Promise<void>;
}>({
  openCredentialPage: async ({ app, browser, screen }, use) => {
    await use(async (method, path, mode = "held-first") => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await expect(
        screen.getByRole("link", "Acme Robotics Owner"),
      ).toBeVisible();
      await browser.evaluate(
        ({ method, mode }) => {
          sessionStorage.setItem("ownsand-e2e-credential-method", method);
          if (mode === "failed-create") {
            sessionStorage.setItem(
              "ownsand-e2e-credential-create-fails",
              "true",
            );
          }
          if (mode === "held-refresh") {
            sessionStorage.setItem(
              "ownsand-e2e-credential-hold-refresh",
              "true",
            );
          }
          return null;
        },
        { method, mode },
      );
      await app.open(`/orgs/${ACME}/${path}`);
    });
  },
});

const fixtures = [
  {
    path: "api-keys",
    method: "ListApiKeys",
    noun: "API keys",
    list: "API keys",
    open: "Create API key",
    form: "Create an API key",
    submit: "Create key",
    secret: "Your new API key",
    value: "osk_fixture",
    row: "new-key",
    empty: "No API keys",
    revoke: "Revoke key",
  },
  {
    path: "enrollment-tokens",
    method: "ListEnrollmentTokens",
    noun: "enrollment tokens",
    list: "Enrollment tokens",
    open: "Create token",
    form: "Create an enrollment token",
    submit: "Create token",
    secret: "Your new enrollment token",
    value: "ose_fixture",
    row: "Not used",
    empty: "No enrollment tokens",
    revoke: "Revoke token",
  },
  {
    path: "members",
    method: "ListInvitations",
    noun: "invitations",
    list: "Pending invitations",
    open: "Invite someone",
    form: "Invite someone",
    submit: "Create link",
    secret: "Your invitation link",
    value: "/invitations/accept#secret=osi_fixture",
    row: "Member invitation",
    empty: "No pending invitations",
    revoke: "Revoke invitation",
  },
];

describe("credential creation during the first list request", () => {
  for (const fixture of fixtures) {
    for (const dismiss of [false, true]) {
      credentialTest(
        `refreshes ${fixture.noun} with the create form ${dismiss ? "dismissed" : "open"}`,
        async ({ app, browser, screen, openCredentialPage }) => {
          await openCredentialPage(fixture.method, fixture.path);
          await expect(
            screen.getByRole("status", `Loading ${fixture.noun}`),
          ).toBeVisible();
          await screen.getByRole("button", fixture.open).tap();
          const form = screen.getByRole("dialog", fixture.form);
          if (fixture.path === "api-keys") {
            await form.getByRole("textbox", "Name").fill("new-key");
          }
          const submit = form.getByRole("button", fixture.submit);
          await submit.tap();
          await expect(submit).toBeDisabled();
          expect(await credentialState(browser, "list")).toBe("held");
          expect(await credentialState(browser, "tokens")).toBe('[""]');

          if (dismiss) {
            await browser.keyboard.press("Escape");
            await expect(form).toBeHidden();
            await expect(browser.locator(".dui-dialog-backdrop")).toHaveCount(
              0,
            );
            await screen.getByRole("button", fixture.open).tap();
          }
          await releaseCredentialRequest(browser, "create");
          const secret = screen.getByRole("dialog", fixture.secret);
          if (!dismiss) {
            await expect(secret).toBeVisible();
            await secret.getByRole("button", "Done").tap();
          }
          // Close the new opening after its create has refreshed the list.
          // The modal makes the list inert, so wait for the new row by DOM.
          if (dismiss) {
            await expect(
              browser.locator(`ul[aria-label="${fixture.list}"] > li`),
            ).toHaveCount(2);
            await expect(form).toBeVisible();
            await expect(
              form.getByRole("button", fixture.submit),
            ).toBeEnabled();
            await expect(secret).toBeHidden();
            await browser.keyboard.press("Escape");
          }
          const rows = screen
            .getByRole("list", fixture.list)
            .getByRole("listitem");
          await expect(rows).toHaveCount(2);
          const row = rows.last();
          const revoke = row.getByRole("button", /^Revoke/);
          await expect(revoke).toBeVisible();
          expect(await credentialState(browser, "tokens")).toBe('["",""]');
          expect(await credentialState(browser, "list")).toBe("held");
          await releaseCredentialRequest(browser, "list");
          expect(await credentialState(browser, "list")).toBe("delivered");
          await expect(secret).toBeHidden();
          // The closing modal's backdrop can still intercept a tap after
          // its dialog disappears from the accessibility tree.
          await expect(browser.locator(".dui-dialog-backdrop")).toHaveCount(0);
          await expect(rows).toHaveCount(2);
          await expect(row).toContainText(fixture.row);
          await app.screenshot(
            `fresh-${fixture.path}-${dismiss ? "dismissed" : "open"}`,
          );

          await revoke.tap();
          const confirm = screen.getByRole("alertdialog");
          await expect(confirm).toBeVisible();
          await confirm.getByRole("button", fixture.revoke).tap();
          if (fixture.path === "members") {
            await expect(rows).toHaveCount(1);
          } else {
            await expect(row).toContainText("Revoked");
            await expect(revoke).toBeHidden();
          }
        },
      );
    }

    credentialTest(
      `keeps the first ${fixture.noun} request when creation fails`,
      async ({ browser, screen, openCredentialPage }) => {
        await openCredentialPage(fixture.method, fixture.path, "failed-create");
        await expect(
          screen.getByRole("status", `Loading ${fixture.noun}`),
        ).toBeVisible();
        await screen.getByRole("button", fixture.open).tap();
        const form = screen.getByRole("dialog", fixture.form);
        if (fixture.path === "api-keys") {
          await form.getByRole("textbox", "Name").fill("new-key");
        }
        await form.getByRole("button", fixture.submit).tap();
        await releaseCredentialRequest(browser, "create");
        await expect(form.getByText("Synthetic create failure")).toBeVisible();
        await browser.keyboard.press("Escape");
        await releaseCredentialRequest(browser, "list");
        await expect(screen.getByText(fixture.empty)).toBeVisible();
        await expect(screen.getByRole("dialog", fixture.secret)).toBeHidden();
        expect(await credentialState(browser, "tokens")).toBe('[""]');
      },
    );
  }
});

describe("one-time results during list refresh", () => {
  for (const fixture of fixtures) {
    credentialTest(
      `shows and copies the new value while ${fixture.noun} refresh`,
      async ({ app, browser, screen, openCredentialPage }) => {
        await openCredentialPage(fixture.method, fixture.path, "held-refresh");
        const rows = screen
          .getByRole("list", fixture.list)
          .getByRole("listitem");
        await expect(rows).toHaveCount(1);
        await screen.getByRole("button", fixture.open).tap();
        const form = screen.getByRole("dialog", fixture.form);
        if (fixture.path === "api-keys") {
          await form.getByRole("textbox", "Name").fill("new-key");
        }
        const submit = form.getByRole("button", fixture.submit);
        await submit.tap();
        await expect(submit).toBeDisabled();
        await releaseCredentialRequest(browser, "create");

        const result = screen.getByRole("dialog", fixture.secret);
        await expect(form).toBeHidden();
        await expect(result).toContainText(fixture.value);
        expect(await credentialState(browser, "refresh")).toBe("held");
        // Headless Chromium can deny system clipboard access. Capture the
        // fixture text at the browser clipboard API to check the Copy action.
        const origin = await browser.evaluate(() => {
          Object.defineProperty(navigator.clipboard, "writeText", {
            value: async (value: string) =>
              sessionStorage.setItem("ownsand-e2e-credential-copied", value),
          });
          return location.origin;
        });
        const expected =
          fixture.path === "members" ? origin + fixture.value : fixture.value;
        const copy = result.getByRole("button", "Copy");
        await copy.tap();
        expect(await credentialState(browser, "copied")).toBe(expected);
        await app.screenshot(`credential-before-refresh-${fixture.path}`);

        await releaseCredentialRequest(browser, "refresh");
        await expect(browser.locator(".notice-error")).toContainText(
          "The last refresh failed",
        );
        await browser.evaluate(() => {
          sessionStorage.removeItem("ownsand-e2e-credential-copied");
          return null;
        });
        await copy.tap();
        expect(await credentialState(browser, "copied")).toBe(expected);
        await expect(result).toContainText(fixture.value);
        await result.getByRole("button", "Done").tap();

        await expect(rows).toHaveCount(1);
        const retry = screen.getByRole("button", "Try again");
        await expect(retry).toBeVisible();
        await expect(browser.locator(".dui-dialog-backdrop")).toHaveCount(0);
        await app.screenshot(`credential-refresh-failed-${fixture.path}`);
        await retry.tap();
        await expect(rows).toHaveCount(2);
        await expect(rows.last()).toContainText(fixture.row);
        await expect(rows.last().getByRole("button", /^Revoke/)).toBeVisible();
        await expect(retry).toBeHidden();
        await expect(result).toBeHidden();
        expect(await credentialState(browser, "tokens")).toBe('["","",""]');
      },
    );
  }
});
