// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

const ORGANIZATION_ID = "0193a000-0000-7000-8000-000000000001";
const FIRST_ID = "0194993a-5800-7000-8000-000000000002";
const SECOND_ID = "0194993a-5800-7000-8000-000000000003";

describe("revocation target identity", () => {
  for (const flow of [
    {
      path: "enrollment-tokens",
      mode: "same",
      list: "Enrollment tokens",
      loading: "Loading more enrollment tokens",
      firstAction: "Revoke enrollment token ose_12ab",
      secondAction: "Revoke enrollment token ose_12ab",
      title: "Revoke token ose_12ab…",
      confirm: "Revoke token",
      idField: "enrollmentTokenId",
    },
    {
      path: "members",
      mode: "cross-role",
      list: "Pending invitations",
      loading: "Loading more pending invitations",
      firstAction: "Revoke Member invitation osi_12ab",
      secondAction: "Revoke Admin invitation osi_12ab",
      title: "Revoke invitation osi_12ab…",
      confirm: "Revoke invitation",
      idField: "invitationId",
    },
  ]) {
    test(`identifies the selected record across a later page of ${flow.list.toLowerCase()}`, async ({
      app,
      browser,
      screen,
    }) => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await expect(
        screen.getByRole("link", "Acme Robotics Owner"),
      ).toBeVisible();
      await browser.evaluate((mode) => {
        sessionStorage.setItem("ownsand-e2e-revoke-collision", mode);
        return null;
      }, flow.mode);
      await app.open(`/orgs/${ORGANIZATION_ID}/${flow.path}`);

      const list = screen.getByRole("list", flow.list);
      const loading = screen.getByText(flow.loading);
      const firstUnique = list.getByRole("button", flow.firstAction);
      await expect(loading).toBeVisible();
      const continuationStatus = await browser.evaluate(async () => {
        const key = "ownsand-e2e-revoke-continuation";
        const deadline = performance.now() + 5_000;
        while (
          sessionStorage.getItem(key) !== "held" &&
          performance.now() < deadline
        ) {
          await new Promise(requestAnimationFrame);
        }
        return sessionStorage.getItem(key);
      });
      expect(continuationStatus).toEqual("held");
      await firstUnique.tap();
      await expect(
        screen.getByRole("alertdialog", `${flow.title}?`),
      ).toBeVisible();
      await app.screenshot(`${flow.path}-before-continuation`);

      await browser.evaluate(() => {
        const key = "ownsand-e2e-revoke-continuation";
        sessionStorage.setItem(key, "released");
        window.dispatchEvent(new Event(`${key}-release`));
        return null;
      });
      const firstDialog = screen.getByRole(
        "alertdialog",
        `${flow.title} (${FIRST_ID})?`,
      );
      await expect(firstDialog).toBeVisible();
      await app.screenshot(`${flow.path}-after-continuation`);
      await firstDialog.getByRole("button", "Cancel").tap();
      // Wait for the closing transition to remove the modal before acting on another row.
      await expect(browser.locator(".dui-dialog-content")).toHaveCount(0);

      const readRequests = () =>
        browser.evaluate(() =>
          JSON.parse(
            sessionStorage.getItem("ownsand-e2e-revoke-requests") ?? "[]",
          ),
        );
      expect(await readRequests()).toEqual([]);
      const rows = list.getByRole("listitem");
      await expect(rows.first().getByText(FIRST_ID)).toBeVisible();
      await expect(rows.last().getByText(SECOND_ID)).toBeVisible();
      await expect(
        rows.first().getByRole("button", `${flow.firstAction} (${FIRST_ID})`),
      ).toBeVisible();
      await app.screenshot(`${flow.path}-colliding-rows`);

      await rows
        .last()
        .getByRole("button", `${flow.secondAction} (${SECOND_ID})`)
        .tap();
      const secondDialog = screen.getByRole(
        "alertdialog",
        `${flow.title} (${SECOND_ID})?`,
      );
      await expect(secondDialog).toBeVisible();
      await app.screenshot(`${flow.path}-confirm-second`);
      await secondDialog.getByRole("button", flow.confirm).tap();

      await expect(secondDialog).toBeHidden();
      await expect(firstUnique).toBeVisible();
      expect(await readRequests()).toEqual([
        { organizationId: ORGANIZATION_ID, [flow.idField]: SECOND_ID },
      ]);
    });
  }
});
