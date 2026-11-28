// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

describe("leaving an organization after the caller ID resolves", () => {
  for (const [role, userId] of [
    ["admin", "user-admin-jun"],
    ["member", "user-member-ada"],
  ] as const) {
    for (const missing of ["undefined", "empty"] as const) {
      test(`waits for the ${role}'s ${missing} caller ID`, async ({
        app,
        browser,
        screen,
      }) => {
        await app.open(
          `/member-row-identity.html?${role}${missing === "empty" ? "&empty" : ""}`,
        );
        const roster = screen.getByRole("list", "Members");
        const leave = screen.getByRole("button", "Leave organization");
        const requests = () =>
          browser.evaluate(() =>
            JSON.parse(
              sessionStorage.getItem("ownsand-e2e-leave-requests") ?? "null",
            ),
          );
        await expect(roster.getByRole("listitem")).toHaveCount(3);
        await expect(leave).toHaveCount(0);
        expect(await requests()).toEqual([]);

        await browser.evaluate(() => {
          window.dispatchEvent(new Event("ownsand-e2e-resolve-member-caller"));
          return null;
        });
        await leave.tap();
        await screen
          .getByRole("alertdialog")
          .getByRole("button", "Leave organization")
          .tap();

        const refreshPath = await browser.evaluate(async () => {
          const key = "ownsand-e2e-leave-refresh-path";
          const deadline = performance.now() + 5_000;
          while (
            sessionStorage.getItem(key) === null &&
            performance.now() < deadline
          ) {
            await new Promise(requestAnimationFrame);
          }
          return sessionStorage.getItem(key);
        });
        expect(refreshPath).toBe("/");
        await expect(browser).toHaveURL("/");
        expect(await requests()).toEqual([
          { organizationId: MOCK_IDS.acme, userId },
        ]);
        await browser.evaluate(() => {
          window.dispatchEvent(new Event("ownsand-e2e-release-leave-refresh"));
          return null;
        });
        await expect(
          screen.getByText("You are not in an organization yet"),
        ).toBeVisible();
      });
    }
  }

  test("keeps Leave unavailable to an owner with a resolved caller ID", async ({
    app,
    screen,
  }) => {
    await app.open("/member-row-identity.html?owner");
    const roster = screen.getByRole("list", "Members");
    await expect(roster.getByText("(you)")).toBeVisible();
    await expect(screen.getByRole("button", "Leave organization")).toHaveCount(
      0,
    );
  });
});
