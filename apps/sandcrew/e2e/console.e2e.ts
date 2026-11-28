// SPDX-License-Identifier: Apache-2.0
import { describe, test, type WebFixtures } from "@e2e-dev/web";
import { expect, type TestFixtures } from "e2e";

// The suite runs the development server with PUBLIC_SANDCREW_MOCK_API=true
// (see e2e.config.ts): an in-browser fake control plane and a fake signed-in
// user. These IDs come from src/mock/mock-api.ts.
const ACME = "0194c000-0000-7000-8000-000000000001";
const NORTHWIND = "0194c000-0000-7000-8000-000000000002";
const GLOBEX = "0194c000-0000-7000-8000-000000000003";
const UNKNOWN_ORG = "0194c000-0000-7000-8000-0000000000ff";
const ACTIVE_SESSION = "0194c200-0000-7000-8000-000000000001";
const PAUSED_SESSION = "0194c200-0000-7000-8000-000000000002";
const ENDED_SESSION = "0194c200-0000-7000-8000-000000000003";
const OTHER_SESSION = "0194c200-0000-7000-8000-000000000005";
const DELETED_SESSION = "0194c200-0000-7000-8000-0000000000ff";

type Fixtures = Pick<TestFixtures & WebFixtures, "app" | "screen">;

/** Signs in with the mock user and opens a path. */
async function signInAndOpen({ app, screen }: Fixtures, path: string) {
  await app.open("/");
  await screen.getByRole("button", "Sign in with Delino").tap();
  await expect(
    screen.getByRole("heading", "Organizations", { level: 1 }),
  ).toBeVisible();
  if (path !== "/") await app.open(path);
}

describe("organization picker", () => {
  test("lists the SandRun organizations and links to the SandRun console", async ({
    app,
    screen,
  }) => {
    await signInAndOpen({ app, screen }, "/");

    await expect(screen.getByRole("link", /Acme Robotics/)).toBeVisible();
    await expect(screen.getByRole("link", /Northwind Labs/)).toBeVisible();
    await expect(screen.getByRole("link", "Account settings")).toHaveAttribute(
      "href",
      "https://accounts.delino.io",
    );
  });

  test("returns to the picker from an organization without access", async ({
    app,
    browser,
    screen,
  }) => {
    await signInAndOpen({ app, screen }, `/orgs/${UNKNOWN_ORG}/sessions`);

    await expect(
      screen.getByText(
        "You no longer have access to that organization. Select another one.",
      ),
    ).toBeVisible();
    await expect(browser).toHaveURL(/\/$/);
  });
});

describe("session list", () => {
  test("shows the states and end reasons of my sessions", async ({
    app,
    screen,
  }) => {
    await signInAndOpen({ app, screen }, `/orgs/${ACME}/sessions`);

    const list = screen.getByRole("list", "Sessions");
    await expect(list).toContainText("Fix the flaky upload test");
    await expect(list).toContainText("Waiting for input");
    await expect(list).toContainText("Ended: The agent exited");
    await expect(list).toContainText("unless it resumes");
  });

  test("shows other members' sessions as metadata", async ({ app, screen }) => {
    await signInAndOpen({ app, screen }, `/orgs/${ACME}/sessions`);
    await screen.getByRole("radio", "All sessions").tap();
    await screen.getByRole("link", "Refactor the billing module").tap();

    await expect(
      screen.getByText(
        "This session belongs to another member. Only its owner reads the chat.",
      ),
    ).toBeVisible();
    await expect(screen.getByRole("region", "Transcript")).toBeHidden();
  });

  test("shows the loading state until the sessions arrive", async ({
    app,
    browser,
    screen,
  }) => {
    await signInAndOpen({ app, screen }, "/");
    await browser.evaluate(() => {
      window.sessionStorage.setItem("sandcrew.mock-response-delay-ms", "1500");
      return null;
    });
    await app.open(`/orgs/${ACME}/sessions`);

    await expect(
      screen.getByRole("status", "Loading the organization"),
    ).toBeVisible();
    await expect(screen.getByRole("status", "Loading sessions")).toBeVisible();
    await expect(screen.getByRole("list", "Sessions")).toContainText(
      "Fix the flaky upload test",
    );
  });

  test("shows the empty state", async ({ app, screen }) => {
    await signInAndOpen({ app, screen }, `/orgs/${NORTHWIND}/sessions`);

    await expect(screen.getByText("You have no sessions yet")).toBeVisible();
  });
});

