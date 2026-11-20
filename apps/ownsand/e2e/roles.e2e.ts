// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import {
  DEVICE_ID,
  ORGANIZATION_ID,
  installMockApi,
  organizationHandlers,
  signInBeforeLoad,
} from "./mock-api.ts";

describe("role-based visibility", () => {
  test("hides admin routes and actions from a member", async ({
    app,
    screen,
    browser,
  }) => {
    await signInBeforeLoad(browser);
    await installMockApi(browser, organizationHandlers("ROLE_MEMBER"));

    await app.open(`/orgs/${ORGANIZATION_ID}/devices/${DEVICE_ID}`);

    await expect(
      screen.getByRole("heading", "build-mac", { level: 1 }),
    ).toBeVisible();
    await expect(screen.getByRole("link", "Members")).toBeVisible();
    await expect(screen.getByRole("link", "API keys")).toHaveCount(0);
    await expect(screen.getByRole("link", "Enrollment tokens")).toHaveCount(0);
    await expect(screen.getByRole("heading", "Admin settings")).toHaveCount(0);
  });

  test("shows the per-device admin settings to an admin", async ({
    app,
    screen,
    browser,
  }) => {
    await signInBeforeLoad(browser);
    await installMockApi(browser, organizationHandlers("ROLE_ADMIN"));

    await app.open(`/orgs/${ORGANIZATION_ID}/devices/${DEVICE_ID}`);

    await expect(
      screen.getByRole("heading", "Admin settings", { level: 2 }),
    ).toBeVisible();
    await expect(screen.getByRole("button", "Save limits")).toBeVisible();
    await expect(screen.getByRole("button", "Remove device")).toBeVisible();
    await expect(screen.getByRole("link", "API keys")).toBeVisible();
  });

  test("shows ownership transfer and deletion only to the owner", async ({
    app,
    screen,
    browser,
  }) => {
    await signInBeforeLoad(browser);
    await installMockApi(browser, organizationHandlers("ROLE_ADMIN"));

    await app.open(`/orgs/${ORGANIZATION_ID}/settings`);

    await expect(screen.getByRole("button", "Save policy")).toBeVisible();
    await expect(screen.getByRole("button", "Delete organization")).toHaveCount(
      0,
    );
    await expect(screen.getByRole("button", "Transfer ownership")).toHaveCount(
      0,
    );
  });
});
