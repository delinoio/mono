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

describe("template deletion", () => {
  const title = "Delete the template node-24-toolchain?";
  const target = /^Delete template node-24-toolchain from snapshot/;

  pagedSources(
    "keeps the other templates and source snapshot",
    async ({ screen, sources }) => {
      await sources.open("snapshots");
      const source = screen.getByRole(
        "button",
        "Make template from snapshot 0193a000…00b1",
      );
      const other = screen.getByRole(
        "button",
        /^Delete template Template 002 /,
      );
      await expect(source).toBeVisible();
      await expect(other).toBeVisible();

      await screen.getByRole("button", target).tap();
      const confirmation = screen.getByRole("alertdialog", title);
      await confirmation.getByRole("button", "Delete template").tap();

      await expect(confirmation).toBeHidden();
      await expect(screen.getByRole("button", target)).toHaveCount(0);
      await expect(source).toBeVisible();
      await expect(other).toBeVisible();
    },
  );

  pagedSources(
    "shows the missing template error in a stale confirmation",
    async ({ app, screen, sources }) => {
      await sources.open("snapshots", { "stale-template-delete": "true" });
      await screen.getByRole("button", target).tap();
      const confirmation = screen.getByRole("alertdialog", title);
      await confirmation.getByRole("button", "Delete template").tap();

      await expect(
        confirmation
          .getByRole("alert")
          .getByText("It was not found, or you no longer have access to it."),
      ).toBeVisible();
      await app.screenshot("stale-template-delete");
    },
  );
});

