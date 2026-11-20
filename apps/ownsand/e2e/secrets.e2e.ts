// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import {
  ORGANIZATION_ID,
  installMockApi,
  organizationHandlers,
  signInBeforeLoad,
} from "./mock-api.ts";

describe("one-time secrets and confirmations", () => {
  test("shows a new API key once and closes with Escape", async ({
    app,
    screen,
    browser,
  }) => {
    await signInBeforeLoad(browser);
    const calls = await installMockApi(
      browser,
      organizationHandlers("ROLE_ADMIN"),
    );
    await app.open(`/orgs/${ORGANIZATION_ID}/api-keys`);

    await screen.getByRole("button", "Create API key").click();
    // The single critical input of the form has focus.
    await expect(screen.getByLabel("Key name")).toBeFocused();
    await browser.keyboard.type("deploy-bot");
    await screen.getByRole("button", "Create key").click();

    const dialog = screen.getByRole("dialog", "API key");
    await expect(dialog).toContainText("This will not be shown again");
    await expect(
      screen.getByDisplayValue("osk_e2e_one_time_secret"),
    ).toBeVisible();
    await expect(screen.getByRole("button", "Copy")).toBeVisible();
    expect(
      calls.find((call) => call.procedure === "ApiKeyService/CreateApiKey")
        ?.body,
    ).toEqual({ organizationId: ORGANIZATION_ID, name: "deploy-bot" });

    await browser.keyboard.press("Escape");

    await expect(screen.getByRole("dialog")).toHaveCount(0);
    await expect(screen.getByText("osk_e2e_one_time_secret")).toHaveCount(0);
  });

  test("revokes an API key only after confirmation", async ({
    app,
    screen,
    browser,
  }) => {
    await signInBeforeLoad(browser);
    const calls = await installMockApi(
      browser,
      organizationHandlers("ROLE_ADMIN"),
    );
    await app.open(`/orgs/${ORGANIZATION_ID}/api-keys`);

    await screen.getByRole("button", "Revoke ci").click();
    await expect(screen.getByRole("alertdialog", "Revoke ci?")).toBeVisible();
    await screen.getByRole("button", "Cancel").click();
    await expect(screen.getByRole("alertdialog")).toHaveCount(0);
    expect(
      calls.some((call) => call.procedure === "ApiKeyService/RevokeApiKey"),
    ).toBe(false);

    await screen.getByRole("button", "Revoke ci").click();
    await screen.getByRole("button", "Revoke API key").click();

    await expect(screen.getByRole("alertdialog")).toHaveCount(0);
    expect(
      calls.filter((call) => call.procedure === "ApiKeyService/RevokeApiKey")
        .length,
    ).toBe(1);
  });
});
