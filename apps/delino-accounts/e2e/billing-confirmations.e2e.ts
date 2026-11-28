// SPDX-License-Identifier: Apache-2.0
import { BillingAccountRole } from "@delino/accounts-protos/v1/billing_account_pb";
import { SubscriptionStatus } from "@delino/accounts-protos/v1/credit_pb";
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
    {
      tab: "Members",
      launch: "Leave billing account",
      title: "Leave this billing account?",
      rpc: "RemoveMember",
      action: "Leave as an Admin",
      path: FIXTURE,
      revoke: transferToAdmin,
      restore: transferBack,
      revokedRole: "Owner",
    },
    {
      tab: "Members",
      launch: "Leave billing account",
      title: "Leave this billing account?",
      rpc: "RemoveMember",
      action: "Leave as a Viewer",
      path: FIXTURE,
      startAsViewer: true,
      revoke: transferToAdmin,
      restore: { ...transferBack, ...toViewer },
      revokedRole: "Owner",
    },
  ]) {
    test(`dismisses ${scenario.action ?? scenario.rpc} after permission is revoked`, async ({
      app,
      browser,
      screen,
    }) => {
      await app.open(scenario.path);
      if (scenario.startAsViewer) {
        await expect(screen.getByText("Your role: Admin")).toBeVisible();
        await browser.evaluate(
          (roles) =>
            window.billingConfirmations.setRoles(roles).then(() => null),
          toViewer,
        );
        await expect(screen.getByText("Your role: Viewer")).toBeVisible();
      }
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

  test("keeps Leave open when an Admin becomes a Viewer", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open(FIXTURE);
    await expect(screen.getByText("Your role: Admin")).toBeVisible();
    await screen.getByRole("tab", "Members").click();
    await screen.getByRole("button", "Leave billing account").click();
    const dialog = screen.getByRole(
      "alertdialog",
      "Leave this billing account?",
    );
    await expect(dialog).toBeVisible();

    await browser.evaluate(
      (roles) => window.billingConfirmations.setRoles(roles).then(() => null),
      toViewer,
    );
    await expect(screen.getByText("Your role: Viewer")).toBeVisible();
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", "Leave billing account").click();

    await expect(browser).toHaveURL(app.baseUrl + "/billing");
    await expect(screen.getByText("No billing accounts yet")).toBeVisible();
    expect(
      await browser.evaluate(() =>
        window.billingConfirmations.actionRequests("RemoveMember"),
      ),
    ).toEqual([{ billingAccountId: ACCOUNT_ID, userId: ADMIN }]);
  });

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

describe("cancellation confirmation after a subscription refresh", () => {
  for (const scenario of [
    {
      name: "scheduled cancellation",
      subscription: {
        status: SubscriptionStatus.ACTIVE,
        cancelAtPeriodEnd: true,
      },
      refreshedState: "Resume subscription",
    },
    {
      name: "completed cancellation",
      subscription: {
        status: SubscriptionStatus.CANCELED,
        cancelAtPeriodEnd: false,
      },
      refreshedState: "Canceled",
    },
  ]) {
    test(`dismisses and clears the dialog after ${scenario.name}`, async ({
      app,
      browser,
      screen,
    }) => {
      await app.open(FIXTURE);
      await expect(screen.getByText("Your role: Admin")).toBeVisible();
      const launch = screen.getByRole("button", "Cancel subscription");
      const dialog = screen.getByRole(
        "alertdialog",
        "Cancel the subscription?",
      );
      await launch.click();
      await expect(dialog).toBeVisible();

      await browser.evaluate(
        (next) =>
          window.billingConfirmations.setSubscription(next).then(() => null),
        scenario.subscription,
      );
      await expect(
        screen.getByText(scenario.refreshedState, { exact: true }),
      ).toBeVisible();
      await expect(dialog).toBeHidden();
      await expect(launch).toBeHidden();
      await app.screenshot("cancellation-unavailable");

      await browser.evaluate(
        (next) =>
          window.billingConfirmations.setSubscription(next).then(() => null),
        { status: SubscriptionStatus.ACTIVE, cancelAtPeriodEnd: false },
      );
      await expect(launch).toBeVisible();
      await expect(dialog).toBeHidden();
      await app.screenshot("cancellation-available-again");
      await launch.click();
      await expect(dialog).toBeVisible();
      expect(
        await browser.evaluate(() =>
          window.billingConfirmations.actionRequests("CancelSubscription"),
        ),
      ).toEqual([]);
    });
  }

  test("keeps the dialog open through a retryable subscription refresh error", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open(FIXTURE);
    await screen.getByRole("button", "Cancel subscription").click();
    const dialog = screen.getByRole("alertdialog", "Cancel the subscription?");
    await expect(dialog).toBeVisible();

    await browser.evaluate(() =>
      window.billingConfirmations.failSubscriptionRefresh().then(() => null),
    );
    const retryNotice = screen.getByText(/Delino Accounts is not available/);
    await expect(retryNotice).toBeVisible();
    await expect(dialog).toBeVisible();
    await app.screenshot("subscription-refresh-failed");

    await browser.evaluate(
      (next) =>
        window.billingConfirmations.setSubscription(next).then(() => null),
      { status: SubscriptionStatus.PAST_DUE, cancelAtPeriodEnd: false },
    );
    await expect(screen.getByText("Payment overdue")).toBeVisible();
    await expect(retryNotice).toBeHidden();
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", "Cancel subscription").click();
    await expect(dialog).toBeHidden();
    await expect(screen.getByText(/Ends on/)).toBeVisible();
    expect(
      await browser.evaluate(() =>
        window.billingConfirmations.actionRequests("CancelSubscription"),
      ),
    ).toEqual([{ billingAccountId: ACCOUNT_ID, userId: null }]);
  });
});
