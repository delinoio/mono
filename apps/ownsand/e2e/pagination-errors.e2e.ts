// SPDX-License-Identifier: Apache-2.0
import { Code } from "@connectrpc/connect";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

const ACME = "0193a000-0000-7000-8000-000000000001";
const NORTHWIND = "0193a000-0000-7000-8000-000000000002";

const paginationTest = test.extend<{
  pagedList: {
    open(
      method: string,
      path: string,
      options?: { hold?: boolean; failFirstSandboxPage?: boolean },
    ): Promise<void>;
    release(code: Code): Promise<void>;
    allowRetry(): Promise<void>;
    tokens(): Promise<string[]>;
  };
}>({
  pagedList: async ({ app, browser, screen }, use) => {
    await use({
      async open(
        method,
        path,
        { hold = true, failFirstSandboxPage = false } = {},
      ) {
        await app.open("/");
        await screen.getByRole("button", "Sign in with Delino").tap();
        await expect(
          screen.getByRole("link", "Acme Robotics Owner"),
        ).toBeVisible();
        await browser.evaluate(
          ({ name, hold, failFirstSandboxPage }) => {
            sessionStorage.setItem("ownsand-e2e-paged-list", name);
            if (hold) sessionStorage.setItem("ownsand-e2e-page-error", "hold");
            if (failFirstSandboxPage) {
              sessionStorage.setItem(
                "ownsand-e2e-sandbox-first-page-error",
                "true",
              );
            }
            return true;
          },
          { name: method, hold, failFirstSandboxPage },
        );
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
          sessionStorage.removeItem("ownsand-e2e-sandbox-first-page-error");
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

describe("sandbox pagination errors", () => {
  const path = `/orgs/${ACME}/sandboxes`;

  for (const [code, message] of [
    [Code.NotFound, "It was not found, or you no longer have access to it."],
    [Code.PermissionDenied, "You do not have permission to do this."],
    [Code.Unauthenticated, "Your session ended. Sign in again."],
  ] as const) {
    paginationTest(
      `hides the sandbox list and Show more after ${Code[code]}`,
      async ({ app, pagedList, screen }) => {
        await pagedList.open("ListSandboxes", path);
        const list = screen.getByRole("region", "Sandbox list");
        const more = screen.getByRole("button", "Show more");
        await expect(list.getByRole("link")).toHaveCount(1);
        const initialRequests = await pagedList.tokens();
        await more.tap();
        await pagedList.release(code);

        await expect(screen.getByText(message)).toBeVisible();
        await app.screenshot(`sandbox-terminal-${Code[code]}`);
        await expect(list).toHaveCount(0);
        await expect(more).toHaveCount(0);
        await expect(screen.getByRole("heading", "Sandboxes")).toBeVisible();
        await expect(screen.getByRole("combobox", "State")).toBeVisible();
        await expect(screen.getByRole("combobox", "Device")).toBeVisible();
        if (code === Code.Unauthenticated) {
          await expect(
            screen.getByRole("button", "Sign in with Delino"),
          ).toBeVisible();
        }
        expect(await pagedList.tokens()).toEqual([
          ...initialRequests,
          "e2e-next-page",
        ]);
      },
    );
  }

  paginationTest(
    "keeps sandbox rows during a continuation error and retries the same token",
    async ({ app, pagedList, screen }) => {
      await pagedList.open("ListSandboxes", path);
      const list = screen.getByRole("region", "Sandbox list");
      const more = screen.getByRole("button", "Show more");
      const error = screen.getByText("Something went wrong. Try again.");
      await expect(more).toBeVisible();
      const initialRequests = await pagedList.tokens();
      await more.tap();
      await pagedList.release(Code.Internal);

      await expect(error).toBeVisible();
      await expect(list.getByRole("link")).toHaveCount(1);
      await expect(more).toBeVisible();
      await app.screenshot("sandbox-continuation-error");

      await pagedList.allowRetry();
      await screen.getByRole("button", "Try again").tap();
      await expect(list.getByRole("link")).toHaveCount(3);
      await expect(error).toHaveCount(0);
      await expect(more).toHaveCount(0);
      expect(await pagedList.tokens()).toEqual([
        ...initialRequests,
        "e2e-next-page",
        "e2e-next-page",
      ]);
      await app.screenshot("sandbox-continuation-recovered");
    },
  );

  paginationTest(
    "recovers the first sandbox page using an empty token",
    async ({ app, pagedList, screen }) => {
      await pagedList.open("ListSandboxes", path, {
        hold: false,
        failFirstSandboxPage: true,
      });
      const list = screen.getByRole("region", "Sandbox list");
      const more = screen.getByRole("button", "Show more");
      const error = screen.getByText("Something went wrong. Try again.");
      await expect(error).toBeVisible();
      await expect(list).toHaveCount(0);
      await expect(more).toHaveCount(0);
      await app.screenshot("sandbox-first-page-error");

      const failedRequests = await pagedList.tokens();
      expect(failedRequests.length).toBeGreaterThan(0);
      expect(failedRequests.every((token) => token === "")).toBe(true);
      await pagedList.allowRetry();
      await screen.getByRole("button", "Try again").tap();
      await expect(list.getByRole("link")).toHaveCount(1);
      await expect(more).toBeVisible();
      await expect(error).toHaveCount(0);
      expect(await pagedList.tokens()).toEqual([...failedRequests, ""]);
      await app.screenshot("sandbox-first-page-recovered");
    },
  );
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
      await expect(screen.getByText(/The last refresh failed/)).toBeHidden();
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

const refreshTest = test.extend<{
  refresh: {
    open(path: string, method: string, mutation: string): Promise<void>;
    fail(): Promise<void>;
    recover(): Promise<void>;
  };
}>({
  refresh: async ({ app, browser, screen }, use) => {
    await use({
      async open(path, method, mutation) {
        await app.open("/");
        await screen.getByRole("button", "Sign in with Delino").tap();
        await expect(
          screen.getByRole("link", "Acme Robotics Owner"),
        ).toBeVisible();
        await browser.evaluate(
          ({ method, mutation }) => {
            sessionStorage.setItem("ownsand-e2e-refresh-list", method);
            sessionStorage.setItem("ownsand-e2e-tracked-mutation", mutation);
            return true;
          },
          { method, mutation },
        );
        await app.open(`/orgs/${ACME}/${path}`);
      },
      async fail() {
        await browser.evaluate(async (code) => {
          sessionStorage.setItem("ownsand-e2e-refresh-error", String(code));
          // Treat the list as stale when React Query handles the visibility event.
          const now = Date.now;
          Date.now = () => now() + 11_000;
          try {
            window.dispatchEvent(new Event("visibilitychange"));
            // React Query checks staleness after resuming paused mutations.
            await Promise.resolve();
          } finally {
            Date.now = now;
          }
          return true;
        }, Code.PermissionDenied);
      },
      async recover() {
        expect(
          await browser.evaluate(() => {
            sessionStorage.removeItem("ownsand-e2e-refresh-error");
            return JSON.parse(
              sessionStorage.getItem("ownsand-e2e-mutation-calls") ?? "[]",
            );
          }),
        ).toEqual([]);
        await screen.getByRole("button", "Try again").tap();
      },
    });
  },
});

describe("terminal full refresh errors", () => {
  const error = "You do not have permission to do this.";

  for (const fixture of [
    {
      path: "api-keys",
      list: "API keys",
      method: "ListApiKeys",
      mutation: "RevokeApiKey",
      action: /^Revoke API key/,
    },
    {
      path: "enrollment-tokens",
      list: "Enrollment tokens",
      method: "ListEnrollmentTokens",
      mutation: "RevokeEnrollmentToken",
      action: /^Revoke enrollment token/,
    },
    {
      path: "members",
      list: "Pending invitations",
      method: "ListInvitations",
      mutation: "RevokeInvitation",
      action: /^Revoke Member invitation/,
    },
    {
      path: "members",
      list: "Members",
      method: "ListMembers",
      mutation: "RemoveMember",
      action: "Remove Jun Park",
    },
  ]) {
    refreshTest(
      `dismisses the ${fixture.list} confirmation`,
      async ({ app, refresh, screen }) => {
        await refresh.open(fixture.path, fixture.method, fixture.mutation);
        const list = screen.getByRole("list", fixture.list);
        const dialog = screen.getByRole("alertdialog");
        await list.getByRole("button", fixture.action).tap();
        await expect(dialog).toBeVisible();

        await refresh.fail();
        await expect(dialog).toBeHidden();
        await expect(screen.getByText(error)).toBeVisible();
        await expect(list).toBeHidden();
        await app.screenshot(`terminal-refresh-${fixture.method}`);

        await refresh.recover();
        await expect(list).toBeVisible();
        await expect(dialog).toBeHidden();
      },
    );
  }

  refreshTest(
    "clears the chosen owner and leaves deletion available",
    async ({ app, refresh, screen }) => {
      await refresh.open("settings", "ListMembers", "TransferOwnership");
      const picker = screen.getByRole("combobox", "New owner");
      const transfer = screen.getByRole("button", "Transfer", { exact: true });
      const dialog = screen.getByRole(
        "alertdialog",
        "Make Jun Park the owner?",
      );
      await picker.tap();
      await screen.getByRole("option", "Jun Park").tap();
      await transfer.tap();
      await expect(dialog).toBeVisible();

      await refresh.fail();
      await expect(dialog).toBeHidden();
      await expect(screen.getByText(error)).toBeVisible();
      await expect(picker).toBeHidden();
      await expect(transfer).toBeHidden();
      await expect(
        screen.getByRole("button", "Delete organization"),
      ).toBeVisible();
      await app.screenshot("terminal-refresh-owner-picker");

      await refresh.recover();
      await expect(picker).toHaveText("Choose a member");
      await expect(transfer).toBeDisabled();
      await expect(dialog).toBeHidden();
      await picker.tap();
      await expect(screen.getByRole("option", "Jun Park")).toBeVisible();
    },
  );
});

describe("device inventory pagination", () => {
  paginationTest(
    "opens, places on and filters by a device beyond the first page",
    async ({ app, browser, pagedList, screen }) => {
      await pagedList.open("ListDevices", `/orgs/${ACME}/devices`, {
        hold: false,
      });
      const device = screen.getByRole("link", "Device 201");
      await expect(device).toHaveCount(1);
      await expect(
        screen.getByRole("region", "Device list").getByRole("link"),
      ).toHaveCount(201);
      const path = await browser.evaluate(() => {
        const link = [...document.links].find(
          (a) => a.textContent === "Device 201",
        )!;
        link.scrollIntoView({ block: "center" });
        return link.pathname;
      });
      await app.screenshot("device-from-second-page");

      await device.tap();
      await expect(browser).toHaveURL(path);
      await expect(screen.getByRole("heading", "Device 201")).toBeVisible();

      await screen.getByRole("link", "Sandboxes", { exact: true }).tap();
      await screen.getByRole("button", "Create sandbox").tap();
      const dialog = screen.getByRole("dialog", "Create a sandbox");
      const placement = dialog.getByRole("combobox", "Device");
      await expect(placement).toHaveText("Any device that fits");
      await placement.tap();
      const option = screen.getByRole("option", "Device 201 (Online)");
      await expect(option).toHaveCount(1);
      await expect(screen.getByRole("option")).toHaveCount(202);
      await option.tap();
      await expect(placement).toHaveText("Device 201 (Online)");
      await app.screenshot("placement-on-second-page-device");
      await dialog.getByRole("button", "Create", { exact: true }).tap();
      await expect(browser).toHaveURL(/\/sandboxes\/[0-9a-f-]+$/);

      await screen.getByRole("link", "Sandboxes", { exact: true }).tap();
      await expect(screen.getByRole("cell", "Device 201")).toBeVisible();
      const filter = screen.getByRole("combobox", "Device");
      await expect(filter).toHaveText("All devices");
      await filter.tap();
      await expect(screen.getByRole("option", "Device 201")).toHaveCount(1);
      await expect(screen.getByRole("option")).toHaveCount(202);
      await screen.getByRole("option", "Device 201").tap();
      await expect(screen.getByRole("cell", "build-box-1")).toBeHidden();
      await expect(screen.getByRole("cell", "Device 201")).toBeVisible();
    },
  );

  paginationTest(
    "keeps loaded device choices, retries the cursor once and scopes results by organization",
    async ({ app, browser, pagedList, screen }) => {
      await pagedList.open("ListDevices", `/orgs/${ACME}/sandboxes`);
      await expect(screen.getByText("Loading more devices")).toBeVisible();
      await pagedList.release(Code.Unavailable);
      const notice = screen.getByText("Some devices are not shown yet.");
      await expect(notice).toBeVisible();
      const filter = screen.getByRole("combobox", "Device");
      await filter.tap();
      await expect(screen.getByRole("option", "build-box-1")).toBeVisible();
      await expect(screen.getByRole("option", "Device 201")).toBeHidden();
      await browser.keyboard.press("Escape");

      const dialog = screen.getByRole("dialog", "Create a sandbox");
      await screen.getByRole("button", "Create sandbox").tap();
      const placement = dialog.getByRole("combobox", "Device");
      await placement.tap();
      await expect(
        screen.getByRole("option", "build-box-1 (Online)"),
      ).toBeVisible();
      await browser.keyboard.press("Escape");
      await app.screenshot("partial-device-picker");
      const failedTokens = await pagedList.tokens();
      const continuations = failedTokens.filter((token) => token !== "");
      const token = continuations[0];
      expect(token).toBeTruthy();
      expect(continuations).toEqual([token, token, token]);

      await pagedList.allowRetry();
      await dialog.getByRole("button", "Try again").tap();
      await expect(notice).toBeHidden();
      await placement.tap();
      const lastDevice = screen.getByRole("option", "Device 201 (Online)");
      await expect(lastDevice).toHaveCount(1);
      expect(await pagedList.tokens()).toEqual([...failedTokens, token]);
      await lastDevice.tap();
      await dialog.getByRole("button", "Cancel").tap();
      await expect(screen.getByRole("button", "Create sandbox")).toBeFocused();

      const northwindSandboxes = `/orgs/${NORTHWIND}/sandboxes`;
      await browser.evaluate((path) => {
        window.history.pushState(null, "", path);
        window.dispatchEvent(new PopStateEvent("popstate"));
        return true;
      }, northwindSandboxes);
      await expect(browser).toHaveURL(northwindSandboxes);
      await expect(screen.getByRole("heading", "Sandboxes")).toBeVisible();
      await filter.tap();
      await expect(screen.getByRole("option", "All devices")).toBeVisible();
      await expect(screen.getByRole("option", "build-box-1")).toBeHidden();
      await expect(screen.getByRole("option", "Device 201")).toBeHidden();
      const organizationTokens = (await pagedList.tokens()).slice(
        failedTokens.length + 1,
      );
      expect(organizationTokens.length).toBeGreaterThan(0);
      expect(organizationTokens.every((token) => token === "")).toBe(true);
    },
  );
});
