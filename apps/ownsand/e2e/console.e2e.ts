// SPDX-License-Identifier: Apache-2.0
import { Code } from "@connectrpc/connect";
import { describe, test, type Browser } from "@e2e-dev/web";
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

type ResourceWrite = {
  resources?: { vcpuCount: number; memoryMib: string; diskGib: string };
  limits?: { vcpuCount: number };
};

function trackResourceMutation(
  browser: Browser,
  method: "CreateSandbox" | "ReplaceDeviceLimits",
) {
  return browser.evaluate((method) => {
    sessionStorage.setItem("ownsand-e2e-tracked-mutation", method);
    return true;
  }, method);
}

function resourceWrites(browser: Browser) {
  return browser.evaluate(() => {
    const calls = sessionStorage.getItem(
      "ownsand-e2e-resource-mutation-inputs",
    );
    return (JSON.parse(calls ?? "[]") as string[]).map(
      (call) => JSON.parse(call) as ResourceWrite,
    );
  });
}

async function failOnWindowFocus({ key, code }: { key: string; code: Code }) {
  sessionStorage.setItem(key, String(code));
  const now = Date.now;
  // Treat the queries as stale when React Query handles the visibility event.
  Date.now = () => now() + 11_000;
  try {
    window.dispatchEvent(new Event("visibilitychange"));
    // React Query checks staleness after resuming paused mutations.
    await Promise.resolve();
  } finally {
    Date.now = now;
  }
  return true;
}

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

  test("returns focus to Create organization after Escape closes the dialog", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await screen.getByRole("button", "Create organization").tap();
    await expect(screen.getByLabel("Organization name")).toBeFocused();

    await browser.keyboard.press("Escape");

    await expect(screen.getByLabel("Organization name")).toBeHidden();
    await expect(
      screen.getByRole("button", "Create organization"),
    ).toBeFocused();
  });
});

