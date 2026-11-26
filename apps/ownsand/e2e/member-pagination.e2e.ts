// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

describe("member pagination", () => {
  test("lists a later member and offers them for ownership transfer", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(screen.getByRole("link", "Acme Robotics Owner")).toBeVisible();
    await browser.evaluate(() => {
      sessionStorage.setItem("ownsand-e2e-extra-members", "198");
      return null;
    });

    await app.open(`/orgs/${MOCK_IDS.acme}/members`);
    const rows = screen.getByRole("list", "Members").getByRole("listitem");
    await expect(rows).toHaveCount(201);
    await expect(rows.last()).toHaveText(/Fixture member 198/);

    await screen.getByRole("link", "Settings").tap();
    await screen.getByRole("combobox", "New owner").tap();
    await screen.getByRole("option", "Fixture member 198").tap();
    await screen.getByRole("button", "Transfer", { exact: true }).tap();
    await expect(
      screen.getByRole("alertdialog", "Make Fixture member 198 the owner?"),
    ).toBeVisible();
  });
});
