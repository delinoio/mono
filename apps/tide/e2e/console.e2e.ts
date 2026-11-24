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
const UNKNOWN_ORG = "0193b000-0000-7000-8000-0000000000ff";
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
  test("offers retry when the organization cannot be found", async ({
    app,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(screen.getByRole("link", "Acme Robotics Owner")).toBeVisible();
    await app.open(`/orgs/${UNKNOWN_ORG}`);

    await expect(
      screen.getByText("It was not found, or you no longer have access to it."),
    ).toBeVisible();
    await expect(screen.getByRole("button", "Try again")).toBeVisible();
    await expect(
      screen.getByRole("button", "Sign in with Delino"),
    ).toBeHidden();
  });

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

describe("organization layout", () => {
  test("fits the document and scrolls the section list at narrow widths", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/members`);
    await expect(
      screen.getByRole("heading", "Members", { level: 1 }),
    ).toBeVisible();

    for (const width of [320, 390, 760]) {
      await browser.setViewport({ width, height: 844 });
      const layout = await browser.evaluate(() => {
        const list = document.querySelector(".sidenav ul");
        if (!list) throw new Error("Missing organization section list");
        const root = document.documentElement;
        return {
          documentWidth: root.clientWidth,
          documentScrollWidth: root.scrollWidth,
          listWidth: list.clientWidth,
          listScrollWidth: list.scrollWidth,
          listOverflowX: getComputedStyle(list).overflowX,
        };
      });

      expect(layout.documentScrollWidth, `${width}px document`).toBe(
        layout.documentWidth,
      );
      expect(layout.listScrollWidth, `${width}px section list`).toBeGreaterThan(
        layout.listWidth,
      );
      expect(layout.listOverflowX, `${width}px section list overflow`).toBe(
        "auto",
      );
    }
  });

  test("keeps the 220 px vertical navigation column at desktop widths", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/members`);
    await expect(
      screen.getByRole("heading", "Members", { level: 1 }),
    ).toBeVisible();

    for (const width of [761, 1024]) {
      await browser.setViewport({ width, height: 844 });
      const layout = await browser.evaluate(() => {
        const navigation = document.querySelector(".sidenav");
        const first = navigation?.querySelector("li");
        const second = first?.nextElementSibling;
        if (!navigation || !first || !second) {
          throw new Error("Missing organization section links");
        }
        return {
          columnWidth: navigation.getBoundingClientRect().width,
          firstBottom: first.getBoundingClientRect().bottom,
          secondTop: second.getBoundingClientRect().top,
        };
      });

      expect(layout.columnWidth, `${width}px navigation column`).toBe(220);
      expect(
        layout.secondTop,
        `${width}px section list`,
      ).toBeGreaterThanOrEqual(layout.firstBottom);
    }
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
  test("opens management actions for an integration on the next page", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/integrations`);
    const integration = screen
      .getByRole("list", "Agent integrations")
      .getByRole("listitem")
      .filter({ hasText: "claude-team · Claude Code" });

    await integration.getByRole("button", "Edit").tap();
    await expect(screen.getByRole("dialog", "Edit claude-team")).toBeVisible();
    await browser.keyboard.press("Escape");

    await integration.getByRole("button", "Delete").tap();
    await expect(
      screen.getByRole("alertdialog", "Delete claude-team?"),
    ).toBeVisible();
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

describe("runner profiles", () => {
  test("creates an agent-session profile with an integration on the next page", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/profiles`);
    await expect(
      screen.getByText("Integration: claude-team (Claude Code)"),
    ).toBeVisible();
    await screen.getByRole("button", "Create runner profile").tap();
    await expect(screen.getByLabel("Name")).toBeFocused();
    await browser.keyboard.type("claude-later");
    await screen.getByRole("combobox", "Workload kind").tap();
    await screen.getByRole("option", "Agent session").tap();
    await screen.getByRole("combobox", "Agent integration").tap();
    await screen.getByRole("option", "claude-team (Claude Code)").tap();

    await screen.getByRole("combobox", "Guest OS").tap();
    await screen.getByRole("option", "Windows").tap();
    await screen.getByRole("button", "Create profile").tap();
    await expect(
      screen.getByText(
        "The vendor of this integration does not run on this guest OS.",
      ),
    ).toBeVisible();

    await screen.getByRole("combobox", "Guest OS").tap();
    await screen.getByRole("option", "Linux").tap();
    await screen.getByRole("button", "Create profile").tap();
    await expect(
      screen.getByRole("listitem").filter({ hasText: "claude-later" }),
    ).toContainText("Integration: claude-team (Claude Code)");
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