describe("organization list errors", () => {
  const refreshNotice = /The last refresh failed/;
  const listName = "Your organizations";

  test("keeps loaded rows while retrying a failed refresh", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    const acme = screen.getByRole("link", "Acme Robotics Owner");
    const northwind = screen.getByRole("link", "Northwind Labs Member");
    await expect(northwind).toBeVisible();

    await browser.evaluate(failOnWindowFocus, {
      key: "ownsand-e2e-organization-list-error",
      code: Code.Unavailable,
    });

    const notice = screen.getByText(refreshNotice);
    await expect(notice).toBeVisible();
    await expect(acme).toBeVisible();
    await expect(northwind).toBeVisible();
    await app.screenshot("organization-refresh-failed");

    await browser.evaluate(() => {
      sessionStorage.setItem("ownsand-e2e-organization-list-error", "hold");
      return true;
    });
    const retry = screen.getByRole("button", "Try again");
    await retry.tap();
    await expect(retry).toHaveAttribute("disabled", "");
    await expect(acme).toBeVisible();
    await expect(northwind).toBeVisible();
    await app.screenshot("organization-refresh-retrying");

    await browser.evaluate(() => {
      sessionStorage.removeItem("ownsand-e2e-organization-list-error");
      window.dispatchEvent(new Event("ownsand-e2e-release-organization-list"));
      return true;
    });
    await expect(notice).toBeHidden();
    await expect(acme).toBeVisible();
    await expect(northwind).toBeVisible();
    await app.screenshot("organization-refresh-recovered");
  });

  test("replaces the list when the session ends during a refresh", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    const list = screen.getByRole("list", listName);
    await expect(list).toBeVisible();

    await browser.evaluate(failOnWindowFocus, {
      key: "ownsand-e2e-organization-list-error",
      code: Code.Unauthenticated,
    });

    await expect(
      screen.getByText("Your session ended. Sign in again."),
    ).toBeVisible();
    await expect(
      screen.getByRole("button", "Sign in with Delino"),
    ).toBeVisible();
    await expect(list).toBeHidden();
    await expect(screen.getByText(refreshNotice)).toBeHidden();
    await app.screenshot("organization-refresh-ended-session");
  });

  test("shows a full error when the first organization request fails", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");
    await browser.evaluate((code) => {
      sessionStorage.setItem(
        "ownsand-e2e-organization-list-error",
        String(code),
      );
      return true;
    }, Code.Unavailable);
    await screen.getByRole("button", "Sign in with Delino").tap();

    await expect(
      screen.getByText(
        "OwnSand is not available right now. Try again in a moment.",
      ),
    ).toBeVisible();
    await expect(screen.getByRole("button", "Try again")).toBeVisible();
    await expect(screen.getByRole("list", listName)).toBeHidden();
    await expect(screen.getByText(refreshNotice)).toBeHidden();
    await app.screenshot("organization-first-load-failed");
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

    await screen.getByRole("checkbox", "Show ended sandboxes").tap();
    await expect(screen.getByText("Deleted")).toBeHidden();
    await expect(screen.getByText("Paused")).toBeVisible();
    await expect(screen.getByRole("button", "Show more")).toBeHidden();
  });

  test("shows a field error when the sandbox disk exceeds 8589934591 GiB", async ({
    app,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/sandboxes`);
    await screen.getByRole("button", "Create sandbox").tap();

    const dialog = screen.getByRole("dialog", "Create a sandbox");
    const disk = dialog.getByLabel("Disk (GiB)");
    await disk.fill("8589934592");
    await dialog.getByRole("button", "Create").tap();

    await expect(dialog).toBeVisible();
    await expect(disk).toHaveAttribute("aria-invalid", "true");
    await expect(
      dialog.getByText("Enter a whole number of GiB from 1 to 8589934591."),
    ).toBeVisible();
  });

  for (const [field, name, fraction, correction, expected, message] of [
    [
      "vCPUs",
      "vcpuCount",
      "2.5",
      "3",
      3,
      "Enter a whole number of vCPUs from 1 to 2147483647.",
    ],
    [
      "Memory (MiB)",
      "memoryMib",
      "1000.5",
      "1000",
      "1000",
      "Enter a whole number of MiB from 1 to 9007199254740991.",
    ],
    [
      "Disk (GiB)",
      "diskGib",
      "20.5",
      "21",
      "21",
      "Enter a whole number of GiB from 1 to 8589934591.",
    ],
  ] as const) {
    test(`rejects fractional sandbox ${field} on Enter and after blur`, async ({
      app,
      screen,
      browser,
    }) => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await app.open(`/orgs/${ACME}/sandboxes`);
      await screen.getByRole("button", "Create sandbox").tap();

      const dialog = screen.getByRole("dialog", "Create a sandbox");
      const input = dialog.getByLabel(field);
      await trackResourceMutation(browser, "CreateSandbox");
      await input.fill(fraction);
      await browser.keyboard.press("Enter");
      await expect(input).toHaveAttribute("aria-invalid", "true");
      await expect(dialog.getByText(message)).toBeVisible();
      expect(await resourceWrites(browser)).toHaveLength(0);

      const secondFraction = `${fraction}5`;
      await input.fill(secondFraction);
      await browser.keyboard.press("Tab");
      await dialog.getByRole("button", "Create").tap();
      await expect(dialog).toBeVisible();
      await expect(input).toHaveValue(secondFraction);
      await expect(dialog.getByText(message)).toBeVisible();
      expect(await resourceWrites(browser)).toHaveLength(0);
      await app.screenshot(`sandbox-fraction-${name}`);

      await input.fill(correction);
      if (name === "memoryMib") {
        await browser.keyboard.press("Tab");
        await expect(input).toHaveValue("1,000");
        await input.tap();
        await browser.keyboard.press("ArrowUp");
        await expect(input).toHaveValue("1,001");
        await input.fill("1000");
        await browser.keyboard.press("Shift+ArrowUp");
        await expect(input).toHaveValue("1,256");
        await input.fill(correction);
      }
      await dialog.getByRole("button", "Create").tap();
      await expect(dialog).toBeHidden();
      await expect(browser).toHaveURL(/\/sandboxes\/[0-9a-f-]+$/);
      const writes = await resourceWrites(browser);
      expect(writes).toHaveLength(1);
      expect(writes[0]?.resources?.[name]).toBe(expected);
    });
  }

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

    // The focus starts on Copy, so Enter copies and keeps the dialog open;
    // one Tab reaches Done.
    await expect(screen.getByRole("button", "Copy")).toBeFocused();
    await browser.keyboard.press("Enter");
    await expect(dialog).toBeVisible();
    await browser.keyboard.press("Tab");
    await expect(screen.getByRole("button", "Done")).toBeFocused();

    await browser.keyboard.press("Escape");
    await expect(dialog).toBeHidden();
    await expect(screen.getByText("nightly-ci · Active")).toBeVisible();
    await expect(screen.getByRole("button", "Create API key")).toBeFocused();
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

  test("rejects a fractional device vCPU limit on Enter and after blur", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/devices/${BUILD_BOX}`);

    const input = screen.getByLabel("vCPU limit");
    const message = "vCPU limit must be a whole number from 0 to 2147483647.";
    await trackResourceMutation(browser, "ReplaceDeviceLimits");
    await input.fill("2.5");
    await browser.keyboard.press("Enter");
    await expect(input).toHaveAttribute("aria-invalid", "true");
    await expect(screen.getByText(message)).toHaveCount(2);
    expect(await resourceWrites(browser)).toHaveLength(0);

    await input.fill("0.5");
    await browser.keyboard.press("Tab");
    await screen.getByRole("button", "Save limits").tap();
    await expect(input).toHaveValue("0.5");
    await expect(input).toHaveAttribute("aria-invalid", "true");
    expect(await resourceWrites(browser)).toHaveLength(0);
    await app.screenshot("device-fraction-field-and-toast");

    await input.fill("0");
    await screen.getByRole("button", "Save limits").tap();
    await expect(
      screen.getByText("The device limits are saved."),
    ).toBeVisible();
    const writes = await resourceWrites(browser);
    expect(writes).toHaveLength(1);
    expect(writes[0]?.limits?.vcpuCount).toBe(0);
    await expect(input).toHaveValue("0");
  });

  test("shows saved device limits when returning to the device page", async ({
    app,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/devices/${BUILD_BOX}`);

    const disk = screen.getByLabel("Disk limit (GiB)");
    await expect(disk).toHaveValue("500");
    await disk.fill("600");
    await screen.getByRole("button", "Save limits").tap();
    await expect(
      screen
        .getByRole("region", "Notifications")
        .getByText("The device limits are saved."),
    ).toBeVisible();

    await screen.getByRole("link", "Devices", { exact: true }).tap();
    await expect(screen.getByText("80 of 600 GiB")).toBeVisible();
    await screen.getByRole("link", "build-box-1").tap();
    await expect(disk).toHaveValue("600");
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

    await screen.getByRole("link", "Sandboxes", { exact: true }).tap();
    const sandbox = screen.getByRole("link", "0193a000…00a1");
    await expect(sandbox).toBeHidden();
    await screen.getByRole("checkbox", "Show ended sandboxes").tap();
    await sandbox.tap();
    await expect(screen.getByText("Deleted", { exact: true })).toBeVisible();
    await expect(
      screen.getByText("Device removed", { exact: true }),
    ).toBeVisible();
  });

  test("asks before it revokes a pending enrollment token", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/enrollment-tokens`);
    await screen.getByRole("button", "Create token").tap();
    await screen.getByRole("dialog").getByRole("button", "Create token").tap();
    await screen.getByRole("button", "Done").tap();
    await expect(
      screen.getByRole("dialog", "Your new enrollment token"),
    ).toBeHidden();
    await expect(screen.getByText("Not used", { exact: false })).toBeVisible();

    await screen
      .getByRole("button", "Revoke enrollment token ose_", { exact: false })
      .tap();
    const dialog = screen.getByRole("alertdialog", "Revoke token ose_", {
      exact: false,
    });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByRole("button", "Cancel")).toBeFocused();
    await expect(screen.getByText("Not used", { exact: false })).toBeVisible();

    await browser.keyboard.press("Escape");
    await expect(dialog).toBeHidden();
    await expect(screen.getByText("Not used", { exact: false })).toBeVisible();

    await screen
      .getByRole("button", "Revoke enrollment token ose_", { exact: false })
      .tap();
    await screen
      .getByRole("alertdialog", "Revoke token ose_", { exact: false })
      .getByRole("button", "Revoke token")
      .tap();
    await expect(screen.getByText("The token is revoked.")).toBeVisible();
    await expect(screen.getByText("Not used", { exact: false })).toBeHidden();
  });

  test("moves the focus to the page heading after a confirmed removal", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/members`);
    const heading = screen.getByRole("heading", "Members", { level: 1 });
    await expect(heading).toHaveAttribute("tabindex", "-1");

    // Escape keeps the focus on the opener.
    const remove = screen.getByRole("button", "Remove Jun Park");
    await remove.focus();
    await browser.keyboard.press("Enter");
    await expect(screen.getByRole("alertdialog")).toBeVisible();
    await browser.keyboard.press("Escape");
    await expect(remove).toBeFocused();

    // A confirmed removal deletes the opener, so the heading gets the focus.
    await browser.keyboard.press("Enter");
    await screen
      .getByRole("alertdialog")
      .getByRole("button", "Remove member")
      .tap();
    await expect(screen.getByText("Jun Park")).toBeHidden();
    await expect(heading).toBeFocused();
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

  test("lets a member leave from the Members page", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(
      screen.getByRole("link", "Northwind Labs Member"),
    ).toBeVisible();
    await app.open(`/orgs/${NORTHWIND}/members`);

    await screen.getByRole("button", "Leave organization").tap();
    await screen
      .getByRole("alertdialog", "Leave this organization?")
      .getByRole("button", "Leave organization")
      .tap();

    await expect(browser).toHaveURL("/");
    await expect(
      screen.getByRole("heading", "Organizations", { level: 1 }),
    ).toBeVisible();
    await expect(
      screen.getByRole("link", "Northwind Labs Member"),
    ).toBeHidden();
  });
});

describe("billing", () => {
  const billingListErrorKey = "ownsand-e2e-billing-list-error";
  const acmeBillingId = "0193a000-0000-7000-8000-0000000000e1";
  const labBillingId = "0193a000-0000-7000-8000-0000000000e2";

  function billingLinkRequests() {
    return JSON.parse(
      sessionStorage.getItem("ownsand-e2e-billing-link-requests") ?? "[]",
    ) as string[];
  }

  function submitBillingForm() {
    document
      .querySelector<HTMLFormElement>('[role="dialog"] form')!
      .requestSubmit();
    return true;
  }

  async function openBillingLinkDialog(screen: Screen) {
    await screen.getByRole("button", "Unlink billing account").tap();
    const confirm = screen.getByRole("alertdialog");
    await confirm.getByRole("button", "Unlink billing account").tap();
    await expect(confirm).toBeHidden();
    await screen.getByRole("button", "Link a billing account").tap();
    return screen.getByRole("dialog", "Link a billing account");
  }

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

  for (const keepAccount of [true, false]) {
    test(`recovers from a terminal billing refresh with the chosen account ${keepAccount ? "present" : "removed"}`, async ({
      app,
      screen,
      browser,
    }) => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await app.open(`/orgs/${ACME}/settings`);
      const dialog = await openBillingLinkDialog(screen);
      const link = dialog.getByRole("button", "Link");
      const acme = dialog.getByRole("radio", "Acme Robotics billing · Owner");
      const lab = dialog.getByRole("radio", "Research lab credits · Admin");
      await acme.tap();
      await expect(acme).toHaveAttribute("aria-checked", "true");

      await browser.evaluate(failOnWindowFocus, {
        key: billingListErrorKey,
        code: Code.PermissionDenied,
      });
      await expect(dialog.getByRole("alert")).toContainText(
        "You do not have permission to do this.",
      );
      await expect(acme).toBeHidden();
      await expect(lab).toBeHidden();
      await expect(link).toHaveAttribute("disabled", "");
      await browser.evaluate(submitBillingForm);
      expect(await browser.evaluate(billingLinkRequests)).toEqual([]);
      await app.screenshot(
        `billing-terminal-refresh-${keepAccount ? "keep" : "remove"}`,
      );

      await browser.evaluate(
        ({ key, keepAccount }) => {
          sessionStorage.removeItem(key);
          if (!keepAccount) {
            sessionStorage.setItem(
              "ownsand-e2e-billing-list-remove-first",
              "true",
            );
          }
          return true;
        },
        { key: billingListErrorKey, keepAccount },
      );
      await dialog.getByRole("button", "Try again").tap();
      await expect(lab).toBeVisible();
      await expect(dialog.getByRole("alert")).toBeHidden();
      if (keepAccount) {
        await expect(acme).toHaveAttribute("aria-checked", "true");
      } else {
        await expect(acme).toBeHidden();
        await expect(lab).toHaveAttribute("aria-checked", "false");
        await browser.evaluate(submitBillingForm);
        await expect(
          dialog.getByText("Choose a billing account."),
        ).toBeVisible();
        expect(await browser.evaluate(billingLinkRequests)).toEqual([]);
      }
      await app.screenshot(
        `billing-refresh-recovered-${keepAccount ? "keep" : "remove"}`,
      );
      if (!keepAccount) await lab.tap();
      await link.tap();
      await expect(dialog).toBeHidden();
      expect(await browser.evaluate(billingLinkRequests)).toEqual([
        keepAccount ? acmeBillingId : labBillingId,
      ]);
    });
  }

  test("links the visible choice after a transient billing refresh error", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/settings`);
    const dialog = await openBillingLinkDialog(screen);
    const acme = dialog.getByRole("radio", "Acme Robotics billing · Owner");
    await acme.tap();

    await browser.evaluate(failOnWindowFocus, {
      key: billingListErrorKey,
      code: Code.Internal,
    });
    await expect(dialog.getByRole("alert")).toContainText(
      "The last refresh failed",
    );
    await expect(acme).toHaveAttribute("aria-checked", "true");
    await app.screenshot("billing-transient-refresh");
    await dialog.getByRole("button", "Link").tap();
    await expect(dialog).toBeHidden();
    expect(await browser.evaluate(billingLinkRequests)).toEqual([
      acmeBillingId,
    ]);
  });
});

