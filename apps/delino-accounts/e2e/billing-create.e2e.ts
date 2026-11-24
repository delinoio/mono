// SPDX-License-Identifier: Apache-2.0
import { create, toBinary } from "@bufbuild/protobuf";
import {
  BillingAccountRole,
  CreateBillingAccountResponseSchema,
  GetBillingAccountResponseSchema,
  ListBillingAccountsResponseSchema,
} from "@delino/accounts-protos/v1/billing_account_pb";
import { type Browser, describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import {
  BILLING_API_ORIGIN,
  installBillingSession,
} from "./billing-session.ts";

const existingAccount = {
  billingAccountId: "0193a1b2-0000-7000-8000-000000000001",
  name: "Acme Robotics",
  callerRole: BillingAccountRole.OWNER,
};
const newAccount = {
  billingAccountId: "0193a1b2-0000-7000-8000-000000000002",
  name: "Northwind",
  callerRole: BillingAccountRole.OWNER,
};

async function deferBillingCreate(browser: Browser, holdListRefresh: boolean) {
  let markCreateStarted!: () => void;
  let markListRefreshStarted!: () => void;
  let releaseCreate!: () => void;
  let releaseList!: () => void;
  const createStarted = new Promise<void>((resolve) => {
    markCreateStarted = resolve;
  });
  const listRefreshStarted = new Promise<void>((resolve) => {
    markListRefreshStarted = resolve;
  });
  const createGate = new Promise<void>((resolve) => {
    releaseCreate = resolve;
  });
  const listGate = new Promise<void>((resolve) => {
    releaseList = resolve;
  });
  let creates = 0;
  let reads = 0;
  let created = false;
  await browser.route(BILLING_API_ORIGIN + "/**", async (route) => {
    const headers = {
      "access-control-allow-origin": "*",
      "access-control-allow-methods": "POST",
      "access-control-allow-headers": "*",
    };
    if (route.request.method === "OPTIONS") {
      await route.fulfill({ status: 204, headers });
      return;
    }

    const method = new URL(route.request.url).pathname.split("/").pop();
    let body: Uint8Array<ArrayBufferLike> = new Uint8Array();
    switch (method) {
      case "CreateBillingAccount":
        creates += 1;
        markCreateStarted();
        await createGate;
        created = true;
        body = toBinary(
          CreateBillingAccountResponseSchema,
          create(CreateBillingAccountResponseSchema, {
            billingAccount: newAccount,
          }),
        );
        break;
      case "ListBillingAccounts":
        reads += 1;
        if (holdListRefresh && reads > 1) {
          markListRefreshStarted();
          await listGate;
        }
        body = toBinary(
          ListBillingAccountsResponseSchema,
          create(ListBillingAccountsResponseSchema, {
            billingAccounts: created
              ? [existingAccount, newAccount]
              : [existingAccount],
          }),
        );
        break;
      case "GetBillingAccount":
        body = toBinary(
          GetBillingAccountResponseSchema,
          create(GetBillingAccountResponseSchema, {
            billingAccount: newAccount,
          }),
        );
        break;
    }
    await route.fulfill({ contentType: "application/proto", headers, body });
  });
  return {
    createStarted,
    listRefreshStarted,
    releaseCreate,
    releaseList,
    counts() {
      return { creates, reads };
    },
  };
}

describe("billing account creation", () => {
  for (const leave of [true, false]) {
    test(
      leave
        ? "preserves browser Back and refreshes billing after a late create"
        : "opens the created account while the list refresh is held",
      async ({ app, browser, screen }) => {
        await installBillingSession(browser, app.baseUrl);
        const billing = await deferBillingCreate(browser, !leave);
        try {
          await app.open("/");
          await screen.getByRole("link", "Manage billing").click();
          await expect(screen.getByRole("link", /Acme Robotics/)).toBeVisible();
          await screen.getByRole("button", "Create billing account").click();
          await screen.getByRole("textbox", "Name").fill(newAccount.name);
          await screen.getByRole("button", "Create", { exact: true }).click();
          await billing.createStarted;

          if (leave) {
            await browser.evaluate(() => {
              window.history.back();
              return null;
            });
            await expect(
              screen.getByRole("heading", "Delino Accounts", { level: 1 }),
            ).toBeVisible();
            await expect(screen.getByRole("dialog")).toBeHidden();
          }
          const historyLength = await browser.evaluate(
            () => window.history.length,
          );
          const response = browser.waitForResponse(/\/CreateBillingAccount$/);
          billing.releaseCreate();
          await (await response).body();

          if (leave) {
            // Let the app process the completed response before checking navigation.
            await browser.evaluate(
              () =>
                new Promise<number>((resolve) => {
                  requestAnimationFrame(() => requestAnimationFrame(resolve));
                }),
            );
            await expect(browser).toHaveURL(app.baseUrl + "/");
            expect(await browser.evaluate(() => window.history.length)).toBe(
              historyLength,
            );
            await app.screenshot("home-after-late-create");

            await browser.evaluate(() => {
              window.history.forward();
              return null;
            });
            await expect(browser).toHaveURL(app.baseUrl + "/billing");
            await expect(screen.getByRole("link", /Northwind/)).toBeVisible();
            await app.screenshot("billing-list-after-late-create");
          } else {
            await billing.listRefreshStarted;
            await expect(
              screen.getByRole("heading", newAccount.name, { level: 1 }),
            ).toBeVisible();
            await expect(screen.getByRole("dialog")).toBeHidden();
            await app.screenshot("new-account-during-list-refresh");
          }
          expect(billing.counts()).toEqual({ creates: 1, reads: 2 });
        } finally {
          billing.releaseCreate();
          billing.releaseList();
        }
      },
    );
  }
});
