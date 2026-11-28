// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

describe("sandbox template source", () => {
  test("uses the short ID when a template refresh fails and restores the name on recovery", async ({
    app,
    browser,
    screen,
  }) => {
    const setLookup = (result: "success" | "fail") =>
      browser.evaluate((value) => {
        sessionStorage.setItem("ownsand-e2e-sandbox-template-lookup", value);
        return true;
      }, result);

    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(screen.getByRole("link", "Acme Robotics Owner")).toBeVisible();
    await setLookup("success");
    await app.open(
      `/orgs/${MOCK_IDS.acme}/sandboxes/${MOCK_IDS.runningSandbox}`,
    );

    const name = screen.getByText("node-24-toolchain", { exact: true });
    await expect(name).toHaveAttribute("title", MOCK_IDS.template);
    await app.screenshot("sandbox-template-source-before");

    const notifications = screen.getByRole("region", "Notifications");
    await setLookup("fail");
    await screen.getByRole("button", "Pause").tap();
    await expect(
      notifications.getByText("The sandbox is paused."),
    ).toBeVisible();
    const shortId = screen.getByText("0193a000…00c1", { exact: true });
    await expect(shortId).toHaveAttribute("title", MOCK_IDS.template);
    await expect(name).toBeHidden();
    await expect(
      screen.getByRole("heading", "Sandbox", { exact: true }),
    ).toBeVisible();
    await expect(
      screen.getByText(/Loading|Synthetic template lookup failure/),
    ).toBeHidden();
    await expect(screen.getByRole("button", /Try again/i)).toBeHidden();
    await app.screenshot("sandbox-template-source-failed");

    await setLookup("success");
    await screen.getByRole("button", "Resume").tap();
    await expect(
      notifications.getByText("The sandbox is running."),
    ).toBeVisible();
    await expect(name).toHaveAttribute("title", MOCK_IDS.template);
    await expect(shortId).toBeHidden();
    await app.screenshot("sandbox-template-source-recovered");
  });
});
