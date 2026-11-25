// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

const incompleteReply = /The organization may have been created/;

const organizationCreate = test.extend<{
  createSubmittedOrganization(reply: string, name: string): Promise<void>;
}>({
  createSubmittedOrganization: async ({ app, browser, screen }, use) => {
    await use(async (reply, name) => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await expect(
        screen.getByRole("link", "Acme Robotics Owner"),
      ).toBeVisible();
      await browser.evaluate((response) => {
        sessionStorage.setItem(
          "ownsand-e2e-organization-create-reply",
          response,
        );
        return null;
      }, reply);
      await screen.getByRole("button", "Create organization").tap();
      await browser.keyboard.type(name);
      await screen.getByRole("button", "Create", { exact: true }).tap();
    });
  },
});

describe("incomplete organization creation replies", () => {
  for (const reply of ["missing-organization", "empty-id"]) {
    organizationCreate(
      `keeps the form open when the response contains ${reply}`,
      async ({ app, browser, screen, createSubmittedOrganization }) => {
        await createSubmittedOrganization(reply, "Fabrikam");

        const dialog = screen.getByRole("dialog", "Create an organization");
        await expect(dialog.getByLabel("Organization name")).toHaveValue(
          "Fabrikam",
        );
        await expect(dialog.getByText(incompleteReply)).toBeVisible();
        await expect(
          dialog.getByText(/check the organization list/),
        ).toBeVisible();
        await expect(browser).toHaveURL("/");
        await app.screenshot(`organization-create-${reply}`);

        await dialog.getByRole("button", "Cancel").tap();
        await expect(screen.getByRole("link", "Fabrikam Owner")).toBeVisible();
        expect(
          await browser.evaluate(() =>
            sessionStorage.getItem("ownsand-e2e-organization-create-count"),
          ),
        ).toBe("1");
        await app.screenshot(`organization-list-after-${reply}`);
      },
    );
  }

  organizationCreate(
    "refreshes after dismissal without changing a newer dialog",
    async ({ app, browser, screen, createSubmittedOrganization }) => {
      await createSubmittedOrganization(
        "late-missing-organization",
        "Fabrikam",
      );
      const dialog = screen.getByRole("dialog", "Create an organization");
      await expect(dialog.getByRole("button", "Create")).toBeDisabled();
      expect(
        await browser.evaluate(() =>
          sessionStorage.getItem("ownsand-e2e-organization-create-count"),
        ),
      ).toBe("1");
      await browser.keyboard.press("Escape");
      await expect(dialog).toBeHidden();

      await screen.getByRole("button", "Create organization").tap();
      await browser.keyboard.type("New draft");
      await browser.evaluate(() => {
        window.dispatchEvent(
          new Event("ownsand-e2e-release-organization-create"),
        );
        return null;
      });

      // Wait for the refresh due to the first request before checking the new form.
      await expect(screen.getByText("Fabrikam", { exact: true })).toBeVisible();
      await expect(dialog.getByLabel("Organization name")).toHaveValue(
        "New draft",
      );
      await expect(dialog.getByText(incompleteReply)).toBeHidden();
      await expect(browser).toHaveURL("/");
      await app.screenshot("organization-create-newer-draft");
    },
  );
});
