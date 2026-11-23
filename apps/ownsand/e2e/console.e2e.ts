// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect, type Screen } from "e2e";

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

describe("routing", () => {
  test("shows Page not found for a malformed percent escape and recovers", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(
      screen.getByRole("heading", "Organizations", { level: 1 }),
    ).toBeVisible();

    // The development server answers a raw malformed URL with 400 before the
    // app loads, so the failure is reachable only through client-side
    // navigation. Push the malformed path and fire popstate, as a history
    // traversal does, so the console parses it in the browser.
    await browser.evaluate(() => {
      window.history.pushState(null, "", "/orgs/%/sandboxes");
      window.dispatchEvent(new PopStateEvent("popstate"));
      return true;
    });

    await expect(
      screen.getByRole("heading", "Page not found", { level: 1 }),
    ).toBeVisible();

    await screen.getByRole("link", "Go to your organizations").tap();

    await expect(browser).toHaveURL("/");
    await expect(
      screen.getByRole("heading", "Organizations", { level: 1 }),
    ).toBeVisible();
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

  test("removes a device and drops it from the device list", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/devices`);
    await expect(screen.getByText("build-box-1").first()).toBeVisible();
    await expect(screen.getByText("studio-mac").first()).toBeVisible();

    await app.open(`/orgs/${ACME}/devices/${BUILD_BOX}`);
    await screen.getByRole("button", "Remove device").tap();
    const dialog = screen.getByRole("alertdialog", "Remove build-box-1?");
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", "Remove device").tap();

    // The console returns to the device list. The mock API omits a removed
    // device from a list request that sets no state, like the real API.
    await expect(browser).toHaveURL(`/orgs/${ACME}/devices`);
    await expect(screen.getByText("studio-mac").first()).toBeVisible();
    await expect(screen.getByText("build-box-1")).toBeHidden();
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

describe("billing", () => {
  test("shows the suspension banner to a member", async ({ app, screen }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    // The mock API seeds Northwind as suspended.
    await app.open(`/orgs/${NORTHWIND}/sandboxes`);

    const banner = screen.getByRole("region", "This organization is suspended");
    await expect(banner).toBeVisible();
    await expect(banner).toContainText("Running sandboxes are paused");
    await expect(
      banner.getByRole("link", "Manage billing at accounts.delino.io"),
    ).toHaveAttribute("href", "https://accounts.delino.io");
    await expect(screen.getByRole("region", "Billing")).toBeHidden();
  });

  test("shows the billing account and this month's usage to the owner", async ({
    app,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/settings`);

    const section = screen.getByRole("region", "Billing");
    await expect(section).toContainText("Acme Robotics billing");
    await expect(section).toContainText("$42.52");
    await expect(section).toContainText("Class A RPCs");
    await expect(section).toContainText("$0.00558");
    await expect(
      screen.getByRole("region", "This organization is blocked"),
    ).toBeHidden();
  });

  test("unlinks after a confirmation and links again", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/settings`);

    await screen.getByRole("button", "Unlink billing account").tap();
    const confirm = screen.getByRole("alertdialog");
    await expect(confirm).toContainText("becomes blocked at once");
    await confirm.getByRole("button", "Unlink billing account").tap();
    await expect(confirm).toBeHidden();
    await expect(
      screen.getByRole("region", "This organization is blocked"),
    ).toBeVisible();

    await screen.getByRole("button", "Link a billing account").tap();
    const dialog = screen.getByRole("dialog", "Link a billing account");
    await expect(
      dialog.getByRole("radio", "Acme Robotics billing · Owner"),
    ).toBeFocused();
    await browser.keyboard.press("Space");
    await dialog.getByRole("button", "Link").tap();
    await expect(dialog).toBeHidden();
    await expect(
      screen.getByRole("region", "This organization is blocked"),
    ).toBeHidden();
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

// Layout metrics of the current page. `overflow` is true when the document
// scrolls horizontally. `clientWidth` (not `innerWidth`) is the comparison
// width: a classic vertical scrollbar takes space from it, so the document of
// a page that fits has `scrollWidth === clientWidth`, below `innerWidth`.
// `offscreen` lists the visible interactive controls that reach beyond the
// viewport, except controls inside a `.table-wrap` or the section list, which
// scroll inside themselves on purpose.
interface LayoutMetrics {
  scrollWidth: number;
  clientWidth: number;
  navListScrollWidth: number;
  navListClientWidth: number;
  navListLinksReachable: boolean;
  navColumnWidth: number;
  navListFlexDirection: string;
  offscreen: string[];
}

async function measureLayout(browser: {
  evaluate<T extends import("e2e").JsonValue>(fn: () => T): Promise<T>;
}): Promise<LayoutMetrics> {
  return browser.evaluate(() => {
    const doc = document.documentElement;
    const nav = document.querySelector(".sidenav");
    const list = document.querySelector(".sidenav ul");
    const offscreen: string[] = [];
    for (const el of Array.from(
      document.querySelectorAll("a, button, input, select, textarea"),
    )) {
      if (el.closest(".table-wrap") || el.closest(".sidenav ul")) continue;
      const box = el.getBoundingClientRect();
      if (box.width === 0 || box.height === 0) continue;
      if (box.right > doc.clientWidth + 0.5 || box.left < -0.5) {
        offscreen.push(
          `${el.tagName.toLowerCase()} "${(el.textContent ?? "").trim().slice(0, 40)}" right=${Math.round(box.right)}`,
        );
      }
    }
    // Every section link must be reachable: either inside the viewport or
    // inside the scrollable list, so scrolling the list to its end shows it.
    let reachable = true;
    if (list) {
      const listBox = list.getBoundingClientRect();
      for (const a of Array.from(list.querySelectorAll("a"))) {
        const box = a.getBoundingClientRect();
        const start = box.left - listBox.left + list.scrollLeft;
        if (start < -0.5 || start + box.width > list.scrollWidth + 0.5) {
          reachable = false;
        }
      }
      if (listBox.left < -0.5 || listBox.right > doc.clientWidth + 0.5) {
        reachable = false;
      }
    }
    return {
      scrollWidth: doc.scrollWidth,
      clientWidth: doc.clientWidth,
      navListScrollWidth: list ? list.scrollWidth : 0,
      navListClientWidth: list ? list.clientWidth : 0,
      navListLinksReachable: reachable,
      navColumnWidth: nav ? Math.round(nav.getBoundingClientRect().width) : 0,
      navListFlexDirection: list ? getComputedStyle(list).display : "",
      offscreen,
    };
  });
}

// Widths of the narrow layout (`max-width: 760px`) and the pages that the
// console shows to each role. A page counts as loaded when its heading or button is visible.
const NARROW_WIDTHS = [320, 360, 390, 430, 600, 760];
interface PageUnderTest {
  path: string;
  // Heading of a section page, or button of a detail page.
  heading?: string;
  button?: string;
}
const OWNER_PAGES: PageUnderTest[] = [
  { path: "sandboxes", heading: "Sandboxes" },
  { path: "snapshots", heading: "Snapshots" },
  { path: "devices", heading: "Devices" },
  { path: "members", heading: "Members" },
  { path: "enrollment-tokens", heading: "Enrollment tokens" },
  { path: "api-keys", heading: "API keys" },
  { path: "settings", heading: "Settings" },
  { path: "sandboxes/0193a000-0000-7000-8000-0000000000a1", button: "Pause" },
  { path: "sandboxes/0193a000-0000-7000-8000-0000000000a2", button: "Resume" },
  { path: `devices/${BUILD_BOX}`, button: "Remove device" },
];
const MEMBER_PAGES: PageUnderTest[] = [
  { path: "sandboxes", heading: "Sandboxes" },
  { path: "snapshots", heading: "Snapshots" },
  { path: "devices", heading: "Devices" },
];

async function expectLoaded(
  screen: Screen,
  page: PageUnderTest,
): Promise<void> {
  if (page.heading !== undefined) {
    await expect(
      screen.getByRole("heading", page.heading, { level: 1 }),
    ).toBeVisible();
  } else if (page.button !== undefined) {
    await expect(screen.getByRole("button", page.button)).toBeVisible();
  }
}

describe("narrow screens", () => {
  test("fits every organization page of an owner in 320 to 760 px", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();

    for (const width of NARROW_WIDTHS) {
      await browser.setViewport({ width, height: 844 });
      for (const page of OWNER_PAGES) {
        await app.open(`/orgs/${ACME}/${page.path}`);
        await expectLoaded(screen, page);
        const m = await measureLayout(browser);
        const where = `${width}px ${page.path}`;
        expect(
          m.scrollWidth,
          `${where}: document scrolls sideways`,
        ).toBeLessThanOrEqual(m.clientWidth);
        expect(m.offscreen, `${where}: controls outside the viewport`).toEqual(
          [],
        );
        expect(m.navListLinksReachable, `${where}: section links`).toBe(true);
      }
    }
  });

  test("fits every organization page of a member in 320 to 760 px", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();

    for (const width of NARROW_WIDTHS) {
      await browser.setViewport({ width, height: 844 });
      for (const page of MEMBER_PAGES) {
        await app.open(`/orgs/${NORTHWIND}/${page.path}`);
        await expectLoaded(screen, page);
        const m = await measureLayout(browser);
        const where = `${width}px ${page.path}`;
        expect(
          m.scrollWidth,
          `${where}: document scrolls sideways`,
        ).toBeLessThanOrEqual(m.clientWidth);
        expect(m.offscreen, `${where}: controls outside the viewport`).toEqual(
          [],
        );
        expect(m.navListLinksReachable, `${where}: section links`).toBe(true);
      }
    }
  });

  test("scrolls the section list inside itself at 390 px", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await browser.setViewport({ width: 390, height: 844 });
    await app.open(`/orgs/${ACME}/members`);
    await expect(
      screen.getByRole("heading", "Members", { level: 1 }),
    ).toBeVisible();

    const m = await measureLayout(browser);
    expect(m.navListScrollWidth).toBeGreaterThan(m.navListClientWidth);
    expect(m.navListClientWidth).toBeLessThanOrEqual(m.clientWidth);
  });

  test("keeps the side column and the vertical list from 761 px", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();

    for (const width of [761, 1024]) {
      await browser.setViewport({ width, height: 800 });
      await app.open(`/orgs/${ACME}/members`);
      await expect(
        screen.getByRole("heading", "Members", { level: 1 }),
      ).toBeVisible();
      const m = await measureLayout(browser);
      expect(m.scrollWidth).toBeLessThanOrEqual(m.clientWidth);
      expect(m.navColumnWidth).toBe(220);
      // The list is vertical: it does not scroll sideways.
      expect(m.navListScrollWidth).toBeLessThanOrEqual(m.navListClientWidth);
      expect(m.navListFlexDirection).not.toBe("flex");
    }
  });

  test("fits the organization list at 320 px", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await browser.setViewport({ width: 320, height: 640 });
    await expect(
      screen.getByRole("heading", "Organizations", { level: 1 }),
    ).toBeVisible();
    const m = await measureLayout(browser);
    expect(m.scrollWidth).toBeLessThanOrEqual(m.clientWidth);
  });

  test("fits the invitation page at 320 px", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await browser.setViewport({ width: 320, height: 640 });
    await app.open(`/invitations/accept#secret=${INVITATION_SECRET}`);
    await expect(screen.getByRole("button", "Sign in to accept")).toBeVisible();
    const m = await measureLayout(browser);
    expect(m.scrollWidth).toBeLessThanOrEqual(m.clientWidth);
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