describe("new-session form", () => {
  test("starts a session from the focused first message and opens its chat", async ({
    app,
    browser,
    screen,
  }) => {
    await signInAndOpen({ app, screen }, `/orgs/${ACME}/sessions/new`);

    await expect(screen.getByLabel("First message")).toBeFocused();
    await browser.keyboard.type("Clone the repository and run the tests");
    await app.screenshot("new-session-form");
    await screen.getByRole("button", "Start session").tap();

    await expect(browser).toHaveURL(
      new RegExp(`/orgs/${ACME}/sessions/[0-9a-f-]{36}$`),
    );
    await expect(
      screen.getByText(
        "I read your message: Clone the repository and run the tests",
      ),
    ).toBeVisible();
  });

  test("shows the SandRun session limit message", async ({
    app,
    browser,
    screen,
  }) => {
    await signInAndOpen({ app, screen }, `/orgs/${GLOBEX}/sessions/new`);
    await expect(screen.getByLabel("First message")).toBeFocused();
    await browser.keyboard.type("Hello");
    await screen.getByRole("button", "Start session").tap();

    await expect(
      screen.getByText(
        "You have reached the session limit of this organization. Delete a session or ask an administrator.",
      ),
    ).toBeVisible();
  });

  test("asks a member to contact an administrator without an allowed profile", async ({
    app,
    screen,
  }) => {
    await signInAndOpen({ app, screen }, `/orgs/${NORTHWIND}/sessions/new`);

    await expect(
      screen.getByText(
        "Ask an owner or admin of the organization to allow a runner profile for SandCrew sessions.",
      ),
    ).toBeVisible();
  });
});

