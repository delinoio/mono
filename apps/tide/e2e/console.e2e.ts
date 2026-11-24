// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

// The suite runs the development server with PUBLIC_TIDE_MOCK_API=true (see
// e2e.config.ts): an in-browser fake control plane and a fake signed-in
// user. These IDs and the secret come from src/mock/mock-api.ts.
const ACME = "0193b000-0000-7000-8000-000000000001";
const NORTHWIND = "0193b000-0000-7000-8000-000000000002";
const CONTOSO = "0193b000-0000-7000-8000-000000000003";
const GLOBEX = "0193b000-0000-7000-8000-000000000004";
const INVITATION_SECRET = "tdi_mock_contoso_invitation";

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
    await expect(screen.getByRole("link", "OwnSand console")).toHaveAttribute(
      "href",
      "https://ownsand.delino.io",
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

    await expect(browser).toHaveURL(/\/orgs\/[0-9a-f-]+$/);
    await expect(screen.getByText("Set up tide")).toBeVisible();
  });
});

describe("overview", () => {
  test("shows the live queue and the device pool", async ({ app, screen }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}`);

    await expect(
      screen.getByRole("heading", "Overview", { level: 1 }),
    ).toBeVisible();
    await expect(screen.getByRole("status", "Queue updates")).toContainText(
      "Live",
    );
    await expect(screen.getByRole("table", "Waiting requests")).toContainText(
      "GitHub job",
    );
    await expect(screen.getByRole("table", "Devices")).toContainText(
      "build-box",
    );
  });

  test("an invalid OwnSand link shows the dispatch banner", async ({
    app,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${NORTHWIND}/requests`);

    await expect(screen.getByRole("alert", "OwnSand link")).toContainText(
      "Dispatch has stopped",
    );
    await expect(
      screen.getByRole("link", "Go to integrations"),
    ).toHaveAttribute("href", `/orgs/${NORTHWIND}/integrations`);
  });
});

describe("integrations", () => {
  test("saves a Cursor integration name, pool, and credential together", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${GLOBEX}/integrations`);
    await screen.getByRole("button", "Create agent integration").tap();
    await screen.getByRole("combobox", "Vendor").tap();
    await screen.getByRole("option", "Cursor").tap();
    await screen.getByLabel("Name").tap();
    await browser.keyboard.type("cursor-before");
    await screen.getByLabel("Cursor self-hosted pool name").tap();
    await browser.keyboard.type("pool-before");
    await screen.getByLabel("Service account API key").tap();
    await browser.keyboard.type("synthetic-cursor-before");
    await screen.getByRole("button", "Create integration").tap();

    await screen.getByRole("button", "Edit").tap();
    await screen.getByLabel("Name").tap();
    await browser.keyboard.press("ControlOrMeta+A");
    await browser.keyboard.type("cursor-after");
    await screen.getByLabel("Cursor self-hosted pool name").tap();
    await browser.keyboard.press("ControlOrMeta+A");
    await browser.keyboard.type("pool-after");
    await screen.getByLabel("Service account API key").tap();
    await browser.keyboard.type("synthetic-cursor-after");
    await screen.getByRole("button", "Save changes").tap();

    const row = screen
      .getByRole("listitem")
      .filter({ hasText: "cursor-after" });
    await expect(row).toContainText("Cursor self-hosted pool name: pool-after");
    await expect(row).toContainText("Service account API key: Set at");
  });

  test("links OwnSand and shows only when the key was set", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${GLOBEX}/integrations`);
    await screen.getByRole("button", "Link OwnSand").tap();

    await expect(screen.getByLabel("OwnSand organization ID")).toBeFocused();
    await browser.keyboard.type("0193a000-0000-7000-8000-00000000aaaa");
    await screen.getByLabel("OwnSand API key").tap();
    await browser.keyboard.type("osk_e2e_secret");
    await screen.getByRole("button", "Link").tap();

    await expect(screen.getByText("Set at", { exact: false })).toBeVisible();
    await expect(screen.getByText("osk_e2e_secret")).toBeHidden();
  });

  test("the GitHub callback links the installation and removes code and state", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(
      "/github/setup?installation_id=51000001&code=e2e_code&state=mock-state",
    );

    await expect(browser).toHaveURL("/github/setup");
    await screen.getByRole("combobox", "tide organization").tap();
    await screen.getByRole("option", "Globex (admin)").tap();
    await screen.getByRole("button", "Link installation").tap();

    await expect(browser).toHaveURL(`/orgs/${GLOBEX}/integrations`);
    await expect(
      screen.getByText("github-org-51000001", { exact: false }),
    ).toBeVisible();
  });
});

describe("personal API keys", () => {
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
    await browser.keyboard.type("codex-agent");
    await screen.getByRole("button", "Create key").tap();

    const dialog = screen.getByRole("dialog", "Your new personal API key");
    await expect(dialog).toContainText("appears only once");
    await expect(dialog).toContainText("works only for dev sessions");
    await expect(dialog).toContainText("tide_");

    await browser.keyboard.press("Escape");
    await expect(dialog).toBeHidden();
    await expect(
      screen.getByText("codex-agent", { exact: false }),
    ).toBeVisible();
  });
});

describe("invitations", () => {
  test("accepts an invitation link and removes the secret from the address", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/invitations/accept#secret=${INVITATION_SECRET}`);

    await expect(browser).toHaveURL("/invitations/accept");
    await screen.getByRole("button", "Join organization").tap();

    await expect(browser).toHaveURL(`/orgs/${CONTOSO}`);
    await expect(screen.getByRole("navigation", "Organization")).toContainText(
      "Contoso",
    );
  });
});

describe("routing", () => {
  test("shows Page not found for an unknown path", async ({ app, screen }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open("/orgs/x/unknown");

    await expect(
      screen.getByRole("heading", "Page not found", { level: 1 }),
    ).toBeVisible();
  });
});
