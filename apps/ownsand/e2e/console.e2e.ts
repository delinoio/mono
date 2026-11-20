// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import {
  E2E_API_ORIGIN,
  ORGANIZATION_ID,
  installMockApi,
  organizationHandlers,
  signInBeforeLoad,
} from "./mock-api.ts";

describe("sign-in and organizations", () => {
  test("asks a signed-out user to sign in", async ({
    app,
    screen,
    browser,
  }) => {
    await installMockApi(browser, organizationHandlers("ROLE_OWNER"));

    await app.open("/");

    await expect(
      screen.getByRole("heading", "Sign in to OwnSand", { level: 3 }),
    ).toBeVisible();
    await expect(screen.getByRole("button", "Sign in")).toBeVisible();
  });

  test("lists organizations after sign-in with a bearer token", async ({
    app,
    screen,
    browser,
  }) => {
    const calls = await installMockApi(
      browser,
      organizationHandlers("ROLE_OWNER"),
    );
    await app.open("/");

    await screen.getByRole("button", "Sign in").click();

    await expect(screen.getByRole("link", /Acme/)).toBeVisible();
    const list = calls.find(
      (call) => call.procedure === "OrganizationService/ListOrganizations",
    );
    expect(list?.authorization).toBe("Bearer e2e-fake-access-token");
  });

  test("links to account settings in delino-accounts", async ({
    app,
    screen,
    browser,
  }) => {
    await signInBeforeLoad(browser);
    await installMockApi(browser, organizationHandlers("ROLE_OWNER"));

    await app.open("/");

    await expect(screen.getByRole("link", "Account settings")).toHaveAttribute(
      "href",
      "https://accounts.delino.io",
    );
  });

  test("exposes the configured API origin on the document", async ({
    app,
    browser,
  }) => {
    await installMockApi(browser, organizationHandlers("ROLE_OWNER"));

    await app.open("/");

    await expect(browser.locator("html")).toHaveAttribute(
      "data-api-origin",
      E2E_API_ORIGIN,
    );
  });

  test("lists sandboxes with state, device, resources and labels", async ({
    app,
    screen,
    browser,
  }) => {
    await signInBeforeLoad(browser);
    await installMockApi(browser, organizationHandlers("ROLE_MEMBER"));

    await app.open(`/orgs/${ORGANIZATION_ID}/sandboxes`);

    await expect(
      screen.getByRole("heading", "Sandboxes", { level: 1 }),
    ).toBeVisible();
    await expect(screen.getByText("Running")).toBeVisible();
    await expect(screen.getByText("2 vCPU · 4096 MiB · 20 GiB")).toBeVisible();
    await expect(screen.getByText("team=infra")).toBeVisible();
  });
});
