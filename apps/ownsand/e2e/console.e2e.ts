// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

// The suite runs the development server with PUBLIC_OWNSAND_MOCK_API=true
// (see e2e.config.ts): an in-browser fake control plane and a fake signed-in
// user. These IDs and the secret come from src/mock/mock-api.ts.
const ACME = "0193a000-0000-7000-8000-000000000001";
const NORTHWIND = "0193a000-0000-7000-8000-000000000002";
const CONTOSO = "0193a000-0000-7000-8000-000000000003";
const BUILD_BOX = "0193a000-0000-7000-8000-0000000000d1";
const INVITATION_SECRET = "osi_mock_contoso_invitation";
// A seeded Acme invitation. The mock user already belongs to Acme, so
// accepting it fails and leaves the invitation unused.
const ACME_INVITATION_SECRET = "osi_7f3a_unused";

describe("sign-in", () => {
  test("signs in and lists the organizations", async ({ app, screen }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();

    await expect(
      screen.getByRole("heading", "Organizations", { level: 1 }),
    ).toBeVisible();
    await expect(screen.getByRole("link", "Acme Robotics Owner")).toBeVisible();
    await expect(screen.getByRole("link", "Account settings")).toHaveAttribute(
      "href",
      "https://accounts.delino.io",
    );
  });
});

describe("organizations", () => {
  test("creates an organization from a dialog that focuses its only input", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await screen.getByRole("button", "Create organization").tap();

    await expect(screen.getByLabel("Organization name")).toBeFocused();
    await browser.keyboard.type("Fabrikam");
    await screen.getByRole("button", "Create").tap();

    await expect(browser).toHaveURL(/\/orgs\/[0-9a-f-]+\/sandboxes$/);
    await expect(
      screen.getByText("Fabrikam", { exact: false }).first(),
    ).toBeVisible();
  });
});

describe("sandboxes", () => {
  test("lists active sandboxes and shows ended ones on request", async ({
    app,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/sandboxes`);

    await expect(
      screen.getByRole("heading", "Sandboxes", { level: 1 }),
    ).toBeVisible();
    await expect(screen.getByText("Paused")).toBeVisible();
    await expect(screen.getByText("Deleted")).toBeHidden();

    await screen.getByRole("checkbox", "Show ended sandboxes").tap();
    await expect(screen.getByText("Deleted")).toBeVisible();
  });

  test("pauses and resumes a sandbox", async ({ app, screen }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(
      `/orgs/${ACME}/sandboxes/0193a000-0000-7000-8000-0000000000a1`,
    );

    await screen.getByRole("button", "Pause").tap();
    await expect(screen.getByRole("button", "Resume")).toBeVisible();
    await screen.getByRole("button", "Resume").tap();
    await expect(screen.getByRole("button", "Pause")).toBeVisible();
  });
});

describe("API keys", () => {
  test("shows a new key once and closes the dialog with Escape", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/api-keys`);
    await screen.getByRole("button", "Create API key").tap();
    await expect(screen.getByLabel("Name")).toBeFocused();
    await browser.keyboard.type("nightly-ci");
    await screen.getByRole("button", "Create key").tap();

    const dialog = screen.getByRole("dialog", "Your new API key");
    await expect(dialog).toContainText("appears only once");
    await expect(dialog).toContainText("osk_");

    await browser.keyboard.press("Escape");
    await expect(dialog).toBeHidden();
    await expect(screen.getByText("nightly-ci · Active")).toBeVisible();
  });
});

describe("devices", () => {
  test("shows the effective vCPU limit with overcommit", async ({
    app,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/devices/${BUILD_BOX}`);

    await expect(
      screen.getByRole("status", "Effective vCPU limit"),
    ).toContainText("24");
    await expect(screen.getByRole("button", "Remove device")).toBeVisible();
  });
});

describe("roles", () => {
  test("hides admin sections from a member", async ({ app, screen }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${NORTHWIND}/sandboxes`);

    await expect(screen.getByRole("link", "Sandboxes")).toBeVisible();
    await expect(screen.getByRole("link", "API keys")).toBeHidden();
    await expect(screen.getByRole("link", "Enrollment tokens")).toBeHidden();
  });
});

describe("invitations", () => {
  test("accepts an invitation link through sign-in and removes the secret from the address", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open(`/invitations/accept#secret=${INVITATION_SECRET}`);
    await expect(browser).toHaveURL("/invitations/accept");

    await screen.getByRole("button", "Sign in to accept").tap();
    await screen.getByRole("button", "Accept invitation").tap();

    await expect(browser).toHaveURL(`/orgs/${CONTOSO}/sandboxes`);
    await expect(screen.getByRole("navigation", "Organization")).toContainText(
      "Contoso Research",
    );
  });

  test("shows the error of a failed acceptance and keeps the accept button", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open(`/invitations/accept#secret=${ACME_INVITATION_SECRET}`);
    await screen.getByRole("button", "Sign in to accept").tap();
    await screen.getByRole("button", "Accept invitation").tap();

    await expect(screen.getByText("you are already a member")).toBeVisible();
    await expect(screen.getByRole("button", "Accept invitation")).toBeVisible();
    await expect(browser).toHaveURL("/invitations/accept");
  });
});

describe("design", () => {
  test(
    "keeps one primary action on the sandbox list",
    { tags: ["agent"] },
    async ({ app, screen, agent }) => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await app.open(`/orgs/${ACME}/sandboxes`);
      await expect(
        screen.getByRole("heading", "Sandboxes", { level: 1 }),
      ).toBeVisible();

      await expect(screen.getByRole("button", "Create sandbox")).toBeVisible();

      // Prominence is visual: the semantic tree has no button styles, so a
      // judge that reads the tree cannot decide it. Judge the screenshot
      // alone.
      await agent.assert(
        "the page has exactly one prominent primary button, named Create sandbox",
        { vision: "only" },
      );
    },
  );
});
