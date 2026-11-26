// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

const roster = [
  ["Mina Kim", "Owner"],
  ["Jun Park", "Admin"],
  ["user-member-ada", "Member"],
] as const;

describe("member-row actions while caller identity resolves", () => {
  for (const caller of ["admin", "member"] as const) {
    test(`updates the Members page after the ${caller} ID resolves`, async ({
      app,
      browser,
      screen,
    }) => {
      const query = caller === "member" ? "?member" : "";
      await app.open(`/member-row-identity.html${query}`);
      const members = screen.getByRole("list", "Members");
      const rows = members.getByRole("listitem");
      const roleSelectors = members.getByRole("combobox");
      const removeButtons = members.getByRole("button", /^Remove /);

      await expect(rows).toHaveCount(3);
      await expect(roleSelectors).toHaveCount(0);
      await expect(removeButtons).toHaveCount(0);
      for (const [name, role] of roster) {
        const row = rows.filter({ hasText: name });
        await expect(row.getByText(role)).toBeVisible();
      }
      await expect(members.getByText("(you)")).toHaveCount(0);
      await app.screenshot(`members-${caller}-unresolved`);

      await browser.evaluate(() => {
        window.dispatchEvent(new Event("ownsand-e2e-resolve-member-caller"));
        return null;
      });

      const callerName = caller === "admin" ? "Jun Park" : "user-member-ada";
      await expect(
        rows.filter({ hasText: callerName }).getByText("(you)"),
      ).toBeVisible();
      await expect(rows).toHaveCount(3);
      if (caller === "admin") {
        await expect(
          rows.filter({ hasText: "Mina Kim" }).getByText("Owner"),
        ).toBeVisible();
        await expect(
          rows.filter({ hasText: "Jun Park" }).getByText("Admin"),
        ).toBeVisible();
        await expect(
          members.getByRole("combobox", "Role of user-member-ada"),
        ).toBeVisible();
        await expect(
          members.getByRole("button", "Remove user-member-ada"),
        ).toBeVisible();
        await expect(roleSelectors).toHaveCount(1);
        await expect(removeButtons).toHaveCount(1);
      } else {
        for (const [name, role] of roster) {
          const row = rows.filter({ hasText: name });
          await expect(row.getByText(role)).toBeVisible();
        }
        await expect(roleSelectors).toHaveCount(0);
        await expect(removeButtons).toHaveCount(0);
      }

      expect(
        await browser.evaluate(() =>
          sessionStorage.getItem("ownsand-e2e-member-mutations"),
        ),
      ).toBe("[]");
      await app.screenshot(`members-${caller}-resolved`);
    });
  }
});
