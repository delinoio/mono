// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

const ACME = "0193b000-0000-7000-8000-000000000001";
const GLOBEX = "0193b000-0000-7000-8000-000000000004";
const SESSION_ENDED = "Your session ended. Sign in again.";
const SIGN_IN = "Sign in with Delino";
const SIGN_IN_FAILURE =
  "Sign-in did not start. Check your connection and try again.";
const PERMISSION_ERROR = "You do not have permission to do this.";

async function releaseDirectAction() {
  window.dispatchEvent(new Event("release-direct-action"));
  // Let the rejected mutation and its React update settle.
  await new Promise<void>((resolve) => {
    requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
  });
  return true;
}

describe("sign-in recovery from direct actions", () => {
  for (const [action, page, list] of [
    ["UpdateMemberRole", "members", "Members"],
    ["RevokeInvitation", "members", "Pending invitations"],
    ["CreateGitHubInstallUrl", "integrations", "GitHub installations"],
  ] as const) {
    for (const failure of ["session", "permission"] as const) {
      test(`${action} handles ${failure} errors on the current page`, async ({
        app,
        browser,
        screen,
      }) => {
        await browser.setViewport({ width: 1280, height: 900 });
        let signIns = 0;
        await browser.route(
          "https://logto.test/oidc/.well-known/openid-configuration",
          async (route) => {
            signIns++;
            await route.abort();
          },
        );
        await app.open(
          `/direct-action-fixture.html?action=${action}&failure=${failure}`,
        );
        const pageList = screen.getByRole("list", list);
        await expect(pageList).toBeVisible();
        if (page === "members") {
          await expect(
            screen.getByText("Nora Lee", { exact: true }),
          ).toBeVisible();
        }

        if (action === "UpdateMemberRole") {
          await screen.getByRole("combobox", "Role of Ada Park").tap();
          await screen.getByRole("option", "Member", { exact: true }).tap();
        } else {
          const button =
            action === "RevokeInvitation" ? "Revoke" : "Connect GitHub";
          await screen.getByRole("button", button, { exact: true }).tap();
        }

        const signIn = screen.getByRole("button", SIGN_IN);
        if (failure === "session") {
          const notice = browser
            .locator(".notice")
            .filter({ hasText: SESSION_ENDED });
          await expect(notice.getByRole("alert")).toHaveText(SESSION_ENDED);
          await expect(notice.getByRole("button")).toHaveCount(1);
          expect(signIns).toBe(0);
          await signIn.tap();
          await expect(screen.getByText(SIGN_IN_FAILURE)).toBeVisible();
          await expect(signIn).toBeEnabled();
          await expect(
            screen.getByText(SESSION_ENDED, { exact: true }),
          ).toBeVisible();
          expect(signIns).toBe(1);
          await app.screenshot(`${action}-sign-in-recovery`);
        } else {
          await expect(
            screen
              .getByRole("region", "Notifications")
              .getByText(PERMISSION_ERROR),
          ).toBeVisible();
          await expect(signIn).toHaveCount(0);
          expect(signIns).toBe(0);
        }
        await expect(pageList).toBeVisible();
        await expect(
          screen.getByRole("combobox", "Organization"),
        ).toContainText("Acme Robotics");
        await expect(browser).toHaveURL(`/orgs/${ACME}/${page}`);
        await expect(browser.locator("body")).toHaveAttribute(
          "data-action-requests",
          "1",
        );
      });
    }
  }

  for (const failure of ["session", "permission"] as const) {
    test(`ignores a delayed GitHub ${failure} error after switching organizations`, async ({
      app,
      browser,
      screen,
    }) => {
      await browser.setViewport({ width: 1280, height: 900 });
      await app.open(
        `/direct-action-fixture.html?action=CreateGitHubInstallUrl&failure=${failure}&delay=true`,
      );
      await screen.getByRole("button", "Connect GitHub").tap();
      const body = browser.locator("body");
      await expect(body).toHaveAttribute("data-action-requests", "1");
      await screen.getByRole("combobox", "Organization").tap();
      await screen.getByRole("option", "Globex").tap();
      await expect(browser).toHaveURL(`/orgs/${GLOBEX}/integrations`);

      await browser.evaluate(releaseDirectAction);

      await expect(body).toHaveAttribute("data-action-rejected", "true");
      await expect(browser).toHaveURL(`/orgs/${GLOBEX}/integrations`);
      await expect(screen.getByText(SESSION_ENDED)).toHaveCount(0);
      await expect(screen.getByRole("button", SIGN_IN)).toHaveCount(0);
      await expect(
        screen.getByRole("region", "Notifications").getByText(PERMISSION_ERROR),
      ).toHaveCount(0);
      await expect(
        screen.getByText("No GitHub organization is connected"),
      ).toBeVisible();
      if (failure === "permission") {
        await app.screenshot("github-stale-permission-error");
      }
    });
  }

  test("shows a delayed GitHub permission error and can retry on the same visit", async ({
    app,
    browser,
    screen,
  }) => {
    await browser.setViewport({ width: 1280, height: 900 });
    const installUrl =
      /^https:\/\/github\.com\/apps\/sandrun\/installations\/new\?state=.+$/;
    let redirects = 0;
    await browser.route(installUrl, async (route) => {
      redirects++;
      await route.fulfill({
        contentType: "text/plain",
        body: "Mock GitHub installation",
      });
    });
    await app.open(
      "/direct-action-fixture.html?action=CreateGitHubInstallUrl&failure=permission&delay=true",
    );
    const connect = screen.getByRole("button", "Connect GitHub");
    await connect.tap();
    await expect(browser.locator("body")).toHaveAttribute(
      "data-action-requests",
      "1",
    );
    await expect(connect).toBeDisabled();
    await browser.evaluate(releaseDirectAction);

    const toast = screen
      .getByRole("region", "Notifications")
      .getByText(PERMISSION_ERROR);
    await expect(toast).toBeVisible();
    await expect(toast).toHaveCount(1);
    await expect(connect).toBeEnabled();
    await expect(browser).toHaveURL(`/orgs/${ACME}/integrations`);
    await app.screenshot("github-active-permission-error");

    await connect.tap();
    await expect(browser).toHaveURL(installUrl);
    expect(redirects).toBe(1);
  });
});
