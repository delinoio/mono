// SPDX-License-Identifier: Apache-2.0
import { Code } from "@connectrpc/connect";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

const ACME = "0193a000-0000-7000-8000-000000000001";
const LATER_TEMPLATE = "Template 201";

const pagedSources = test.extend<{
  sources: {
    open(path: string, options?: Record<string, string>): Promise<void>;
    value(name: string): Promise<string>;
    tokens(method: string): Promise<string[]>;
  };
}>({
  sources: async ({ app, browser, screen }, use) => {
    await use({
      async open(path, options = {}) {
        await app.open("/");
        await screen.getByRole("button", "Sign in with Delino").tap();
        await expect(
          screen.getByRole("link", "Acme Robotics Owner"),
        ).toBeVisible();
        await browser.evaluate((settings) => {
          sessionStorage.setItem("ownsand-e2e-paged-list", "SnapshotSources");
          for (const [key, value] of Object.entries(settings)) {
            sessionStorage.setItem(`ownsand-e2e-${key}`, value);
          }
          return true;
        }, options);
        await app.open(`/orgs/${ACME}/${path}`);
      },
      value(name) {
        return browser.evaluate(
          (key) => sessionStorage.getItem(`ownsand-e2e-${key}`) ?? "",
          name,
        );
      },
      async tokens(method) {
        return JSON.parse(
          (await this.value(`${method}-tokens`)) || "[]",
        ) as string[];
      },
    });
  },
});