describe("open template form after a snapshot refresh", () => {
  const sourceAction = "Make template from snapshot 0193a000…00b1";
  const draft = "review-template-draft";
  const unavailable =
    "The source snapshot is no longer available for templates.";

  const refreshingSource = pagedSources.extend<{
    refreshSource: (state: string) => Promise<void>;
  }>({
    refreshSource: async ({ browser, screen, sources }, use) => {
      await sources.open("snapshots", { "template-source-state": "initial" });
      await screen.getByRole("button", sourceAction).tap();
      await screen
        .getByRole("dialog", "Make a template")
        .getByLabel("Template name")
        .fill(draft);
      await use(async (state) => {
        await browser.evaluate((value) => {
          sessionStorage.setItem("ownsand-e2e-template-source-state", value);
          return true;
        }, state);
      });
    },
  });

  for (const state of ["missing", "non-ready"] as const) {
    refreshingSource(
      `blocks template submission when the refreshed source is ${state}`,
      async ({ app, browser, refreshSource, screen, sources }) => {
        await refreshSource(state);

        const form = screen.getByRole("dialog", "Make a template");
        const name = form.getByLabel("Template name");
        const submit = form.getByRole("button", "Create template");
        await expect(form.getByRole("alert")).toHaveText(unavailable);
        await expect(name).toHaveValue(draft);
        await expect(submit).toBeDisabled();

        await browser.evaluate(() => {
          const form = document.querySelector<HTMLFormElement>(
            '[role="dialog"] form',
          );
          if (!form) throw new Error("Missing template form");
          form
            .querySelector<HTMLButtonElement>('button[type="submit"]')
            ?.click();
          form.querySelector("input")?.focus();
          return true;
        });
        await browser.keyboard.press("Enter");
        await browser.evaluate(() => {
          document
            .querySelector<HTMLFormElement>('[role="dialog"] form')
            ?.requestSubmit();
          return true;
        });
        expect(await sources.value("template-source-snapshot")).toBe("");
        await app.screenshot(`template-source-${state}-draft`);
      },
    );
  }

  refreshingSource(
    "keeps the draft through a failed refresh and submits when the source stays ready",
    async ({ browser, refreshSource, screen, sources }) => {
      const form = screen.getByRole("dialog", "Make a template");
      const name = form.getByLabel("Template name");
      const submit = form.getByRole("button", "Create template");
      await refreshSource("transient");
      await expect(
        screen.getByText("OwnSand is not available right now.", {
          exact: false,
        }),
      ).toBeVisible({ timeout: 10_000 });
      await expect(name).toHaveValue(draft);
      await expect(submit).toBeEnabled();

      await refreshSource("ready");
      await expect(
        browser.locator('ul[aria-label="Snapshots"] li'),
      ).toHaveCount(1, { timeout: 10_000 });
      await expect(name).toHaveValue(draft);
      await submit.tap();
      await expect(form).toBeHidden();
      await expect(screen.getByText(draft, { exact: true })).toBeVisible();
      expect(await sources.value("template-source-snapshot")).toBe(
        "0193a000-0000-7000-8000-0000000000b1",
      );
    },
  );

  refreshingSource(
    "keeps the source through a partial refresh and failed continuation",
    async ({ browser, refreshSource, screen, sources }) => {
      const form = screen.getByRole("dialog", "Make a template");
      const name = form.getByLabel("Template name");
      const submit = form.getByRole("button", "Create template");
      const sourceButton = browser.locator(
        `button[aria-label="${sourceAction}"]`,
      );
      const more = browser.locator('button[aria-label="Show more snapshots"]');
      await refreshSource("partial");
      await expect(more).toBeEnabled();
      await expect(sourceButton).toHaveCount(0);
      await expect(name).toHaveValue(draft);
      await expect(submit).toBeEnabled();

      // Simulate a continuation from the underlying list while the modal is open.
      const showMore = (error: Code | null) =>
        browser.evaluate((code) => {
          sessionStorage.setItem("ownsand-e2e-source-fault", "ListSnapshots");
          if (code === null)
            sessionStorage.removeItem("ownsand-e2e-page-error");
          else sessionStorage.setItem("ownsand-e2e-page-error", String(code));
          document
            .querySelector<HTMLButtonElement>(
              'button[aria-label="Show more snapshots"]',
            )
            ?.click();
          return true;
        }, error);
      await showMore(Code.Unavailable);
      await expect(
        screen.getByText("OwnSand is not available right now.", {
          exact: false,
        }),
      ).toBeVisible({ timeout: 10_000 });
      await expect(name).toHaveValue(draft);
      await expect(submit).toBeEnabled();

      await showMore(null);
      await expect(sourceButton).toHaveCount(1);
      await expect(name).toHaveValue(draft);
      await submit.tap();
      await expect(form).toBeHidden();
      expect(await sources.value("template-source-snapshot")).toBe(
        "0193a000-0000-7000-8000-0000000000b1",
      );
    },
  );

  refreshingSource(
    "dismisses the form when the snapshot refresh fails with a terminal error",
    async ({ refreshSource, screen, sources }) => {
      const form = screen.getByRole("dialog", "Make a template");
      await refreshSource("terminal");
      await expect(form).toBeHidden();
      await expect(
        screen.getByText("You do not have permission to do this."),
      ).toBeVisible();
      expect(await sources.value("template-source-snapshot")).toBe("");
    },
  );
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
      await sources.open("snapshots", {
        "snapshot-export-phase": "exporting",
      });
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
      const showMoreToken =
        (await sources.tokens("ListSnapshots")).find(Boolean) ?? "";
      expect(showMoreToken).toBeTruthy();

      await browser.evaluate(() => {
        sessionStorage.setItem("ownsand-e2e-snapshot-export-phase", "complete");
        return true;
      });
      const exportButton = snapshots
        .last()
        .getByRole("button", "Export snapshot", { exact: false });
      await expect(exportButton).toBeVisible();
      expect(await sources.value("snapshot-completion-token")).toBe(
        showMoreToken,
      );

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

      const laterSnapshotId = await sources.value("later-snapshot");
      expect(laterSnapshotId).toMatch(
        /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/,
      );
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
      expect(await sources.value("template-source-snapshot")).toBe(
        laterSnapshotId,
      );
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