describe("chat view", () => {
  test("answers a permission request and shows the agent's reply", async ({
    app,
    screen,
  }) => {
    await signInAndOpen(
      { app, screen },
      `/orgs/${ACME}/sessions/${ACTIVE_SESSION}`,
    );
    const request = screen.getByRole(
      "article",
      "Permission request for go test ./...",
    );
    await expect(request).toBeVisible();
    await request.getByRole("button", "Allow once").tap();

    await expect(screen.getByText("The command finished.")).toBeVisible();
    await expect(request).toContainText("Answer: Allow once");
    await app.screenshot("chat-view");
  });

  test("sends a message and shows the reply", async ({
    app,
    browser,
    screen,
  }) => {
    await signInAndOpen(
      { app, screen },
      `/orgs/${ACME}/sessions/${ACTIVE_SESSION}`,
    );
    await screen.getByLabel("Message").tap();
    await browser.keyboard.type("What changed?");
    await screen.getByRole("button", "Send").tap();

    await expect(
      screen.getByText("I read your message: What changed?"),
    ).toBeVisible();
  });

  test("switches to auto-approve in the mode menu and closes it with Escape", async ({
    app,
    browser,
    screen,
  }) => {
    await signInAndOpen(
      { app, screen },
      `/orgs/${ACME}/sessions/${ACTIVE_SESSION}`,
    );
    await screen.getByRole("button", "Mode and model").tap();
    const dialog = screen.getByRole("dialog", "Mode and model");
    await dialog.getByRole("combobox", "Permissions").tap();
    await screen.getByRole("option", "Auto-approve").tap();
    await browser.keyboard.press("Escape");

    await expect(dialog).toBeHidden();
    await expect(screen.getByRole("region", "Transcript")).toContainText(
      "Mode: Default · Auto-approve",
    );
  });

  test("resumes a paused session whose resume waited too long", async ({
    app,
    screen,
  }) => {
    await signInAndOpen(
      { app, screen },
      `/orgs/${ACME}/sessions/${PAUSED_SESSION}`,
    );
    const banner = screen.getByRole("status", "Session state");
    await expect(banner).toContainText("Deleted on");
    await banner.getByRole("button", "Resume again").tap();

    await expect(banner).toBeHidden();
  });

  test("starts a new session from the transcript of an ended session", async ({
    app,
    browser,
    screen,
  }) => {
    await signInAndOpen(
      { app, screen },
      `/orgs/${ACME}/sessions/${ENDED_SESSION}`,
    );
    const ended = screen.getByRole("region", "Session ended");
    await expect(ended).toContainText("The agent exited.");
    await ended.getByLabel("First message of the new session").tap();
    await browser.keyboard.type("Continue with the changelog");
    await ended.getByRole("button", "Start new session from transcript").tap();

    await expect(
      screen.getByText("I read your message: Continue with the changelog"),
    ).toBeVisible();
  });

  test("deletes a session after the confirmation", async ({
    app,
    browser,
    screen,
  }) => {
    await signInAndOpen(
      { app, screen },
      `/orgs/${ACME}/sessions/${ACTIVE_SESSION}`,
    );
    await screen.getByRole("button", "Delete session").tap();
    await screen
      .getByRole("alertdialog")
      .getByRole("button", "Delete session")
      .tap();

    await expect(browser).toHaveURL(`/orgs/${ACME}/sessions`);
    await expect(screen.getByRole("list", "Sessions")).not.toContainText(
      "Fix the flaky upload test",
    );
  });

  test("shows the not-found state of a deleted session", async ({
    app,
    screen,
  }) => {
    await signInAndOpen(
      { app, screen },
      `/orgs/${ACME}/sessions/${DELETED_SESSION}`,
    );

    await expect(
      screen.getByText("It was not found, or you no longer have access to it."),
    ).toBeVisible();
  });

  test("fits a 320 px wide screen", async ({ app, browser, screen }) => {
    await browser.setViewport({ width: 320, height: 720 });
    await signInAndOpen(
      { app, screen },
      `/orgs/${ACME}/sessions/${ACTIVE_SESSION}`,
    );
    await expect(screen.getByRole("region", "Transcript")).toBeVisible();
    await app.screenshot("chat-view-320px");

    const overflow = await browser.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth,
    );
    expect(overflow).toBeLessThanOrEqual(0);
  });
});