describe("agent version policy", () => {
  test("leaves the saved version in place when the admin leaves an unsaved edit", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/settings`);

    const pin = screen.getByRole("radio", "Pin a version");
    const version = screen.getByRole("textbox", "Pinned version");
    await pin.tap();
    await version.tap();
    await browser.keyboard.type("1.4.2");
    await screen.getByRole("button", "Save policy").tap();
    await expect(screen.getByRole("region", "Notifications")).toContainText(
      "The agent version policy is saved.",
    );

    await screen.getByRole("radio", "Hold updates").tap();
    await expect(version).toBeHidden();
    await screen.getByRole("link", "Devices", { exact: true }).tap();
    await screen.getByRole("link", "Settings", { exact: true }).tap();

    await expect(pin).toHaveAttribute("aria-checked", "true");
    await expect(version).toHaveValue("1.4.2");
  });
});

const heldInvitation = test.extend<{ holdInvitation(): Promise<void> }>({
  holdInvitation: async ({ app, browser, screen }, use) => {
    await use(async () => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await expect(
        screen.getByRole("link", "Acme Robotics Owner"),
      ).toBeVisible();
      await browser.evaluate(() => {
        sessionStorage.setItem("ownsand-e2e-acceptance", "hold");
        return null;
      });
      await app.open(`/invitations/accept#secret=${ACME_INVITATION_SECRET}`);
      const accept = screen.getByRole("button", "Accept invitation");
      await accept.tap();
      await expect(accept).toHaveAttribute("data-pending", "");
      expect(
        await browser.evaluate(() =>
          sessionStorage.getItem("ownsand-e2e-acceptance"),
        ),
      ).toEqual("held");
    });
  },
});

