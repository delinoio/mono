// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { MOCK_IDS } from "../src/mock/mock-api.ts";
import { lastKnownStateCases } from "./sandbox-last-known-state-fixture.ts";

describe("sandbox last-known state badges", () => {
  for (const { name, label } of lastKnownStateCases) {
    test(`shows the sandbox with a ${name} last-known state`, async ({
      app,
      browser,
      screen,
    }) => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await expect(
        screen.getByRole("link", "Acme Robotics Owner"),
      ).toBeVisible();
      await browser.evaluate((scenario) => {
        sessionStorage.setItem(
          "ownsand-e2e-sandbox-last-known-state",
          scenario,
        );
        return true;
      }, name);
      await app.open(`/orgs/${MOCK_IDS.acme}/sandboxes`);

      const row = screen.getByRole("row", /0193a000…00a4/);
      const listBadge = row.getByText(label, { exact: true });
      await expect(listBadge).toBeVisible();
      await expect(listBadge).toHaveAttribute("data-variant", "danger");
      await expect(
        screen
          .getByRole("row", /0193a000…00a1/)
          .getByText("Running", { exact: true }),
      ).toBeVisible();
      await app.screenshot(`sandbox-${name}-list`);

      await row.getByRole("link", "0193a000…00a4").tap();
      await expect(
        screen.getByRole("heading", "Sandbox", { exact: true }),
      ).toBeVisible();
      const detailBadge = screen.getByText(label, { exact: true });
      await expect(detailBadge).toBeVisible();
      await expect(detailBadge).toHaveAttribute("data-variant", "danger");
      await expect(screen.getByRole("link", "studio-mac")).toBeVisible();
      await app.screenshot(`sandbox-${name}-detail`);
    });
  }
});
