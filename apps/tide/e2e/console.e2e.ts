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
  test("returns to sign-in from a direct mock callback visit", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/callback");

    await expect(browser).toHaveURL(/\/$/);
    await expect(
      screen.getByRole("button", "Sign in with Delino"),
    ).toBeVisible();
    await expect(
      screen.getByText(
        "Sign-in did not start. Check your connection and try again.",
      ),
    ).toBeHidden();
  });

  test("signs in and lists the mock organizations in resolved development mode", async ({
    app,
    screen,
  }) => {
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
  test("shows the full error when the organization cannot load", async ({
    app,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(screen.getByRole("link", "Acme Robotics Owner")).toBeVisible();
    await app.open(`/orgs/${UNKNOWN_ORG}/api-keys`);

    await expect(
      screen.getByText("It was not found, or you no longer have access to it."),
    ).toBeVisible();
    await expect(screen.getByRole("button", "Try again")).toBeVisible();
    await expect(screen.getByRole("navigation", "Organization")).toBeHidden();
    await expect(
      screen.getByText("The last refresh failed", { exact: false }),
    ).toBeHidden();
  });

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
    await expect(
      screen.getByText(
        "Sign-in did not start. Check your connection and try again.",
      ),
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

  test("fits organization names and role badges in their home list rows", async ({
    app,
    browser,
    screen,
  }) => {
    const longName = "W".repeat(100);
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await screen.getByRole("button", "Create organization").tap();
    await browser.keyboard.type(longName);
    await screen.getByRole("button", "Create").tap();
    await expect(browser).toHaveURL(/\/orgs\/[0-9a-f-]+$/);
    const createdPath = await browser.evaluate(() => location.pathname);
    await screen.getByRole("link", "tide").tap();

    const list = screen.getByRole("list", "Your organizations");
    const acme = list.getByRole("link", "Acme Robotics Owner");
    const created = list.getByRole("link", `${longName} Owner`);
    const measureRow = (name: string) =>
      browser.evaluate((name) => {
        const list = document.querySelector(
          'ul[aria-label="Your organizations"]',
        );
        const title = Array.from(
          list?.querySelectorAll(".list-title") ?? [],
        ).find((title) => title.textContent === name);
        const row = title?.closest("li");
        const badge = row?.querySelector(".dui-badge");
        if (!list || !title || !row || !badge) {
          throw new Error(`Missing organization row for ${name}`);
        }

        const rowBounds = row.getBoundingClientRect();
        const listBounds = list.getBoundingClientRect();
        const fits = (rect: DOMRect) =>
          [rowBounds, listBounds].every(
            (bounds) =>
              rect.width > 0 &&
              rect.height > 0 &&
              rect.left >= bounds.left &&
              rect.right <= bounds.right &&
              rect.top >= bounds.top &&
              rect.bottom <= bounds.bottom,
          );
        const range = document.createRange();
        range.selectNodeContents(title);
        const fragments = Array.from(range.getClientRects());
        const root = document.documentElement;
        return {
          lines: fragments.length,
          titleFits: fragments.every(fits),
          badgeFits: fits(badge.getBoundingClientRect()),
          documentFits: root.scrollWidth === root.clientWidth,
        };
      }, name);

    for (const width of [1024, 320]) {
      await browser.setViewport({ width, height: 844 });
      await expect(acme.getByText("Owner")).toBeVisible();
      await expect(acme).toHaveAttribute("href", `/orgs/${ACME}`);
      const layout = await measureRow("Acme Robotics");
      expect(layout.lines, `${width}px Acme title`).toBe(1);
      expect(layout.titleFits, `${width}px Acme title fits`).toBe(true);
      expect(layout.badgeFits, `${width}px Acme role fits`).toBe(true);
    }

    await expect(created.getByText("Owner")).toBeVisible();
    const layout = await measureRow(longName);
    expect(layout.lines, "320px long title wraps").toBeGreaterThan(1);
    expect(layout.titleFits, "320px long title fits").toBe(true);
    expect(layout.badgeFits, "320px long title role fits").toBe(true);
    expect(layout.documentFits, "320px document fits").toBe(true);
    await created.tap();
    await expect(browser).toHaveURL(createdPath);
  });

  for (const { section, action, title, removed, remaining } of [
    {
      section: `/orgs/${NORTHWIND}/members`,
      action: "Leave organization",
      title: "Leave this organization?",
      removed: "Northwind Labs Member",
      remaining: "Acme Robotics Owner",
    },
    {
      section: `/orgs/${ACME}/settings`,
      action: "Delete organization",
      title: "Delete Acme Robotics?",
      removed: "Acme Robotics Owner",
      remaining: "Northwind Labs Member",
    },
  ]) {
    test(`returns home after confirming ${action.toLowerCase()}`, async ({
      app,
      browser,
      screen,
    }) => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await expect(screen.getByRole("link", removed)).toBeVisible();
      await app.open(section);
      await screen.getByRole("button", action).tap();
      const confirmation = screen.getByRole("alertdialog", title);
      await expect(confirmation).toBeVisible();
      await confirmation.getByRole("button", action).tap();

      await expect(browser).toHaveURL("/");
      await expect(
        screen.getByRole("heading", "Organizations", { level: 1 }),
      ).toBeVisible();
      await expect(screen.getByRole("link", remaining)).toBeVisible();
      await expect(screen.getByRole("link", removed)).toBeHidden();
    });
  }
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

describe("organization settings", () => {
  test("enables saving only when the admin edits the limit", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(screen.getByRole("link", "Globex Admin")).toBeVisible();
    await app.open(`/orgs/${GLOBEX}/settings`);

    const input = screen.getByLabel("Limit per user");
    const save = screen.getByRole("button", "Save limit");
    await expect(input).toBeFocused();
    await expect(save).toBeDisabled();
    await browser.keyboard.press("Enter");
    await expect(save).toBeDisabled();

    await browser.keyboard.press("ControlOrMeta+A");
    await browser.keyboard.type("4");
    await expect(save).toBeEnabled();
    await save.tap();
    await expect(
      screen
        .getByRole("region", "Notifications")
        .getByText("The dev session limit is saved."),
    ).toBeVisible();
    await expect(input).toHaveValue("4");
    await expect(save).toBeDisabled();
  });
});

describe("organization member pagination", () => {
  test("keeps loaded members visible and transfers ownership to a member from the next page", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(screen.getByRole("link", "Acme Robotics Owner")).toBeVisible();
    await app.open(`/orgs/${ACME}/members`);
    await expect(
      screen.getByText("Some members are not shown yet."),
    ).toBeVisible();
    await expect(screen.getByText("Mina Kim", { exact: false })).toBeVisible();

    await screen.getByRole("link", "Settings").tap();
    await expect(
      screen.getByText("Some members are not shown yet."),
    ).toBeVisible();
    await screen.getByRole("button", "Try again").tap();

    const picker = screen.getByRole("combobox", "New owner");
    await picker.tap();
    await expect(screen.getByRole("option", "Mina Kim")).toBeHidden();
    await screen.getByRole("option", "Nora Lee").tap();
    await screen.getByRole("button", "Transfer").tap();
    await expect(
      screen.getByRole("alertdialog", "Make Nora Lee the owner?"),
    ).toBeVisible();
    await screen.getByRole("button", "Transfer ownership").tap();
    await expect(
      screen.getByRole("heading", "Transfer ownership"),
    ).toBeHidden();

    await screen.getByRole("link", "Members").tap();
    await expect(browser).toHaveURL(`/orgs/${ACME}/members`);
    await expect(screen.getByText("Nora Lee")).toBeVisible();
    const role = await browser.evaluate(
      () =>
        Array.from(
          document.querySelectorAll('[aria-label="Members"] .list-row'),
        )
          .find(
            (row) =>
              row.querySelector(".list-title")?.textContent === "Nora Lee",
          )
          ?.querySelector(".role")?.textContent ?? null,
    );
    expect(role).toBe("Owner");
  });
});

describe("overview", () => {
  test("guides an unlinked organization to integrations from the device pool", async ({
    app,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${GLOBEX}`);

    const pool = screen.getByRole("region", "Device pool");
    const integrations = pool.getByRole("link", "Go to integrations");
    await expect(pool).toContainText(
      "An admin must link an OwnSand organization",
    );
    await expect(pool.getByRole("alert")).toBeHidden();
    await expect(pool.getByRole("button", "Try again")).toBeHidden();
    await expect(integrations).toHaveAttribute(
      "href",
      `/orgs/${GLOBEX}/integrations`,
    );
    await integrations.tap();
    await expect(
      screen.getByRole("heading", "Integrations", { level: 1 }),
    ).toBeVisible();
  });

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
    await expect(screen.getByText("Live")).toHaveAttribute(
      "data-variant",
      "accent",
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

  test("opens a fresh live queue after leaving Overview", async ({
    app,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await screen.getByRole("link", "Acme Robotics Owner").tap();
    const status = screen.getByRole("status", "Queue updates");
    const queue = screen.getByRole("table", "Waiting requests");
    await expect(status).toContainText("Live");
    await expect(queue).toContainText("GitHub job");

    await screen.getByRole("link", "Requests").tap();
    const job = screen
      .getByRole("table", "Requests")
      .getByRole("row")
      .filter({ hasText: "acme-robotics/firmware · test (ubuntu)" });
    await job.getByRole("button", "Cancel").tap();
    const dialog = screen.getByRole("alertdialog", "Cancel this request?");
    await dialog.getByRole("button", "Cancel request").tap();
    await expect(dialog).toBeHidden();
    await expect(job).toBeHidden();

    await screen.getByRole("link", "Overview").tap();
    await expect(status).toContainText("Live");
    await expect(queue).toContainText("Agent session");
    await expect(queue).not.toContainText("GitHub job");
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

describe("audit", () => {
  test("shows the newest event first", async ({ app, browser, screen }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/audit`);

    await expect(screen.getByRole("table", "Audit events")).toBeVisible();
    const actions = await browser.evaluate(() =>
      Array.from(
        document.querySelectorAll(
          'table[aria-label="Audit events"] tbody td:nth-child(2)',
        ),
        (cell) => cell.textContent,
      ),
    );
    expect(actions).toEqual(["Dev session created", "OwnSand link set"]);
  });
});

describe("requests", () => {
  test("shows active requests in queue order before ended requests", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/requests`);

    await screen.getByRole("checkbox", "Show ended requests").tap();
    await expect(screen.getByRole("table", "Requests")).toContainText("deploy");
    const sources = await browser.evaluate(() =>
      Array.from(
        document.querySelectorAll(
          'table[aria-label="Requests"] tbody td:nth-child(4)',
        ),
        (cell) => cell.textContent,
      ),
    );
    expect(sources).toEqual([
      "acme-robotics/firmware · build (ubuntu)",
      "acme-robotics/firmware · test (ubuntu)",
      "sess_codex_4711",
      "—",
      "acme-robotics/site · deploy",
    ]);
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

  test("renames an integration without replacing its credentials", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/integrations`);
    const integrations = screen.getByRole("list", "Agent integrations");
    await integrations
      .getByRole("listitem")
      .filter({ hasText: "claude-team · Claude Code" })
      .getByRole("button", "Edit")
      .tap();
    const dialog = screen.getByRole("dialog", "Edit claude-team");
    await dialog.getByLabel("Name").tap();
    await browser.keyboard.press("ControlOrMeta+A");
    await browser.keyboard.type("claude-renamed");
    await dialog.getByRole("button", "Save changes").tap();

    await expect(dialog).toBeHidden();
    await expect(
      integrations
        .getByRole("listitem")
        .filter({ hasText: "claude-renamed · Claude Code" }),
    ).toContainText("Environment key: Set at");
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

  test("the GitHub callback rejects an installation ID above the signed int64 limit", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(
      "/github/setup?installation_id=9223372036854775808&code=e2e_code&state=mock-state",
    );

    await expect(screen.getByRole("main").getByRole("alert")).toContainText(
      "The address has no GitHub installation details.",
    );
    await expect(browser).toHaveURL("/github/setup");
    await expect(
      screen.getByRole("combobox", "tide organization"),
    ).toBeHidden();
  });

  test("an owner can change the runner group and unlink an installation on the next page", async ({
    app,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/integrations`);

    const list = screen.getByRole("list", "GitHub installations");
    const later = list
      .getByRole("listitem")
      .filter({ hasText: "acme-robotics-201" });
    await expect(later).toBeHidden();
    await screen.getByRole("button", "Show more").tap();
    await expect(later).toBeVisible();
    await expect(list.getByText("acme-robotics · Active")).toBeVisible();
    await expect(screen.getByRole("button", "Show more")).toBeHidden();

    await later.getByRole("button", "Change runner group").tap();
    const runnerGroup = screen.getByRole("dialog", "Change the runner group");
    await runnerGroup.getByRole("combobox", "Runner group").tap();
    await screen.getByRole("option", "tide-private · selected").tap();
    await runnerGroup.getByRole("button", "Save").tap();
    await expect(runnerGroup).toBeHidden();
    await expect(later).toContainText("Runner group: tide-private");

    await later.getByRole("button", "Unlink").tap();
    const confirm = screen.getByRole(
      "alertdialog",
      "Unlink acme-robotics-201?",
    );
    await confirm.getByRole("button", "Unlink").tap();
    await expect(later).toBeHidden();
    await expect(list.getByText("acme-robotics · Active")).toBeVisible();
  });

  test("a member can read a later page and switch to another organization's list", async ({
    app,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${NORTHWIND}/integrations`);

    const list = screen.getByRole("list", "GitHub installations");
    const later = list
      .getByRole("listitem")
      .filter({ hasText: "northwind-201" });
    await screen.getByRole("button", "Show more").tap();
    await expect(later).toBeVisible();
    await expect(later.getByRole("button", "Unlink")).toBeHidden();
    await expect(later.getByRole("button", "Change runner group")).toBeHidden();

    await screen.getByRole("combobox", "Organization").tap();
    await screen.getByRole("option", "Acme Robotics").tap();
    await expect(list.getByText("acme-robotics · Active")).toBeVisible();
    await expect(list.getByText("northwind · Active")).toBeHidden();
    await expect(later).toBeHidden();
    await expect(screen.getByRole("button", "Show more")).toBeVisible();
  });

  test("a GitHub install response cannot redirect a later Integrations visit", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/integrations`);
    await expect(screen.getByRole("button", "Connect GitHub")).toBeVisible();

    const redirects = await browser.evaluate(async () => {
      const button = document.querySelector<HTMLButtonElement>(
        "#github-title + button",
      )!;
      const nav = document.querySelector('nav[aria-label="Organization"]')!;
      const members = nav.querySelector<HTMLAnchorElement>(
        'a[href$="/members"]',
      )!;
      const integrations = nav.querySelector<HTMLAnchorElement>(
        'a[href$="/integrations"]',
      )!;
      let redirects = 0;
      const recordRedirect = (event: NavigateEvent) => {
        if (new URL(event.destination.url).origin === "https://github.com") {
          event.preventDefault();
          redirects += 1;
        }
      };
      window.navigation.addEventListener("navigate", recordRedirect);
      try {
        // Both links run before the in-memory API response can resolve.
        button.click();
        members.click();
        integrations.click();
        await new Promise((resolve) => setTimeout(resolve, 0));
        return redirects;
      } finally {
        window.navigation.removeEventListener("navigate", recordRedirect);
      }
    });

    expect(redirects).toBe(0);
    await expect(browser).toHaveURL(`/orgs/${ACME}/integrations`);
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
  test("uses a later profile across pages and refreshes after profile changes", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/profiles`);

    const profiles = screen.getByRole("list", "Runner profiles");
    const laterProfile = profiles.getByRole("listitem").filter({
      hasText: "linux-build ·",
    });
    await expect(laterProfile).toBeVisible();
    await expect(laterProfile.getByRole("button", "Delete")).toBeVisible();
    expect(
      await browser.evaluate(
        () =>
          document.querySelectorAll('ul[aria-label="Runner profiles"] > li')
            .length,
      ),
    ).toBe(201);

    await laterProfile.getByRole("button", "Edit").tap();
    const edit = screen.getByRole("dialog", "Edit linux-build");
    await edit.getByLabel("Name").tap();
    await browser.keyboard.press("ControlOrMeta+A");
    await browser.keyboard.type("linux-build-renamed");
    await edit.getByRole("button", "Save changes").tap();
    await expect(laterProfile).toBeHidden();

    for (const page of ["Requests", "Runners"]) {
      await screen.getByRole("link", page).tap();
      await expect(screen.getByRole("table", page)).toContainText(
        "linux-build-renamed",
      );
      const filter = screen.getByRole("combobox", "Runner profile");
      await filter.tap();
      await screen.getByRole("option", "linux-build-renamed").tap();
      await expect(filter).toContainText("linux-build-renamed");
      await expect(screen.getByRole("table", page)).toContainText(
        "linux-build-renamed",
      );
    }
    await screen.getByRole("link", "Overview").tap();
    await expect(screen.getByRole("table", "Waiting requests")).toContainText(
      "linux-build-renamed",
    );

    await screen.getByRole("link", "Runner profiles").tap();
    await screen.getByRole("combobox", "Organization").tap();
    await screen.getByRole("option", "Northwind Labs").tap();
    await expect(profiles).toContainText("mac-build");
    await expect(profiles).not.toContainText("linux-build-renamed");

    await screen.getByRole("combobox", "Organization").tap();
    await screen.getByRole("option", "Acme Robotics").tap();
    const renamedProfile = profiles.getByRole("listitem").filter({
      hasText: "linux-build-renamed ·",
    });
    await renamedProfile.getByRole("button", "Delete").tap();
    await screen
      .getByRole("alertdialog", "Delete linux-build-renamed?")
      .getByRole("button", "Delete profile")
      .tap();
    await expect(renamedProfile).toBeHidden();
    await expect(
      profiles.getByRole("listitem").filter({ hasText: "fixture-linux-197 ·" }),
    ).toBeVisible();

    await screen.getByRole("button", "Create runner profile").tap();
    const create = screen.getByRole("dialog", "Create a runner profile");
    await create.getByLabel("Name").tap();
    await browser.keyboard.type("new-linux");
    await create.getByRole("button", "Create profile").tap();
    await expect(
      profiles.getByRole("listitem").filter({ hasText: "new-linux ·" }),
    ).toBeVisible();
  });

  test("lets the user retry an edit when a runner profile name already exists", async ({
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
    await expect(profile.getByLabel("runs-on label of linux-build")).toHaveText(
      "tide-linux-build",
    );
    await dialog.getByLabel("Name").tap();
    await browser.keyboard.press("ControlOrMeta+A");
    await browser.keyboard.type("linux-build-retry");
    await dialog.getByRole("button", "Save changes").tap();
    await expect(dialog).toBeHidden();
    await expect(
      screen.getByLabel("runs-on label of linux-build-retry"),
    ).toHaveText("tide-linux-build-retry");
  });

  test("shows field errors and saves after correcting the resource values", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/profiles`);
    await screen.getByRole("button", "Create runner profile").tap();

    const dialog = screen.getByRole("dialog", "Create a runner profile");
    await dialog.getByLabel("Name").tap();
    await browser.keyboard.type("bounds-test");
    const resources = [
      [
        "vCPUs",
        "3000000000",
        "1024",
        "Enter a whole number of vCPUs from 1 to 1024.",
      ],
      [
        "Memory (MiB)",
        "10000000000000000000",
        "16777216",
        "Enter a whole number of MiB from 1 to 16777216.",
      ],
      [
        "Disk (GiB)",
        "1048577",
        "1048576",
        "Enter a whole number of GiB from 1 to 1048576.",
      ],
    ] as const;
    for (const [label, invalid] of resources) {
      await dialog.getByLabel(label).tap();
      await browser.keyboard.press("ControlOrMeta+A");
      await browser.keyboard.type(invalid);
    }
    await dialog.getByRole("button", "Create profile").tap();

    for (const [label, , , message] of resources) {
      await expect(dialog.getByLabel(label)).toHaveAttribute(
        "aria-invalid",
        "true",
      );
      await expect(dialog.getByText(message)).toBeVisible();
    }
    await expect(
      dialog.getByText("Something went wrong. Try again."),
    ).toBeHidden();
    for (const [label, , valid] of resources) {
      await dialog.getByLabel(label).tap();
      await browser.keyboard.press("ControlOrMeta+A");
      await browser.keyboard.type(valid);
    }
    await dialog.getByRole("button", "Create profile").tap();

    await expect(dialog).toBeHidden();
    const row = screen.getByRole("listitem").filter({ hasText: "bounds-test" });
    await expect(row).toContainText("1024 vCPU · 16777216 MiB · 1048576 GiB");
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

describe("API key revocation", () => {
  test("Cancel keeps the key active", async ({ app, screen }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/api-keys`);
    const revoke = screen.getByRole("button", "Revoke", { exact: true });
    const dialog = screen.getByRole("alertdialog", "Revoke claude-on-laptop?");

    await revoke.tap();
    await dialog.getByRole("button", "Cancel").tap();
    await expect(dialog).toBeHidden();
    await expect(screen.getByText("claude-on-laptop · Active")).toBeVisible();
  });

  test("confirming revokes the key", async ({ app, screen }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/api-keys`);
    await screen.getByRole("button", "Revoke", { exact: true }).tap();
    const dialog = screen.getByRole("alertdialog", "Revoke claude-on-laptop?");
    await dialog.getByRole("button", "Revoke key").tap();
    await expect(dialog).toBeHidden();
    await expect(screen.getByText("claude-on-laptop · Revoked")).toBeVisible();
  });
});

describe("invitations", () => {
  test("creates and revokes an invitation as an admin and hides the controls from members", async ({
    app,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${GLOBEX}/members`);
    await expect(screen.getByText("No pending invitations")).toBeVisible();

    await screen.getByRole("button", "Invite someone").tap();
    await screen.getByRole("button", "Create link").tap();
    const link = screen.getByRole("dialog", "Your invitation link");
    await expect(link).toContainText("/invitations/accept#secret=");
    await screen.getByRole("button", "Done").tap();
    await expect(link).toBeHidden();
    await screen.getByRole("button", "Revoke").tap();
    await expect(screen.getByText("No pending invitations")).toBeVisible();

    await app.open(`/orgs/${NORTHWIND}/members`);
    await expect(
      screen.getByRole("heading", "Members", { level: 1 }),
    ).toBeVisible();
    await expect(screen.getByRole("button", "Invite someone")).toBeHidden();
  });

  test("accepts the replacement invitation after a fragment-only navigation", async ({
    app,
    screen,
    browser,
  }) => {
    const path = "/invitations/accept?source=fixture";
    const firstSecret = "tdi_fixture_a";
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(screen.getByRole("link", "Acme Robotics Owner")).toBeVisible();
    await app.open(`${path}#secret=${firstSecret}`);

    const join = screen.getByRole("button", "Join organization");
    const invalid = screen.getByText(
      "It was not found, or you no longer have access to it.",
    );
    await expect(browser).toHaveURL(path);
    await join.tap();
    await expect(invalid).toBeVisible();

    await browser.evaluate((secret) => {
      window.location.hash = `secret=${secret}`;
      return null;
    }, INVITATION_SECRET);

    await expect(browser).toHaveURL(path);
    await expect(invalid).toBeHidden();
    await join.tap();

    await expect(browser).toHaveURL(`/orgs/${CONTOSO}`);
    await expect(screen.getByRole("combobox", "Organization")).toContainText(
      "Contoso",
    );
  });

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
    await screen.getByRole("combobox", "Organization").tap();
    await expect(screen.getByRole("option", "Contoso")).toBeVisible();

    await browser.keyboard.press("Escape");
    await screen.getByRole("link", "tide", { exact: true }).tap();
    await expect(browser).toHaveURL(/\/$/);
    await expect(screen.getByRole("link", "Contoso Member")).toBeVisible();
  });

  test("an expired invitation link keeps the user on the invitation page", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/invitations/accept#secret=${INVITATION_SECRET}`);
    const join = screen.getByRole("button", "Join organization");
    await expect(join).toBeVisible();

    // Advance the clock after the mock creates its seeded invitation.
    await browser.evaluate(() => {
      const later = Date.now() + 7 * 24 * 60 * 60 * 1000;
      Date.now = () => later;
      return null;
    });
    await join.tap();

    await expect(
      screen.getByText("It was not found, or you no longer have access to it."),
    ).toBeVisible();
    await expect(browser).toHaveURL("/invitations/accept");
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
