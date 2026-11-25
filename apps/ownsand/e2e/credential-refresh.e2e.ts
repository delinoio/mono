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

function releaseCredentialRequest(browser: Browser, name: "create" | "list") {
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
    failCreate?: boolean,
  ): Promise<void>;
}>({
  openCredentialPage: async ({ app, browser, screen }, use) => {
    await use(async (method, path, failCreate = false) => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await expect(
        screen.getByRole("link", "Acme Robotics Owner"),
      ).toBeVisible();
      await browser.evaluate(
        ({ method, failCreate }) => {
          sessionStorage.setItem("ownsand-e2e-credential-method", method);
          if (failCreate) {
            sessionStorage.setItem(
              "ownsand-e2e-credential-create-fails",
              "true",
            );
          }
          return null;
        },
        { method, failCreate },
      );
      await app.open(`/orgs/${ACME}/${path}`);
    });
  },
});

describe("credential creation during the first list request", () => {
  for (const fixture of [
    {
      path: "api-keys",
      method: "ListApiKeys",
      noun: "API keys",
      list: "API keys",
      open: "Create API key",
      form: "Create an API key",
      submit: "Create key",
      secret: "Your new API key",
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
      revoke: "Revoke token",
    },
  ]) {
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
          }
          await releaseCredentialRequest(browser, "create");
          const secret = screen.getByRole("dialog", fixture.secret);
          if (!dismiss) {
            await expect(secret).toBeVisible();
            await secret.getByRole("button", "Done").tap();
          }
          // The base mock already has a seeded credential. The new row sorts last.
          const row = screen
            .getByRole("list", fixture.list)
            .getByRole("listitem")
            .last();
          const revoke = row.getByRole("button", /^Revoke/);
          await expect(revoke).toBeVisible();
          expect(await credentialState(browser, "tokens")).toBe('["",""]');
          expect(await credentialState(browser, "list")).toBe("held");
          await releaseCredentialRequest(browser, "list");
          expect(await credentialState(browser, "list")).toBe("delivered");
          await expect(secret).toBeHidden();
          await expect(row).toContainText(
            fixture.path === "api-keys" ? "new-key" : "Not used",
          );
          await app.screenshot(
            `fresh-${fixture.path}-${dismiss ? "dismissed" : "open"}`,
          );

          await revoke.tap();
          await screen
            .getByRole("alertdialog")
            .getByRole("button", fixture.revoke)
            .tap();
          await expect(row).toContainText("Revoked");
          await expect(revoke).toBeHidden();
        },
      );
    }

    credentialTest(
      `keeps the first ${fixture.noun} request when creation fails`,
      async ({ browser, screen, openCredentialPage }) => {
        await openCredentialPage(fixture.method, fixture.path, true);
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
        await expect(screen.getByText(`No ${fixture.noun}`)).toBeVisible();
        expect(await credentialState(browser, "tokens")).toBe('[""]');
      },
    );
  }
});
