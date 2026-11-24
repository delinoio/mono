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
    await expect(screen.getByRole("combobox", "Organization")).toContainText(
      "Fabrikam",
    );
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
  test("shows the live queue and the device pool", async ({
    app,
    browser,
    screen,
  }) => {
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
    const waitingKinds = await browser.evaluate(() =>
      Array.from(
        document.querySelectorAll(
          'table[aria-label="Waiting requests"] tbody td:nth-child(3)',
        ),
        (cell) => cell.textContent,
      ),
    );
    expect(waitingKinds).toEqual([
      "GitHub job",
      "Agent session",
      "Dev session",
    ]);
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
  test("keeps the integration edit dialog open after a duplicate rename", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/integrations`);
    await screen
      .getByRole("listitem")
      .filter({ hasText: "claude-team" })
      .getByRole("button", "Edit")
      .tap();
    await screen.getByLabel("Name").tap();
    await browser.keyboard.press("ControlOrMeta+A");
    await browser.keyboard.type("codex-prod");
    await screen.getByRole("button", "Save changes").tap();

    await expect(screen.getByRole("dialog", "Edit claude-team")).toBeVisible();
    await expect(
      screen.getByText("an agent integration with this name exists"),
    ).toBeVisible();
    await expect(screen.getByText("codex-prod · OpenAI Codex")).toBeVisible();
    await expect(screen.getByText("claude-team · Claude Code")).toBeVisible();
  });

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
    const setupHistoryLength = await browser.evaluate(
      () => window.history.length,
    );
    await screen.getByRole("combobox", "tide organization").tap();
    await screen.getByRole("option", "Globex (admin)").tap();
    await screen.getByRole("button", "Link installation").tap();

    await expect(browser).toHaveURL(`/orgs/${GLOBEX}/integrations`);
    expect(await browser.evaluate(() => window.history.length)).toBe(
      setupHistoryLength,
    );
    await expect(
      screen.getByText("github-org-51000001", { exact: false }),
    ).toBeVisible();
  });

  test("an expired GitHub callback stays on setup and shows restart guidance", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${GLOBEX}/integrations`);

    await browser.evaluate(() => {
      const issuedAt = Date.now();
      Date.now = () => issuedAt;
      // Simulate the GitHub redirect in the same document so the callback
      // uses the in-memory transport that issued the state.
      window.navigation.addEventListener(
        "navigate",
        (event) => {
          const destination = new URL(event.destination.url);
          if (destination.origin !== "https://github.com") return;
          event.preventDefault();
          const state = destination.searchParams.get("state");
          if (!state) throw new Error("Expected an issued state");
          const query = new URLSearchParams({
            installation_id: "51000002",
            code: "fixture-code",
            state,
          });
          Date.now = () => issuedAt + 600_000;
          setTimeout(() => {
            window.history.pushState(null, "", `/github/setup?${query}`);
            window.dispatchEvent(new PopStateEvent("popstate"));
          }, 0);
        },
        { once: true },
      );
      return null;
    });
    await screen.getByRole("button", "Connect GitHub").tap();
    await screen.getByRole("combobox", "tide organization").tap();
    await screen.getByRole("option", "Globex (admin)").tap();
    await screen.getByRole("button", "Link installation").tap();

    await expect(
      screen.getByText("Start the installation again from the integrations", {
        exact: false,
      }),
    ).toBeVisible();
    await expect(browser).toHaveURL("/github/setup");
  });
});

describe("runner profiles", () => {
  test("keeps the edit dialog open when a runner profile name already exists", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/profiles`);

    const profile = screen
      .getByRole("listitem")
      .filter({ hasText: "linux-build ·" });
    await profile.getByRole("button", "Edit").tap();
    const dialog = screen.getByRole("dialog", "Edit linux-build");
    await dialog.getByLabel("Name").tap();
    await browser.keyboard.press("ControlOrMeta+A");
    await browser.keyboard.type("codex-linux");
    await dialog.getByRole("button", "Save changes").tap();

    await expect(dialog).toBeVisible();
    await expect(
      dialog.getByText("a runner profile with this name exists"),
    ).toBeVisible();
    await dialog.getByRole("button", "Cancel").tap();
    await expect(profile.getByLabel("runs-on label of linux-build")).toHaveText(
      "tide-linux-build",
    );
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
    await expect(screen.getByRole("link", "Acme Robotics Owner")).toBeVisible();
    await screen.getByLabel("Invitation link or secret").tap();
    await browser.keyboard.type(INVITATION_SECRET);
    await screen.getByRole("button", "Continue").tap();

    await expect(browser).toHaveURL("/invitations/accept");
    await screen.getByRole("button", "Join organization").tap();

    await expect(browser).toHaveURL(`/orgs/${CONTOSO}`);
    await expect(screen.getByRole("navigation", "Organization")).toContainText(
      "Contoso",
    );
    await expect(screen.getByRole("combobox", "Organization")).toContainText(
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
