// SPDX-License-Identifier: Apache-2.0
import { Code } from "@connectrpc/connect";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

const ACME = "0193a000-0000-7000-8000-000000000001";

const paginationTest = test.extend<{
  pagedList: {
    open(method: string, path: string): Promise<void>;
    release(code: Code): Promise<void>;
    allowRetry(): Promise<void>;
    tokens(): Promise<string[]>;
  };
}>({
  pagedList: async ({ app, browser, screen }, use) => {
    await use({
      async open(method, path) {
        await app.open("/");
        await screen.getByRole("button", "Sign in with Delino").tap();
        await expect(
          screen.getByRole("link", "Acme Robotics Owner"),
        ).toBeVisible();
        await browser.evaluate((name) => {
          sessionStorage.setItem("ownsand-e2e-paged-list", name);
          sessionStorage.setItem("ownsand-e2e-page-error", "hold");
          return true;
        }, method);
        await app.open(path);
      },
      async release(code) {
        await browser.evaluate((errorCode) => {
          sessionStorage.setItem("ownsand-e2e-page-error", String(errorCode));
          window.dispatchEvent(new Event("ownsand-e2e-release-page"));
          return true;
        }, code);
      },
      async allowRetry() {
        await browser.evaluate(() => {
          sessionStorage.removeItem("ownsand-e2e-page-error");
          return true;
        });
      },
      async tokens() {
        return browser.evaluate(
          () =>
            JSON.parse(
              sessionStorage.getItem("ownsand-e2e-page-tokens") ?? "[]",
            ) as string[],
        );
      },
    });
  },
});

describe("continuation errors", () => {
  for (const fixture of [
    {
      method: "ListApiKeys",
      path: "api-keys",
      list: "API keys",
      noun: "API keys",
      row: "ci-runner",
      code: Code.Unauthenticated,
      error: "Your session ended. Sign in again.",
    },
    {
      method: "ListEnrollmentTokens",
      path: "enrollment-tokens",
      list: "Enrollment tokens",
      noun: "enrollment tokens",
      row: "ose_91c2",
      code: Code.NotFound,
      error: "It was not found, or you no longer have access to it.",
    },
  ]) {
    paginationTest(
      `hides loaded ${fixture.noun} on a terminal error`,
      async ({ app, pagedList, screen }) => {
        await pagedList.open(fixture.method, `/orgs/${ACME}/${fixture.path}`);
        await expect(
          screen.getByText(fixture.row, { exact: false }),
        ).toBeVisible();
        await expect(
          screen.getByText(`Loading more ${fixture.noun}`),
        ).toBeVisible();

        await pagedList.release(fixture.code);

        await expect(screen.getByText(fixture.error)).toBeVisible();
        await expect(screen.getByRole("list", fixture.list)).toBeHidden();
        await expect(screen.getByRole("button", /^Revoke/)).toBeHidden();
        await expect(
          screen.getByText(`Some ${fixture.noun} are not shown yet.`),
        ).toBeHidden();
        if (fixture.code === Code.Unauthenticated) {
          await expect(
            screen.getByRole("button", "Sign in with Delino"),
          ).toBeVisible();
        }
        await app.screenshot(`terminal-${fixture.path}`);
      },
    );
  }

  paginationTest(
    "hides loaded organizations in the switcher and on the list",
    async ({ app, pagedList, screen }) => {
      await pagedList.open("ListOrganizations", `/orgs/${ACME}/sandboxes`);
      await screen.getByRole("combobox", "Organization").tap();
      await expect(screen.getByRole("option", "Acme Robotics")).toBeVisible();

      await pagedList.release(Code.Unauthenticated);

      await expect(screen.getByRole("combobox", "Organization")).toBeHidden();
      await expect(screen.getByRole("option", "Acme Robotics")).toBeHidden();
      await screen.getByRole("link", "OwnSand").tap();
      await expect(
        screen.getByRole("button", "Sign in with Delino"),
      ).toBeVisible();
      await expect(screen.getByRole("list", "Your organizations")).toBeHidden();
      await app.screenshot("terminal-organizations");
    },
  );

  paginationTest(
    "discards an open ownership transfer and hides loaded members",
    async ({ app, pagedList, screen }) => {
      await pagedList.open("ListMembers", `/orgs/${ACME}/settings`);
      await screen.getByRole("combobox", "New owner").tap();
      await screen.getByRole("option", "Jun Park").tap();
      await screen.getByRole("button", "Transfer", { exact: true }).tap();
      await expect(
        screen.getByRole("alertdialog", "Make Jun Park the owner?"),
      ).toBeVisible();

      await pagedList.release(Code.PermissionDenied);

      await expect(
        screen.getByText("You do not have permission to do this."),
      ).toBeVisible();
      await expect(
        screen.getByRole("alertdialog", "Make Jun Park the owner?"),
      ).toBeHidden();
      await expect(
        screen.getByRole("button", "Transfer", { exact: true }),
      ).toBeHidden();
      await expect(screen.getByRole("combobox", "New owner")).toBeHidden();
      await expect(
        screen.getByRole("button", "Delete organization"),
      ).toBeVisible();
      await app.screenshot("terminal-owner-picker");
      await screen.getByRole("link", "Members").tap();
      await expect(
        screen.getByText("You do not have permission to do this."),
      ).toBeVisible();
      await expect(screen.getByRole("list", "Members")).toBeHidden();
      await app.screenshot("terminal-members");
    },
  );

  paginationTest(
    "keeps loaded rows and retries the same token after a transient error",
    async ({ app, pagedList, screen }) => {
      await pagedList.open("ListOrganizations", "/");
      await expect(
        screen.getByRole("link", "Acme Robotics Owner"),
      ).toBeVisible();

      await pagedList.release(Code.Unavailable);

      const notice = screen.getByText("Some organizations are not shown yet.");
      const nextRow = screen.getByRole("link", "Northwind Labs Member");
      await expect(notice).toBeVisible();
      await expect(
        screen.getByRole("link", "Acme Robotics Owner"),
      ).toBeVisible();
      await expect(nextRow).toBeHidden();
      await app.screenshot("partial-organizations");

      const failedTokens = await pagedList.tokens();
      expect(failedTokens.filter((token) => token !== "")).toEqual([
        "e2e-next-page",
        "e2e-next-page",
        "e2e-next-page",
      ]);

      await pagedList.allowRetry();
      await screen.getByRole("button", "Try again").tap();
      await expect(nextRow).toBeVisible();
      await expect(nextRow).toHaveCount(1);
      await expect(notice).toBeHidden();
      expect(await pagedList.tokens()).toEqual([
        ...failedTokens,
        failedTokens.at(-1),
      ]);
      await app.screenshot("recovered-organizations");
    },
  );
});
