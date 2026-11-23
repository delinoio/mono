// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

// IDs from src/mock/mock-api.ts: Acme is linked to a billing account, and its
// mock sandbox 0193a000-0000-7000-8000-0000000000a1 is running, so "Take
// snapshot" is offered.
const ACME = "0193a000-0000-7000-8000-000000000001";

describe("billing toast action", () => {
  for (const colorScheme of ["light", "dark"] as const) {
    test(`renders white text on the accent background in ${colorScheme} mode`, async ({
      app,
      screen,
      browser,
    }) => {
      await browser.emulateMedia({ colorScheme });
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();

      // Unlink billing so that the organization is blocked and the next
      // sandbox action fails with a billing reason.
      await app.open(`/orgs/${ACME}/settings`);
      await screen.getByRole("button", "Unlink billing account").tap();
      const confirm = screen.getByRole("alertdialog");
      await confirm.getByRole("button", "Unlink billing account").tap();
      await expect(confirm).toBeHidden();

      // Navigate inside the page, as in the routing test of console.e2e.ts:
      // `app.open` reloads the page and resets the in-browser fake control
      // plane, which would link the billing account again.
      await browser.evaluate(() => {
        window.history.pushState(
          null,
          "",
          "/orgs/0193a000-0000-7000-8000-000000000001/sandboxes/0193a000-0000-7000-8000-0000000000a1",
        );
        window.dispatchEvent(new PopStateEvent("popstate"));
        return true;
      });
      await screen.getByRole("button", "Take snapshot").tap();

      const action = screen.getByRole("link", "Open billing");
      await expect(action).toBeVisible();
      await expect(action).toHaveAttribute(
        "href",
        "https://accounts.delino.io",
      );

      // The unlayered global anchor rule used to give this anchor the link
      // color, which equals the button background in light mode.
      const colors = await browser.evaluate((): string => {
        const link = Array.from(document.querySelectorAll("a")).find(
          (a) => (a.textContent ?? "").trim() === "Open billing",
        );
        if (link === undefined) {
          return "missing";
        }
        const style = getComputedStyle(link);
        return `${style.color} on ${style.backgroundColor}`;
      });
      expect(colors).toBe("rgb(255, 255, 255) on rgb(27, 100, 218)");
    });
  }
});
