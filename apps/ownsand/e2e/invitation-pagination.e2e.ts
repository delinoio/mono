// SPDX-License-Identifier: Apache-2.0
import { Code } from "@connectrpc/connect";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

const ACME = "0193a000-0000-7000-8000-000000000001";
const NORTHWIND = "0193a000-0000-7000-8000-000000000002";
const INCOMPLETE = "Some pending invitations are not shown yet.";

const pagedInvitations = test.extend<{
  invitations: {
    open(settings: Record<string, string>): Promise<void>;
    value(name: string): Promise<string>;
    tokens(): Promise<string[]>;
  };
}>({
  invitations: async ({ app, browser, screen }, use) => {
    await use({
      async open(settings) {
        await app.open("/");
        await screen.getByRole("button", "Sign in with Delino").tap();
        await expect(
          screen.getByRole("link", "Acme Robotics Owner"),
        ).toBeVisible();
        await browser.evaluate((options) => {
          sessionStorage.setItem("ownsand-e2e-paged-list", "ListInvitations");
          for (const [key, value] of Object.entries(options)) {
            sessionStorage.setItem(`ownsand-e2e-${key}`, value);
          }
          return null;
        }, settings);
        await app.open(`/orgs/${ACME}/members`);
      },
      value(name) {
        return browser.evaluate(
          (key) => sessionStorage.getItem(`ownsand-e2e-${key}`) ?? "",
          name,
        );
      },
      async tokens() {
        return JSON.parse(await this.value("page-tokens")) as string[];
      },
    });
  },
});

describe("pending invitation pagination", () => {
  pagedInvitations(
    "can revoke an invitation created on page two after dismissing its form",
    async ({ app, browser, screen, invitations }) => {
      await invitations.open({
        "extra-invitations": "199",
        "hold-invitation": "true",
      });
      const region = screen.getByRole("region", "Invitations");
      const rows = region
        .getByRole("list", "Pending invitations")
        .getByRole("listitem");
      const loading = region.getByText("Loading more pending invitations");

      await expect(rows).toHaveCount(200);
      await expect(loading).toBeHidden();
      const initialTokens = await invitations.tokens();
      const continuations = initialTokens.filter(Boolean);
      expect(initialTokens[0]).toEqual("");
      expect(continuations).toHaveLength(1);

      await region.getByRole("button", "Invite someone").tap();
      const form = screen.getByRole("dialog", "Invite someone");
      const create = form.getByRole("button", "Create link");
      await create.tap();
      await expect(create).toBeDisabled();
      expect(await invitations.value("invitation-held")).toEqual("true");
      await browser.keyboard.press("Escape");
      await expect(form).toBeHidden();
      await browser.evaluate(() => {
        window.dispatchEvent(new Event("ownsand-e2e-release-invitation"));
        return null;
      });

      await expect(rows).toHaveCount(201);
      await expect(
        screen.getByRole("dialog", "Your invitation link"),
      ).toBeHidden();
      const laterId = await invitations.value("later-invitation");
      expect(laterId).toMatch(/^[0-9a-f-]{36}$/);
      await rows
        .last()
        .getByRole("button", /^Revoke/)
        .tap();
      const confirmation = screen.getByRole("alertdialog");
      await expect(confirmation).toBeVisible();
      await app.screenshot("revoke-later-invitation");
      await confirmation.getByRole("button", "Revoke invitation").tap();

      await expect(rows).toHaveCount(200);
      await expect(loading).toBeHidden();
      expect(JSON.parse(await invitations.value("revoked-invitation"))).toEqual(
        { organizationId: ACME, invitationId: laterId },
      );
      expect((await invitations.tokens()).slice(initialTokens.length)).toEqual([
        "",
        ...continuations,
        "",
        ...continuations,
      ]);
    },
  );

  pagedInvitations(
    "retries a failed continuation while keeping the first page available",
    async ({ app, browser, screen, invitations }) => {
      await invitations.open({ "page-error": String(Code.Unavailable) });
      const region = screen.getByRole("region", "Invitations");
      const rows = region
        .getByRole("list", "Pending invitations")
        .getByRole("listitem");
      const incomplete = region.getByText(INCOMPLETE);
      await expect(incomplete).toBeVisible();
      await expect(rows).toHaveCount(200);
      const failedTokens = await invitations.tokens();
      const failedContinuations = failedTokens.filter(Boolean);
      const failedToken = failedContinuations[0];
      expect(failedToken).toMatch(/.+/);
      for (const token of failedContinuations) {
        expect(token).toEqual(failedToken);
      }
      await browser.evaluate((message) => {
        Array.from(document.querySelectorAll("p"))
          .find((element) => element.textContent === message)
          ?.scrollIntoView({ block: "center" });
        return null;
      }, INCOMPLETE);
      await app.screenshot("pending-invitations-incomplete");

      await browser.evaluate(() => {
        sessionStorage.removeItem("ownsand-e2e-page-error");
        return null;
      });
      await region.getByRole("button", "Try again").tap();
      await expect(rows).toHaveCount(201);
      await expect(incomplete).toBeHidden();
      const later = rows.getByRole(
        "button",
        "Revoke Member invitation osi_0199",
      );
      await expect(later).toHaveCount(1);
      await app.screenshot("pending-invitations-recovered");
      expect(await invitations.tokens()).toEqual([
        ...failedTokens,
        failedToken,
      ]);

      await screen.getByRole("combobox", "Organization").tap();
      await screen.getByRole("option", /Northwind Labs/).tap();
      await expect(browser).toHaveURL(`/orgs/${NORTHWIND}/members`);
      await expect(region).toBeHidden();
    },
  );
});
