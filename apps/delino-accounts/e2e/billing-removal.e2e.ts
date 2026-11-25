// SPDX-License-Identifier: Apache-2.0
import { create, toBinary } from "@bufbuild/protobuf";
import {
  BillingAccountRole,
  GetBillingAccountResponseSchema,
  ListBillingAccountsResponseSchema,
} from "@delino/accounts-protos/v1/billing_account_pb";
import { type Browser, describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { API_ORIGIN, seedFixtureSession } from "./fixture-session.ts";

const ACCOUNT_A = {
  billingAccountId: "0193a1b2-0000-7000-8000-000000000001",
  name: "Example account A",
};
const ACCOUNT_B = {
  billingAccountId: "0193a1b2-0000-7000-8000-000000000002",
  name: "Example account B",
  callerRole: BillingAccountRole.OWNER,
};
const PATH_A = `/billing/${ACCOUNT_A.billingAccountId}`;
const PATH_B = `/billing/${ACCOUNT_B.billingAccountId}`;
const FAILURE_MESSAGE = "This billing account cannot be removed.";

const actions = [
  {
    method: "DeleteBillingAccount",
    role: BillingAccountRole.OWNER,
    tab: "Settings",
    label: "Delete billing account",
    title: `Delete ${ACCOUNT_A.name}?`,
  },
  {
    method: "RemoveMember",
    role: BillingAccountRole.VIEWER,
    tab: "Members",
    label: "Leave billing account",
    title: "Leave this billing account?",
  },
] as const;

async function delayRemoval(
  browser: Browser,
  appOrigin: string,
  action: (typeof actions)[number],
) {
  await seedFixtureSession(browser, appOrigin);
  let markStarted!: () => void;
  const started = new Promise<void>((resolve) => {
    markStarted = resolve;
  });
  let finish!: (outcome: "success" | "failure") => void;
  const completion = new Promise<"success" | "failure">((resolve) => {
    finish = resolve;
  });
  const account = { ...ACCOUNT_A, callerRole: action.role };
  let removed = false;

  await browser.route(`${API_ORIGIN}/**`, async (route) => {
    const method = new URL(route.request.url).pathname.split("/").pop();
    const headers = {
      "access-control-allow-origin": "*",
      "access-control-allow-methods": "POST",
      "access-control-allow-headers": "*",
    };
    if (route.request.method === "OPTIONS") {
      await route.fulfill({ status: 204, headers });
      return;
    }

    let body: Uint8Array = new Uint8Array();
    if (method === action.method) {
      markStarted();
      if ((await completion) === "failure") {
        await route.fulfill({
          status: 400,
          contentType: "application/json",
          headers,
          body: JSON.stringify({
            code: "failed_precondition",
            message: FAILURE_MESSAGE,
          }),
        });
        return;
      }
      removed = true;
    } else if (method === "ListBillingAccounts") {
      body = toBinary(
        ListBillingAccountsResponseSchema,
        create(ListBillingAccountsResponseSchema, {
          billingAccounts: removed ? [ACCOUNT_B] : [account, ACCOUNT_B],
        }),
      );
    } else if (method === "GetBillingAccount") {
      const isAccountB = route.request.postData?.includes(
        ACCOUNT_B.billingAccountId,
      );
      if (removed && !isAccountB) {
        await route.fulfill({
          status: 404,
          contentType: "application/json",
          headers,
          body: '{"code":"not_found"}',
        });
        return;
      }
      body = toBinary(
        GetBillingAccountResponseSchema,
        create(GetBillingAccountResponseSchema, {
          billingAccount: isAccountB ? ACCOUNT_B : account,
        }),
      );
    }
    await route.fulfill({ contentType: "application/proto", headers, body });
  });

  return { started, finish };
}

describe("billing account removal navigation", () => {
  for (const action of actions) {
    for (const destination of [
      "account B",
      "billing list",
      "another tab on account A",
    ]) {
      test(`${action.label} finishes while viewing ${destination}`, async ({
        app,
        browser,
        screen,
      }) => {
        const removal = await delayRemoval(browser, app.baseUrl, action);
        const listLink = screen.getByRole("link", "Billing", { exact: true });
        const accountALink = screen.getByRole(
          "link",
          new RegExp(ACCOUNT_A.name),
        );
        const accountBLink = screen.getByRole(
          "link",
          new RegExp(ACCOUNT_B.name),
        );
        const staysOnAccount = destination === "another tab on account A";
        try {
          await app.open("/billing");
          await accountALink.click();
          await screen.getByRole("tab", action.tab).click();
          await screen.getByRole("button", action.label).click();
          const dialog = screen.getByRole("alertdialog", action.title);
          await dialog.getByRole("button", action.label).click();
          await removal.started;
          await browser.keyboard.press("Escape");
          await expect(dialog).toBeHidden();

          if (staysOnAccount) {
            await screen.getByRole("tab", "Overview").click();
          } else {
            await listLink.click();
            await expect(accountALink).toBeVisible();
            if (destination === "account B") {
              await accountBLink.click();
              await expect(
                screen.getByRole("heading", ACCOUNT_B.name),
              ).toBeVisible();
            }
          }

          const historyLength = await browser.evaluate(() => history.length);
          const refresh = browser.waitForResponse(
            destination === "account B"
              ? /\/GetBillingAccount$/
              : /\/ListBillingAccounts$/,
          );
          removal.finish("success");
          await (await refresh).body();

          await expect(browser).toHaveURL(
            app.baseUrl + (destination === "account B" ? PATH_B : "/billing"),
          );
          expect(await browser.evaluate(() => history.length)).toBe(
            historyLength + (staysOnAccount ? 1 : 0),
          );
          if (destination === "account B") {
            await expect(
              screen.getByRole("heading", ACCOUNT_B.name),
            ).toBeVisible();
            await app.screenshot(`${action.method}-preserves-account-b`);
            await listLink.click();
          }
          await expect(accountALink).toBeHidden();
          await expect(accountBLink).toBeVisible();
          if (staysOnAccount) {
            await app.screenshot(`${action.method}-returns-to-billing-list`);
          }
        } finally {
          removal.finish("success");
        }
      });
    }

    test(`${action.label} keeps a failed confirmation open`, async ({
      app,
      browser,
      screen,
    }) => {
      const removal = await delayRemoval(browser, app.baseUrl, action);
      try {
        await app.open(PATH_A);
        await screen.getByRole("tab", action.tab).click();
        await screen.getByRole("button", action.label).click();
        const dialog = screen.getByRole("alertdialog", action.title);
        await dialog.getByRole("button", action.label).click();
        await removal.started;

        const historyLength = await browser.evaluate(() => history.length);
        removal.finish("failure");

        await expect(dialog.getByText(FAILURE_MESSAGE)).toBeVisible();
        await expect(browser).toHaveURL(app.baseUrl + PATH_A);
        expect(await browser.evaluate(() => history.length)).toBe(
          historyLength,
        );
        await app.screenshot(`${action.method}-failure`);
      } finally {
        removal.finish("failure");
      }
    });
  }
});
