// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

const PREFIX = "ownsand-e2e-organization-refresh";
type Step = "list" | "mutation";

const refreshTest = test.extend<{
  requests: {
    hold(...steps: Step[]): Promise<void>;
    waitFor(step: Step, status: "held" | "released"): Promise<void>;
    release(step: Step): Promise<void>;
    tokens(): Promise<string[]>;
  };
}>({
  requests: async ({ browser }, use) => {
    await use({
      async hold(...steps) {
        await browser.evaluate(
          ({ prefix, steps }) => {
            sessionStorage.setItem(prefix, "true");
            for (const step of steps) {
              sessionStorage.setItem(`${prefix}-${step}`, "hold");
            }
            return null;
          },
          { prefix: PREFIX, steps },
        );
      },
      async waitFor(step, status) {
        const actual = await browser.evaluate(
          async ({ key, status }) => {
            const deadline = performance.now() + 5_000;
            while (
              sessionStorage.getItem(key) !== status &&
              performance.now() < deadline
            ) {
              await new Promise(requestAnimationFrame);
            }
            return sessionStorage.getItem(key);
          },
          { key: `${PREFIX}-${step}`, status },
        );
        expect(actual).toEqual(status);
      },
      async release(step) {
        await browser.evaluate((event) => {
          window.dispatchEvent(new Event(event));
          return null;
        }, `${PREFIX}-${step}-release`);
        await this.waitFor(step, "released");
      },
      tokens() {
        return browser.evaluate(
          (key) => JSON.parse(sessionStorage.getItem(key) ?? "[]") as string[],
          `${PREFIX}-tokens`,
        );
      },
    });
  },
});

describe("organization list refresh after gaining membership", () => {
  refreshTest(
    "restarts the first page after leaving invitation acceptance",
    async ({ app, browser, screen, requests }) => {
      await app.open("/invitations/accept#secret=osi_mock_contoso_invitation");
      await requests.hold("mutation", "list");
      await screen.getByRole("button", "Sign in to accept").tap();
      await screen.getByRole("button", "Accept invitation").tap();
      await requests.waitFor("mutation", "held");
      await screen.getByRole("link", "Not now").tap();
      await requests.waitFor("list", "held");
      expect(await requests.tokens()).toEqual([""]);

      await requests.release("mutation");
      const joined = screen.getByRole("link", /^Contoso Research /);
      await expect(joined).toBeVisible();
      expect(await requests.tokens()).toEqual(["", ""]);

      await requests.release("list");
      await expect(browser).toHaveURL("/");
      await expect(joined).toBeVisible();
      await app.screenshot("joined-organization-list");

      await screen.getByRole("link", "Acme Robotics Owner").tap();
      await screen.getByRole("combobox", "Organization").tap();
      await expect(
        screen.getByRole("option", "Contoso Research"),
      ).toBeVisible();
      await app.screenshot("joined-organization-switcher");
    },
  );

  refreshTest(
    "restarts the first page after dismissing organization creation",
    async ({ app, browser, screen, requests }) => {
      await app.open("/");
      await requests.hold("list", "mutation");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await requests.waitFor("list", "held");
      expect(await requests.tokens()).toEqual([""]);
      await screen.getByRole("button", "Create organization").tap();
      await browser.keyboard.type("Fabrikam");
      await screen.getByRole("button", "Create").tap();
      await requests.waitFor("mutation", "held");
      await browser.keyboard.press("Escape");

      await requests.release("mutation");
      const created = screen.getByRole("link", "Fabrikam Owner");
      await expect(created).toBeVisible();
      expect(await requests.tokens()).toEqual(["", ""]);

      await requests.release("list");
      await expect(browser).toHaveURL("/");
      await expect(created).toBeVisible();
      await expect(screen.getByRole("dialog")).toBeHidden();
      await app.screenshot("created-organization-after-dismissal");
    },
  );

  refreshTest(
    "opens the new organization's Sandboxes while the list refresh waits",
    async ({ app, browser, screen, requests }) => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await expect(
        screen.getByRole("link", "Acme Robotics Owner"),
      ).toBeVisible();
      await requests.hold("list");

      await screen.getByRole("button", "Create organization").tap();
      await browser.keyboard.type("Fabrikam");
      await screen.getByRole("button", "Create").tap();

      await requests.waitFor("list", "held");
      expect(await requests.tokens()).toEqual([""]);
      await expect(browser).toHaveURL(/\/orgs\/[0-9a-f-]+\/sandboxes$/);
      await expect(
        screen.getByRole("heading", "Sandboxes", { level: 1 }),
      ).toBeVisible();
      await app.screenshot("created-organization-before-list-refresh");

      await requests.release("list");
      await expect(screen.getByRole("combobox", "Organization")).toContainText(
        "Fabrikam",
      );
    },
  );
});