describe("invitation acceptance while opening another link", () => {
  for (const eventFirst of [false, true]) {
    heldInvitation(
      eventFirst
        ? "accepts the newer link when hashchange ran before the earlier response"
        : "accepts the newer link when the earlier response arrives before hashchange",
      async ({ app, browser, screen, holdInvitation }) => {
        await holdInvitation();
        await browser.evaluate(
          ({ secret, deliverEvent }) => {
            window.history.replaceState(null, "", `#secret=${secret}`);
            if (deliverEvent)
              window.dispatchEvent(new HashChangeEvent("hashchange"));
            window.dispatchEvent(new Event("ownsand-e2e-release-acceptance"));
            return null;
          },
          { secret: INVITATION_SECRET, deliverEvent: eventFirst },
        );

        const accept = screen.getByRole("button", "Accept invitation");
        await expect(accept).toBeEnabled();
        if (!eventFirst) {
          expect(
            await browser.evaluate(
              (secret) =>
                window.location.pathname === "/invitations/accept" &&
                window.location.hash === `#secret=${secret}`,
              INVITATION_SECRET,
            ),
          ).toEqual(true);
          await app.screenshot("new-invitation-before-hashchange");
          await browser.evaluate(() => {
            window.dispatchEvent(new HashChangeEvent("hashchange"));
            return null;
          });
        }
        await expect(browser).toHaveURL("/invitations/accept");
        await accept.tap();
        await expect(browser).toHaveURL(`/orgs/${CONTOSO}/sandboxes`);
        await expect(
          screen.getByRole("heading", "Sandboxes", { level: 1 }),
        ).toBeVisible();
        await app.screenshot(
          eventFirst ? "new-invitation-event-first" : "new-invitation-accepted",
        );
      },
    );
  }

  heldInvitation(
    "redirects when the fragment still identifies the held invitation",
    async ({ app, browser, screen, holdInvitation }) => {
      await holdInvitation();
      await browser.evaluate((secret) => {
        window.history.replaceState(null, "", `#secret=%20${secret}%20`);
        window.dispatchEvent(new Event("ownsand-e2e-release-acceptance"));
        return null;
      }, ACME_INVITATION_SECRET);

      await expect(browser).toHaveURL(`/orgs/${ACME}/sandboxes`);
      await expect(
        screen.getByRole("heading", "Sandboxes", { level: 1 }),
      ).toBeVisible();
      await app.screenshot("current-invitation-accepted");
    },
  );
});