describe("settings", () => {
  test("saves a credential without showing its value", async ({
    app,
    browser,
    screen,
  }) => {
    await signInAndOpen({ app, screen }, `/orgs/${ACME}/settings`);
    await screen.getByRole("button", "Set the Claude Code credential").tap();
    await expect(screen.getByLabel("API key")).toBeFocused();
    await browser.keyboard.type("sk-ant-e2e-secret");
    await screen.getByRole("button", "Save credential").tap();

    const list = screen.getByRole("list", "Your vendor credentials");
    await expect(list).toContainText("API key · set");
    expect(
      await browser.evaluate(() => ({
        page: document.body.innerText.includes("sk-ant-e2e-secret"),
        storage: JSON.stringify({
          ...window.localStorage,
          ...window.sessionStorage,
        }).includes("sk-ant-e2e-secret"),
      })),
    ).toEqual({ page: false, storage: false });
  });

  test("adds a session secret", async ({ app, browser, screen }) => {
    await signInAndOpen({ app, screen }, `/orgs/${ACME}/settings`);
    await screen.getByRole("button", "Add secret").tap();
    await expect(screen.getByLabel("Name")).toBeFocused();
    await browser.keyboard.type("NPM_TOKEN");
    await screen.getByLabel("Value").tap();
    await browser.keyboard.type("npm-e2e-secret");
    await screen.getByRole("button", "Save secret").tap();

    await expect(screen.getByRole("list", "Session secrets")).toContainText(
      "NPM_TOKEN",
    );
  });

  test("shows how to allow notifications that the browser blocks", async ({
    app,
    screen,
  }) => {
    // Headless Chromium blocks notifications unless a test grants them.
    await signInAndOpen({ app, screen }, `/orgs/${ACME}/settings`);

    await expect(
      screen.getByText(
        "Notifications are blocked for this site. Allow them in the browser settings to turn them on.",
      ),
    ).toBeVisible();
    await expect(
      screen.getByRole("button", "Turn on notifications on this browser"),
    ).toBeHidden();
  });

  test("turns on notifications only after the click and registers the service worker", async ({
    app,
    browser,
    screen,
  }) => {
    await signInAndOpen({ app, screen }, `/orgs/${ACME}/settings`);
    // Replace only the permission prompt and the push service, which a
    // headless browser cannot provide. The service worker registration is
    // real.
    await browser.evaluate(() => {
      const state = window as unknown as {
        e2ePermission: NotificationPermission;
        e2ePermissionRequests: number;
      };
      state.e2ePermission = "default";
      state.e2ePermissionRequests = 0;
      Object.defineProperty(Notification, "permission", {
        configurable: true,
        get: () => state.e2ePermission,
      });
      Notification.requestPermission = async () => {
        state.e2ePermissionRequests++;
        state.e2ePermission = "granted";
        return "granted";
      };
      PushManager.prototype.subscribe = async () =>
        ({
          endpoint: "https://push.example.com/send/e2e",
          getKey: (name: string) =>
            new Uint8Array(name === "auth" ? 16 : 65).buffer,
        }) as unknown as PushSubscription;
      return null;
    });
    // Open the settings again without a page load, so they read the stubs.
    await screen.getByRole("link", "Sessions").tap();
    await screen.getByRole("link", "Settings").tap();
    const turnOn = screen.getByRole(
      "button",
      "Turn on notifications on this browser",
    );
    await expect(turnOn).toBeVisible();
    expect(
      await browser.evaluate(
        () =>
          (window as unknown as { e2ePermissionRequests: number })
            .e2ePermissionRequests,
      ),
    ).toBe(0);

    await turnOn.tap();

    await expect(
      screen.getByRole("list", "Notification subscriptions"),
    ).toContainText("https://push.example.com");
    expect(
      await browser.evaluate(async () => {
        const registration = await navigator.serviceWorker.getRegistration("/");
        return {
          requests: (window as unknown as { e2ePermissionRequests: number })
            .e2ePermissionRequests,
          script: new URL(
            (
              registration?.installing ??
              registration?.waiting ??
              registration?.active
            )?.scriptURL ?? "about:blank",
          ).pathname,
        };
      }),
    ).toEqual({ requests: 1, script: "/sw.js" });
  });

  test("shows members that only owners and admins change organization settings", async ({
    app,
    screen,
  }) => {
    await signInAndOpen(
      { app, screen },
      `/orgs/${NORTHWIND}/settings/organization`,
    );

    await expect(
      screen.getByText(
        "Only owners and admins of the organization change its SandCrew settings.",
      ),
    ).toBeVisible();
  });

  test("saves the organization settings", async ({ app, screen }) => {
    await signInAndOpen({ app, screen }, `/orgs/${ACME}/settings/organization`);
    await expect(
      screen.getByText(/does not support this guest OS yet/),
    ).toBeVisible();
    await screen.getByRole("combobox", "Paused-session retention").tap();
    await screen.getByRole("option", "Unlimited").tap();
    await screen.getByRole("button", "Save settings").tap();

    await expect(screen.getByText("Settings saved")).toBeVisible();
    await app.screenshot("organization-settings");
  });
});

describe("session permissions", () => {
  test("shows a session that another member opens as metadata only", async ({
    app,
    screen,
  }) => {
    await signInAndOpen(
      { app, screen },
      `/orgs/${ACME}/sessions/${OTHER_SESSION}`,
    );

    await expect(screen.getByRole("button", "Delete session")).toBeHidden();
  });
});
