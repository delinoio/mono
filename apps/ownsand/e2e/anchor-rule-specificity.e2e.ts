// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

// The global anchor rules in src/index.css exclude `.dui-button`. They must
// keep the specificity of the plain `a` rule, (0,0,1) and (0,1,1) with
// `:hover`. A rule such as `a:not(.dui-button)` has (0,1,1) and (0,2,1), so
// it beats one-class rules like `.button-link` and `.list-link` and turns
// their text into the link color. These tests read the computed color and
// text decoration of anchors that style themselves with one class, and of a
// plain anchor, before and while the pointer hovers them.
const ACME = "0193a000-0000-7000-8000-000000000001";

/**
 * Builds an expression that finds the anchor whose text starts with `text`
 * and that matches `selector`, and describes it: whether its computed color
 * equals the color token, its text decoration line, and whether it is
 * hovered. Evaluated in the page, so it cannot capture local variables.
 */
function describeAnchor(selector: string, text: string, token: string) {
  return `(() => {
    const link = Array.from(document.querySelectorAll(${JSON.stringify(selector)})).find(
      (a) => (a.textContent ?? "").trim().startsWith(${JSON.stringify(text)}),
    );
    if (!link) return "missing";
    const probe = document.createElement("span");
    probe.style.color = ${JSON.stringify(`var(${token})`)};
    document.body.append(probe);
    const expected = getComputedStyle(probe).color;
    probe.remove();
    const style = getComputedStyle(link);
    return [
      "color " + (style.color === expected ? "matches" : "differs from") + " ${token}",
      "decoration " + style.textDecorationLine,
      "hovered " + link.matches(":hover"),
    ].join(", ");
  })()`;
}

describe("anchor rule specificity", () => {
  for (const colorScheme of ["light", "dark"] as const) {
    describe(`${colorScheme} mode`, () => {
      test("keeps the colors of one-class anchors and plain links", async ({
        app,
        screen,
        browser,
      }) => {
        await browser.emulateMedia({ colorScheme });
        await app.open("/");
        await screen.getByRole("button", "Sign in with Delino").tap();
        await expect(
          screen.getByRole("link", "Acme Robotics Owner"),
        ).toBeVisible();

        // `.list-link` sets the text color and keeps the hover underline.
        const listLink = () =>
          browser.evaluate(
            describeAnchor(".list-link", "Acme Robotics", "--dui-color-text"),
          );
        expect(await listLink()).toBe(
          "color matches --dui-color-text, decoration none, hovered false",
        );
        await screen.getByRole("link", "Acme Robotics Owner").hover();
        expect(await listLink()).toBe(
          "color matches --dui-color-text, decoration underline, hovered true",
        );

        // A plain link keeps the link color and the hover underline.
        const plainLink = () =>
          browser.evaluate(
            describeAnchor(
              "a",
              "Account settings",
              "--dui-color-accent-graphic",
            ),
          );
        expect(await plainLink()).toBe(
          "color matches --dui-color-accent-graphic, decoration none, hovered false",
        );
        await screen.getByRole("link", "Account settings").hover();
        expect(await plainLink()).toBe(
          "color matches --dui-color-accent-graphic, decoration underline, hovered true",
        );

        // The owner sees "Add a device", a `.button-link` on the accent
        // background. Its text color is the on-accent color and the hover
        // state has no underline.
        await app.open(`/orgs/${ACME}/devices`);
        const buttonLink = () =>
          browser.evaluate(
            describeAnchor(
              ".button-link",
              "Add a device",
              "--dui-color-on-accent",
            ),
          );
        await expect(screen.getByRole("link", "Add a device")).toBeVisible();
        expect(await buttonLink()).toBe(
          "color matches --dui-color-on-accent, decoration none, hovered false",
        );
        await screen.getByRole("link", "Add a device").hover();
        expect(await buttonLink()).toBe(
          "color matches --dui-color-on-accent, decoration none, hovered true",
        );

        // The side navigation links keep the text color and have no hover
        // underline.
        const navLink = () =>
          browser.evaluate(
            describeAnchor(".sidenav a", "Sandboxes", "--dui-color-text"),
          );
        expect(await navLink()).toBe(
          "color matches --dui-color-text, decoration none, hovered false",
        );
        await screen.getByRole("link", "Sandboxes").hover();
        expect(await navLink()).toBe(
          "color matches --dui-color-text, decoration none, hovered true",
        );
      });
    });
  }
});