describe("snapshot and template pagination", () => {
  pagedSources(
    "refreshes an uncached Templates page after a dismissed create",
    async ({ app, browser, screen, sources }) => {
      await sources.open("snapshots", {
        "template-first-page": "hold",
        "paged-list": "",
      });
      await expect(
        screen.getByRole("status", "Loading templates"),
      ).toBeVisible();
      const opener = screen.getByRole("button", /^Make template/);
      await opener.tap();
      const form = screen.getByRole("dialog", "Make a template");
      const name = form.getByLabel("Template name");
      await name.fill("review-template");
      const submit = form.getByRole("button", "Create template");
      await submit.tap();
      await expect(submit).toBeDisabled();
      expect(await sources.value("template-create")).toBe("held");
      expect(await sources.value("template-list-tokens")).toBe('[""]');

      await browser.keyboard.press("Escape");
      await expect(form).toBeHidden();
      await expect(browser.locator(".dui-dialog-backdrop")).toHaveCount(0);
      await opener.tap();
      await name.fill("another-template");
      await browser.evaluate(() => {
        window.dispatchEvent(new Event("ownsand-e2e-template-create-release"));
        return true;
      });

      const deleteTemplate = screen.getByRole(
        "button",
        /^Delete template review-template /,
      );
      const row = browser
        .locator('ul[aria-label="Templates"] li')
        .filter({ hasText: "review-template" });
      await expect(row).toHaveCount(1);
      await expect(name).toHaveValue("another-template");
      expect(await sources.value("template-list-tokens")).toBe('["",""]');
      expect(await sources.value("template-first-page")).toBe("held");
      await browser.evaluate(() => {
        window.dispatchEvent(
          new Event("ownsand-e2e-template-first-page-release"),
        );
        return true;
      });

      expect(await sources.value("template-first-page")).toBe("delivered");
      await expect(name).toHaveValue("another-template");
      await browser.keyboard.press("Escape");
      await expect(deleteTemplate).toBeVisible();
      await expect(browser.locator(".dui-dialog-backdrop")).toHaveCount(0);
      await app.screenshot("fresh-template-after-late-first-page");
    },
  );

  pagedSources(
    "loads management pages independently and refreshes a later snapshot",
    async ({ app, browser, screen, sources }) => {
      await sources.open("snapshots", { "snapshot-exporting": "true" });
      const snapshots = screen
        .getByRole("list", "Snapshots")
        .getByRole("listitem");
      const templates = screen
        .getByRole("list", "Templates")
        .getByRole("listitem");
      await expect(snapshots).toHaveCount(200);
      await expect(templates).toHaveCount(200);
      const showSnapshots = screen.getByRole("button", "Show more snapshots");
      const showTemplates = screen.getByRole("button", "Show more templates");

      await showSnapshots.tap();
      await expect(snapshots).toHaveCount(201);
      await expect(snapshots.last().getByText(/Exporting/)).toBeVisible();
      await expect(showSnapshots).toBeHidden();
      await expect(templates).toHaveCount(200);
      expect((await sources.tokens("ListTemplates")).filter(Boolean)).toEqual(
        [],
      );
      const snapshotContinuations = (
        await sources.tokens("ListSnapshots")
      ).filter(Boolean);

      await browser.evaluate(() => {
        sessionStorage.removeItem("ownsand-e2e-snapshot-exporting");
        return true;
      });
      const exportButton = snapshots
        .last()
        .getByRole("button", "Export snapshot", { exact: false });
      await expect(exportButton).toBeVisible();
      expect((await sources.tokens("ListSnapshots")).filter(Boolean)).toEqual([
        ...snapshotContinuations,
        snapshotContinuations.at(-1),
      ]);

      await exportButton.tap();
      await expect(snapshots.last().getByText(/Exported/)).toBeVisible();
      await app.screenshot("later-snapshot-exported");

      await showTemplates.tap();
      await expect(templates).toHaveCount(201);
      await expect(showTemplates).toBeHidden();
      const laterTemplate = templates.filter({ hasText: LATER_TEMPLATE });
      await expect(laterTemplate).toHaveCount(1);
      await app.screenshot("later-template-loaded");
      await laterTemplate
        .getByRole("button", "Delete template", { exact: false })
        .tap();
      await screen.getByRole("button", "Delete template").tap();
      await expect(laterTemplate).toBeHidden();
      await expect(templates).toHaveCount(200);
      await expect(browser.locator(".dui-dialog-backdrop")).toHaveCount(0);

      await snapshots
        .last()
        .getByRole("button", "Make template", { exact: false })
        .tap();
      const form = screen.getByRole("dialog", "Make a template");
      await form.getByLabel("Template name").fill("From snapshot 201");
      await form.getByRole("button", "Create template").tap();
      await expect(
        templates.filter({ hasText: "From snapshot 201" }),
      ).toHaveCount(1);
    },
  );

  for (const kind of ["snapshot", "template"] as const) {
    pagedSources(
      `creates a sandbox from a later ${kind}`,
      async ({ app, browser, screen, sources }) => {
        await sources.open("sandboxes");
        await screen.getByRole("button", "Create sandbox").tap();
        const dialog = screen.getByRole("dialog", "Create a sandbox");
        // Both lists load in parallel, and an empty initial fixture value
        // cannot look like a loaded option.
        await expect(
          dialog.getByText(/Loading (sources|more snapshots|more templates)/),
        ).toBeHidden();
        expect(
          (await sources.tokens("ListSnapshots")).filter(Boolean),
        ).toHaveLength(1);
        expect(
          (await sources.tokens("ListTemplates")).filter(Boolean),
        ).toHaveLength(1);
        const id = await sources.value(`later-${kind}`);
        expect(id).toMatch(/^[0-9a-f-]{36}$/);

        await dialog.getByRole("combobox", "Source").tap();
        const option =
          kind === "template"
            ? screen.getByRole("option", `Template: ${LATER_TEMPLATE}`)
            : screen.getByRole(
                "option",
                `Snapshot ${id.slice(0, 8)}…${id.slice(-4)}`,
                { exact: false },
              );
        await expect(option).toHaveCount(1);
        await option.tap();
        await app.screenshot(`later-${kind}-selected`);
        await dialog.getByRole("button", "Create").tap();
        await expect(browser).toHaveURL(/\/sandboxes\/[0-9a-f-]+$/);
        expect(JSON.parse(await sources.value("created-source"))).toEqual({
          case: `${kind}Id`,
          value: id,
        });

        await screen.getByRole("link", "Snapshots").tap();
        await expect(
          screen.getByRole("list", "Snapshots").getByRole("listitem"),
        ).toHaveCount(200);
        await expect(
          screen.getByRole("list", "Templates").getByRole("listitem"),
        ).toHaveCount(200);
      },
    );
  }

  pagedSources(
    "keeps typed values and loaded choices when a picker continuation fails",
    async ({ app, browser, screen, sources }) => {
      await sources.open("sandboxes", {
        "source-fault": "ListTemplates",
        "page-error": "hold",
      });
      await screen.getByRole("button", "Create sandbox").tap();
      const dialog = screen.getByRole("dialog", "Create a sandbox");
      await expect(dialog.getByText("Loading more templates")).toBeVisible();
      await dialog.getByLabel("Labels").fill("owner=qa");
      const select = dialog.getByRole("combobox", "Source");
      await select.tap();
      await screen.getByRole("option", "Template: node-24-toolchain").tap();

      await browser.evaluate((code) => {
        sessionStorage.setItem("ownsand-e2e-page-error", String(code));
        window.dispatchEvent(new Event("ownsand-e2e-release-page"));
        return true;
      }, Code.Unavailable);

      const notice = dialog.getByText("Some templates are not shown yet.");
      await expect(notice).toBeVisible();
      await expect(select).toHaveText("Template: node-24-toolchain");
      await expect(dialog.getByLabel("Labels")).toHaveValue("owner=qa");
      await app.screenshot("partial-template-choices");
      const failedTokens = await sources.tokens("ListTemplates");

      await browser.evaluate(() => {
        sessionStorage.removeItem("ownsand-e2e-page-error");
        return true;
      });
      await dialog.getByRole("button", "Try again").tap();
      await expect(dialog.getByText(/Loading more templates/)).toBeHidden();
      await expect(notice).toBeHidden();
      expect(await sources.tokens("ListTemplates")).toEqual([
        ...failedTokens,
        failedTokens.at(-1),
      ]);
      await expect(dialog.getByLabel("Labels")).toHaveValue("owner=qa");
      await select.tap();
      await expect(
        screen.getByRole("option", `Template: ${LATER_TEMPLATE}`),
      ).toHaveCount(1);
    },
  );

  pagedSources(
    "removes a selected source after its list returns a terminal error",
    async ({ app, browser, screen, sources }) => {
      await sources.open("sandboxes", {
        "source-fault": "ListTemplates",
        "page-error": "hold",
      });
      await screen.getByRole("button", "Create sandbox").tap();
      const dialog = screen.getByRole("dialog", "Create a sandbox");
      await expect(dialog.getByText("Loading more templates")).toBeVisible();
      const select = dialog.getByRole("combobox", "Source");
      await select.tap();
      await screen.getByRole("option", "Template: node-24-toolchain").tap();

      await browser.evaluate((code) => {
        sessionStorage.setItem("ownsand-e2e-page-error", String(code));
        window.dispatchEvent(new Event("ownsand-e2e-release-page"));
        return true;
      }, Code.Unauthenticated);

      await expect(
        dialog.getByRole("button", "Sign in with Delino"),
      ).toBeVisible();
      await expect(dialog.getByRole("button", "Create")).toHaveAttribute(
        "disabled",
        "",
      );
      await app.screenshot("template-choices-session-error");
      await select.tap();
      await expect(
        screen.getByRole("option", "Template: node-24-toolchain"),
      ).toHaveCount(0);
      await expect(
        screen.getByRole("option", "Linux base image"),
      ).toBeVisible();
      await expect(
        screen.getByRole("option", /^Snapshot /).first(),
      ).toBeVisible();
    },
  );
});