describe("invitations", () => {
  test("hides the seeded pending invitation from Members at expiry", async ({
    app,
    screen,
    browser,
  }) => {
    const start = Date.UTC(2026, 0, 1, 12);
    const expiry = start + 6 * 24 * 60 * 60 * 1000;
    await browser.addInitScript((ms) => {
      Date.now = () => ms;
    }, start);
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/members`);

    const invitations = screen.getByRole("region", "Invitations");
    const revoke = invitations.getByRole("button", /^Revoke/);
    await expect(revoke).toBeVisible();
    await app.screenshot("invitation-before-expiry");

    await screen.getByRole("link", "Devices", { exact: true }).tap();
    await browser.evaluate((ms) => {
      Date.now = () => ms;
      return null;
    }, expiry);
    await screen.getByRole("link", "Members", { exact: true }).tap();

    await expect(invitations.getByText("No pending invitations")).toBeVisible();
    await expect(revoke).toBeHidden();
    await app.screenshot("invitation-at-expiry");
  });

  test("rejects a confirmation submitted when the invitation expires", async ({
    app,
    screen,
    browser,
  }) => {
    const start = Date.UTC(2026, 0, 1, 12);
    const expiry = start + 6 * 24 * 60 * 60 * 1000;
    await browser.addInitScript((ms) => {
      Date.now = () => ms;
    }, start);
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/members`);

    const invitations = screen.getByRole("region", "Invitations");
    await invitations.getByRole("button", /^Revoke/).tap();
    const confirmation = screen.getByRole("alertdialog");
    await expect(confirmation).toBeVisible();
    await browser.evaluate((ms) => {
      Date.now = () => ms;
      return null;
    }, expiry);
    await confirmation.getByRole("button", "Revoke invitation").tap();

    await expect(
      confirmation.getByText("only a pending invitation can be revoked", {
        exact: false,
      }),
    ).toBeVisible();
    await expect(screen.getByText("The invitation is revoked.")).toHaveCount(0);
    await app.screenshot("invitation-revocation-at-expiry");
  });

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

  for (const response of ["missing-organization", "empty-id"]) {
    test(`offers Organizations after accepting an invitation with ${response}`, async ({
      app,
      screen,
      browser,
    }) => {
      await app.open(`/invitations/accept#secret=${INVITATION_SECRET}`);
      await screen.getByRole("button", "Sign in to accept").tap();
      await browser.evaluate((mode) => {
        sessionStorage.setItem("ownsand-e2e-accept-response", mode);
        return null;
      }, response);
      expect(
        await browser.evaluate(() =>
          sessionStorage.getItem("ownsand.pending-invitation"),
        ),
      ).toEqual(INVITATION_SECRET);

      const accept = screen.getByRole("button", "Accept invitation");
      await accept.tap();

      await expect(
        screen.getByText("You may have joined the organization."),
      ).toBeVisible();
      await expect(accept).toBeHidden();
      await expect(browser).toHaveURL("/invitations/accept");
      expect(
        await browser.evaluate(() =>
          sessionStorage.getItem("ownsand.pending-invitation"),
        ),
      ).toBeNull();
      await app.screenshot(`invitation-recovery-${response}`);

      await screen.getByRole("link", "Go to your organizations").tap();
      await expect(browser).toHaveURL("/");
      await expect(screen.getByRole("link", /Contoso Research/)).toBeVisible();
      expect(
        await browser.evaluate(() =>
          sessionStorage.getItem("ownsand-e2e-accept-count"),
        ),
      ).toEqual("1");
    });
  }

  test("leaves a newer invitation available after an earlier incomplete success", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open(`/invitations/accept#secret=${INVITATION_SECRET}`);
    await screen.getByRole("button", "Sign in to accept").tap();
    await browser.evaluate(() => {
      sessionStorage.setItem("ownsand-e2e-accept-response", "hold");
      return null;
    });
    const accept = screen.getByRole("button", "Accept invitation");
    await accept.tap();
    await expect(accept).toBeDisabled();
    expect(
      await browser.evaluate(() =>
        sessionStorage.getItem("ownsand-e2e-accept-held"),
      ),
    ).toEqual("true");

    await browser.evaluate((secret) => {
      window.location.hash = `secret=${secret}`;
      return null;
    }, ACME_INVITATION_SECRET);
    await expect(accept).toBeEnabled();
    await browser.evaluate(async (secret) => {
      sessionStorage.setItem("ownsand.pending-invitation", secret);
      window.dispatchEvent(new Event("ownsand-e2e-release-accept"));
      await new Promise<void>((resolve) => setTimeout(resolve, 0));
      return null;
    }, ACME_INVITATION_SECRET);

    await expect(accept).toBeEnabled();
    await expect(
      screen.getByText("You may have joined the organization."),
    ).toBeHidden();
    expect(
      await browser.evaluate(() =>
        sessionStorage.getItem("ownsand.pending-invitation"),
      ),
    ).toEqual(ACME_INVITATION_SECRET);
    await app.screenshot("invitation-newer-after-incomplete-success");
    await accept.tap();
    await expect(screen.getByText("you are already a member")).toBeVisible();
  });

  test("accepts a different link opened on the same page after an invitation error", async ({
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

    await browser.evaluate((secret) => {
      window.location.hash = `secret=${secret}`;
      return true;
    }, INVITATION_SECRET);
    await expect(screen.getByText("you are already a member")).toBeHidden();
    await expect(browser).toHaveURL("/invitations/accept");
    await screen.getByRole("button", "Accept invitation").tap();

    await expect(browser).toHaveURL(`/orgs/${CONTOSO}/sandboxes`);
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

describe("organization switcher popup width", () => {
  for (const width of [390, 240]) {
    test(`fits a ${width}px trigger within the popup width limit`, async ({
      app,
      screen,
      browser,
    }) => {
      const viewportWidth = 390;
      const popupWidthLimit = viewportWidth - 16;
      await browser.setViewport({ width: viewportWidth, height: 844 });
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await app.open(`/orgs/${ACME}/sandboxes`);

      const trigger = screen.getByRole("combobox", "Organization");
      await expect(trigger).toBeVisible();
      const triggerWidth = await browser.evaluate((width) => {
        const element = document.querySelector<HTMLElement>(
          '.org-switcher [role="combobox"]',
        );
        if (!element) throw new Error("Organization trigger is missing");
        element.style.width = `${width}px`;
        element.style.minWidth = `${width}px`;
        return element.offsetWidth;
      }, width);
      expect(triggerWidth).toBe(width);

      await trigger.tap();
      await expect(screen.getByRole("listbox")).toBeVisible();
      await expect(screen.getByRole("option", "Northwind Labs")).toBeVisible();
      const popupWidth = await browser.evaluate(() => {
        const popup = document.querySelector<HTMLElement>(
          ".dui-select-content",
        );
        if (!popup) throw new Error("Organization popup is missing");
        // offsetWidth measures the border box during the opening animation.
        return popup.offsetWidth;
      });
      await app.screenshot(`organization-popup-trigger-${width}`);
      expect(popupWidth).toBeLessThanOrEqual(popupWidthLimit);
      expect(popupWidth).toBeGreaterThanOrEqual(
        Math.min(triggerWidth, popupWidthLimit),
      );
    });
  }
});

// A name that is one 100-character word, the longest the console accepts. It
// has no space to break at, so it must wrap inside its container.
const LONG_NAME_PREFIX = "LONGNAME_";
const LONG_NAME = LONG_NAME_PREFIX + "a".repeat(100 - LONG_NAME_PREFIX.length);

describe("long names", () => {
  test("wraps the title of a dialog instead of widening the panel", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open(`/orgs/${ACME}/snapshots`);
    // The row button is named by its row ("Make template from snapshot ..."),
    // so match the start of the name.
    await screen
      .getByRole("button", "Make template", { exact: false })
      .first()
      .tap();
    // The template name field takes the initial focus.
    await expect(screen.getByLabel("Template name")).toBeFocused();
    await browser.keyboard.type(LONG_NAME);
    await screen.getByRole("button", "Create template").tap();
    // Wait for the form dialog to finish closing, because it keeps the page
    // inert until then.
    await expect(screen.getByRole("dialog", "Make a template")).toBeHidden();
    await expect(
      screen.getByText(LONG_NAME, { exact: false }).first(),
    ).toBeVisible();

    for (const width of [1280, 390]) {
      await browser.setViewport({ width, height: 800 });
      // The snapshot row contains the template rows, so it matches too. The
      // template row is the last match in document order.
      await screen
        .getByRole("listitem")
        .filter({ hasText: LONG_NAME })
        .last()
        .getByRole("button", "Delete template", { exact: false })
        .tap();
      const dialog = screen.getByRole(
        "alertdialog",
        `Delete the template ${LONG_NAME}?`,
      );
      await expect(dialog).toBeVisible();
      const m = await measureDialog(browser);
      expect(m.panelScrollWidth).toBeLessThanOrEqual(m.panelClientWidth);
      expect(m.titleScrollWidth).toBeLessThanOrEqual(m.titleClientWidth);
      // The title wraps onto several lines.
      expect(m.titleHeight).toBeGreaterThan(m.titleLineHeight * 2);
      await browser.keyboard.press("Escape");
      await expect(dialog).toBeHidden();
    }
  });

  test("keeps a short dialog title on one line", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await browser.setViewport({ width: 390, height: 800 });
    // The snapshot dialog title now carries the snapshot short ID and wraps at
    // this width, so the device dialog stands for a short title.
    await app.open(`/orgs/${ACME}/devices/${BUILD_BOX}`);
    await screen.getByRole("button", "Remove device").tap();
    await expect(
      screen.getByRole("alertdialog", "Remove build-box-1?"),
    ).toBeVisible();
    const m = await measureDialog(browser);
    expect(m.titleHeight).toBeLessThanOrEqual(m.titleLineHeight * 1.5);
  });

  test("keeps the organization list inside the viewport", async ({
    app,
    screen,
    browser,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    for (const width of [390, 320]) {
      await app.open("/");
      await browser.setViewport({ width, height: 844 });
      await screen.getByRole("button", "Create organization").tap();
      await expect(screen.getByLabel("Organization name")).toBeFocused();
      await browser.keyboard.type(LONG_NAME);
      await screen.getByRole("button", "Create").tap();
      await expect(browser).toHaveURL(/\/orgs\/[0-9a-f-]+\/sandboxes$/);

      await screen.getByRole("combobox", "Organization").tap();
      await expect(screen.getByRole("listbox")).toBeVisible();
      const m = await browser.evaluate(() => {
        const popup = document.querySelector(".dui-select-content");
        const box = popup?.getBoundingClientRect();
        return {
          left: box ? Math.round(box.left) : -1,
          right: box ? Math.round(box.right) : -1,
          viewportWidth: document.documentElement.clientWidth,
        };
      });
      expect(m.left).toBeGreaterThanOrEqual(0);
      expect(m.right).toBeLessThanOrEqual(m.viewportWidth);
      // The whole name stays readable: it wraps instead of being cut off.
      await expect(screen.getByRole("option", LONG_NAME)).toBeVisible();
      await app.screenshot(`organization-popup-long-option-${width}`);
      await browser.keyboard.press("Escape");
    }
  });
});

interface DialogMetrics {
  panelScrollWidth: number;
  panelClientWidth: number;
  titleScrollWidth: number;
  titleClientWidth: number;
  titleHeight: number;
  titleLineHeight: number;
}

async function measureDialog(browser: {
  evaluate<T extends import("e2e").JsonValue>(fn: () => T): Promise<T>;
}): Promise<DialogMetrics> {
  return browser.evaluate(() => {
    const panel = document.querySelector(".dui-dialog-content[data-open]");
    const title = panel?.querySelector(".dui-dialog-title");
    if (!panel || !title) {
      return {
        panelScrollWidth: -1,
        panelClientWidth: -1,
        titleScrollWidth: -1,
        titleClientWidth: -1,
        titleHeight: -1,
        titleLineHeight: -1,
      };
    }
    const style = getComputedStyle(title);
    return {
      panelScrollWidth: panel.scrollWidth,
      panelClientWidth: panel.clientWidth,
      titleScrollWidth: title.scrollWidth,
      titleClientWidth: title.clientWidth,
      titleHeight: title.getBoundingClientRect().height,
      titleLineHeight:
        parseFloat(style.lineHeight) || parseFloat(style.fontSize) * 1.2,
    };
  });
}

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
