// SPDX-License-Identifier: Apache-2.0
import { BillingAccountRole } from "@delino/accounts-protos/v1/billing_account_pb";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { ACCOUNT_ID, ADMIN, OWNER } from "../src/testing/fake-api.ts";
import type {} from "./billing-confirmations.fixture.tsx";

const FIXTURE = "/billing-confirmations";
const toViewer = { [ADMIN]: BillingAccountRole.VIEWER };
const toAdmin = { [ADMIN]: BillingAccountRole.ADMIN };
const transferToAdmin = {
  [ADMIN]: BillingAccountRole.OWNER,
  [OWNER]: BillingAccountRole.ADMIN,
};
const transferBack = {
  [ADMIN]: BillingAccountRole.ADMIN,
  [OWNER]: BillingAccountRole.OWNER,
};

describe("billing confirmations after a role refresh", () => {
  for (const scenario of [
    {
      tab: "Overview",
      launch: "Cancel subscription",
      title: "Cancel the subscription?",
      rpc: "CancelSubscription",
      path: FIXTURE,
      revoke: toViewer,
      restore: toAdmin,
      revokedRole: "Viewer",
    },
    {
      tab: "Members",
      launch: "Remove",
      title: "Remove Vera Viewer?",
      rpc: "RemoveMember",
      path: FIXTURE,
      revoke: toViewer,
      restore: toAdmin,
      revokedRole: "Viewer",
    },
    {
      tab: "Members",
      launch: "Make owner",
      title: "Make Vera Viewer the owner?",
      rpc: "TransferOwnership",
      path: `${FIXTURE}?owner`,
      revoke: transferToAdmin,
      restore: transferBack,
      revokedRole: "Admin",
    },
  ]) {
    test(`dismisses ${scenario.rpc} after permission is revoked`, async ({
      app,
      browser,
      screen,
    }) => {
      await app.open(scenario.path);
      await screen.getByRole("tab", scenario.tab).click();
      const launch = screen.getByRole("button", scenario.launch);
      await launch.last().click();
      await expect(
        screen.getByRole("alertdialog", scenario.title),
      ).toBeVisible();
      await app.screenshot("confirmation-open");

      await browser.evaluate(
        (roles) => window.billingConfirmations.setRoles(roles).then(() => null),
        scenario.revoke,
      );
      await expect(
        screen.getByText(`Your role: ${scenario.revokedRole}`),
      ).toBeVisible();
      await expect(screen.getByRole("alertdialog")).toBeHidden();
      await expect(launch).toBeHidden();
      await app.screenshot("permission-revoked");

      await browser.evaluate(
        (roles) => window.billingConfirmations.setRoles(roles).then(() => null),
        scenario.restore,
      );
      await expect(launch.last()).toBeVisible();
      await expect(screen.getByRole("alertdialog")).toBeHidden();
      await app.screenshot("permission-restored");
      await launch.last().click();
      await expect(
        screen.getByRole("alertdialog", scenario.title),
      ).toBeVisible();
      await browser.keyboard.press("Escape");
      await expect(screen.getByRole("alertdialog")).toBeHidden();
      expect(
        await browser.evaluate(
          (rpc) => window.billingConfirmations.actionRequests(rpc),
          scenario.rpc,
        ),
      ).toEqual([]);
    });
  }

  test("keeps cancellation open when the last loaded role still permits it", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open(`${FIXTURE}?owner`);
    await screen.getByRole("button", "Cancel subscription").click();
    const dialog = screen.getByRole("alertdialog", "Cancel the subscription?");
    await expect(dialog).toBeVisible();

    await browser.evaluate(() =>
      window.billingConfirmations.failAccountRefresh().then(() => null),
    );
    await expect(
      screen.getByText(/Delino Accounts is not available/),
    ).toBeVisible();
    await expect(dialog).toBeVisible();
    await app.screenshot("transient-refresh-failure");

    await browser.evaluate(
      (roles) => window.billingConfirmations.setRoles(roles).then(() => null),
      transferToAdmin,
    );
    await expect(screen.getByText("Your role: Admin")).toBeVisible();
    await expect(dialog).toBeVisible();
    await app.screenshot("permission-retained");
    await dialog.getByRole("button", "Cancel subscription").click();
    await expect(screen.getByText(/Ends on/)).toBeVisible();
    expect(
      await browser.evaluate(() =>
        window.billingConfirmations.actionRequests("CancelSubscription"),
      ),
    ).toEqual([{ billingAccountId: ACCOUNT_ID, userId: null }]);
  });
});
