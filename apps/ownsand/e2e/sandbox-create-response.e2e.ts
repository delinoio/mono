// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

const SANDBOXES = "/orgs/0193a000-0000-7000-8000-000000000001/sandboxes";

const createSandbox = test.extend<{
  fixture: {
    open(
      response:
        | "missing-sandbox"
        | "empty-id"
        | "held"
        | "valid"
        | "lost"
        | "lost-twice",
    ): Promise<void>;
    creates(): Promise<{ request: string; sandboxId: string }[]>;
  };
}>({
  fixture: async ({ app, browser, screen }, use) => {
    await use({
      async open(response) {
        await app.open("/");
        await screen.getByRole("button", "Sign in with Delino").tap();
        await expect(
          screen.getByRole("link", "Acme Robotics Owner"),
        ).toBeVisible();
        await browser.evaluate((mode) => {
          sessionStorage.setItem("ownsand-e2e-create-response", mode);
          return true;
        }, response);
        await app.open(SANDBOXES);
        await screen.getByRole("button", "Create sandbox").tap();
      },
      creates() {
        return browser.evaluate(() =>
          JSON.parse(sessionStorage.getItem("ownsand-e2e-creates") ?? "[]"),
        );
      },
    });
  },
});

describe("sandbox create replies", () => {
  for (const response of ["missing-sandbox", "empty-id", "lost"] as const) {
    createSandbox(
      `recovers a committed create after ${response}`,
      async ({ app, browser, screen, fixture }) => {
        await fixture.open(response);
        const dialog = screen.getByRole("dialog", "Create a sandbox");
        const labels = dialog.getByLabel("Labels");
        await labels.fill("team=recovery-test");
        const vcpus = dialog.getByLabel("vCPUs");
        await vcpus.fill("3");
        await dialog.getByRole("combobox", "Network").tap();
        await screen.getByRole("option", "No network").tap();
        await dialog.getByRole("combobox", "Device").tap();
        await screen.getByRole("option", "build-box-1 (Online)").tap();
        await dialog.getByRole("button", "Create").tap();
        await expect(dialog.getByRole("alert")).toContainText(
          "The sandbox may have been created.",
        );
        await expect(browser).toHaveURL(SANDBOXES);
        await expect(labels).toHaveValue("team=recovery-test");
        await expect(labels).toBeDisabled();
        await expect(vcpus).toHaveValue("3");
        const [initial] = await fixture.creates();
        expect(initial?.sandboxId).toMatch(/^[0-9a-f-]{36}$/);
        await app.screenshot(`sandbox-create-${response}`);

        await dialog.getByRole("button", "Retry original request").tap();
        await expect(browser).toHaveURL(`${SANDBOXES}/${initial!.sandboxId}`);
        await expect(dialog).toBeHidden();
        expect(await fixture.creates()).toEqual([initial, initial]);
        await app.screenshot(`sandbox-create-recovered-${response}`);

        await screen.getByRole("link", "Sandboxes", { exact: true }).tap();
        await expect(screen.getByText("team=recovery-test")).toHaveCount(1);
      },
    );
  }

  createSandbox(
    "blocks retry at the original deadline after two lost replies",
    async ({ app, browser, screen, fixture }) => {
      const windowMs = 24 * 60 * 60 * 1000;
      await fixture.open("lost-twice");
      const dialog = screen.getByRole("dialog", "Create a sandbox");
      const start = await browser.evaluate(() => {
        const now = Date.now();
        Date.now = () => now;
        return now;
      });
      await dialog.getByRole("button", "Create").tap();

      const retry = dialog.getByRole("button", "Retry original request");
      await expect(retry).toBeEnabled();
      await expect(dialog.getByLabel("vCPUs")).toBeDisabled();
      await browser.evaluate(
        (time) => {
          Date.now = () => time;
          return true;
        },
        start + windowMs - 60_000,
      );
      await retry.tap();
      await expect(retry).toBeEnabled();
      const [first] = await fixture.creates();
      expect(first?.sandboxId).toMatch(/^[0-9a-f-]{36}$/);
      expect(await fixture.creates()).toEqual([first, first]);

      // Advance the wall clock without waiting for the scheduled timer.
      await browser.evaluate((time) => {
        Date.now = () => time;
        return true;
      }, start + windowMs);
      await expect(retry).toBeEnabled();
      await retry.tap();
      await expect(retry).toBeDisabled();
      await expect(dialog.getByRole("alert")).toContainText(
        "Check the sandbox list",
      );
      await expect(browser).toHaveURL(SANDBOXES);
      expect(await fixture.creates()).toEqual([first, first]);
      await app.screenshot("sandbox-create-lost-expired");
    },
  );

  createSandbox(
    "opens the sandbox when the current reply has a valid ID",
    async ({ browser, screen, fixture }) => {
      await fixture.open("valid");
      const dialog = screen.getByRole("dialog", "Create a sandbox");
      await dialog.getByRole("button", "Create").tap();
      await expect(browser).toHaveURL(
        new RegExp(`${SANDBOXES}/[0-9a-f-]{36}$`),
      );
      await expect(dialog).toBeHidden();
      const [first] = await fixture.creates();
      await expect(browser).toHaveURL(`${SANDBOXES}/${first!.sandboxId}`);
    },
  );

  createSandbox(
    "refreshes the list when an incomplete reply arrives after dismissal",
    async ({ app, browser, screen, fixture }) => {
      await fixture.open("held");
      const dialog = screen.getByRole("dialog", "Create a sandbox");
      await dialog.getByLabel("Labels").fill("team=late-create");
      await dialog.getByRole("button", "Create").tap();
      await expect(dialog.getByRole("button", "Create")).toBeDisabled();
      // Cancel and start a new draft before delivering the previous response.
      await dialog.getByRole("button", "Cancel").tap();
      await expect(dialog).toBeHidden();
      const opener = screen.getByRole("button", "Create sandbox");
      await expect(opener).toBeFocused();
      await opener.tap();
      await dialog.getByLabel("Labels").fill("team=new-draft");
      await browser.evaluate(() => {
        window.dispatchEvent(new Event("ownsand-e2e-release-create"));
        return true;
      });

      await expect(screen.getByText("team=late-create")).toBeVisible();
      await expect(dialog.getByLabel("Labels")).toHaveValue("team=new-draft");
      await expect(dialog.getByRole("button", "Create")).toBeEnabled();
      await expect(dialog.getByRole("alert")).toBeHidden();
      await expect(browser).toHaveURL(SANDBOXES);
      await app.screenshot("sandbox-create-late-reply");
    },
  );
});
