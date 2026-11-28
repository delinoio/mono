// SPDX-License-Identifier: Apache-2.0
import { describe, test, type Browser } from "@e2e-dev/web";
import { expect } from "e2e";

const ACME = "0193a000-0000-7000-8000-000000000001";
const SOURCE_SANDBOX = "0193a000-0000-7000-8000-0000000000a1";
const ACTION_NAMES = [
  "Export snapshot 0193a000…00b1",
  "Make template from snapshot 0193a000…00b1",
  "Delete snapshot 0193a000…00b1",
];

const snapshotPage = test.extend<{
  openSnapshots: (exportFailed?: boolean) => Promise<void>;
}>({
  openSnapshots: async ({ app, browser, screen }, use) => {
    await use(async (exportFailed = false) => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await expect(
        screen.getByRole("link", "Acme Robotics Owner"),
      ).toBeVisible();
      if (exportFailed) {
        await browser.evaluate(() => {
          sessionStorage.setItem("ownsand-e2e-snapshot-export-failed", "true");
          return true;
        });
      }
      await app.open("/orgs/" + ACME + "/snapshots");
    });
  },
});

function measureSnapshotRow(browser: Browser) {
  return browser.evaluate(() => {
    const row = document.querySelector('ul[aria-label="Snapshots"] > li')!;
    const details = row.firstElementChild!;
    const actions = row.lastElementChild!;
    const detailBox = details.getBoundingClientRect();
    const actionBox = actions.getBoundingClientRect();
    const rowBox = row.getBoundingClientRect();
    const doc = document.documentElement;
    const isInside = (inner: DOMRect, outer: DOMRect) =>
      inner.width > 0 &&
      inner.left >= outer.left - 0.5 &&
      inner.right <= outer.right + 0.5 &&
      inner.top >= outer.top - 0.5 &&
      inner.bottom <= outer.bottom + 0.5;

    // Text can extend past its element even when the list clips the overflow.
    const range = document.createRange();
    range.selectNodeContents(details);
    const detailsFit = Array.from(range.getClientRects())
      .filter((box) => box.width > 0)
      .every((box) => isInside(box, detailBox));
    const ids = Array.from(details.querySelectorAll("code"));

    return {
      documentFits: doc.scrollWidth === doc.clientWidth,
      detailsAboveActions: detailBox.bottom <= actionBox.top,
      detailsBesideActions:
        detailBox.right <= actionBox.left &&
        detailBox.top < actionBox.bottom &&
        actionBox.top < detailBox.bottom,
      detailsFit,
      shortIdsFit:
        ids.length === 2 &&
        ids.every((id) => isInside(id.getBoundingClientRect(), detailBox)),
      actionsFit: isInside(actionBox, rowBox),
      buttonsFit: Array.from(actions.querySelectorAll("button")).every(
        (button) => isInside(button.getBoundingClientRect(), rowBox),
      ),
    };
  });
}

describe("snapshot row layout", () => {
  for (const { status, exportFailed } of [
    { status: "Only on its device", exportFailed: false },
    { status: "Export failed", exportFailed: true },
  ]) {
    snapshotPage(
      "keeps narrow snapshot details and all three actions visible for " +
        status,
      async ({ browser, screen, openSnapshots }) => {
        await openSnapshots(exportFailed);
        const row = screen.getByRole("list", "Snapshots").getByRole("listitem");
        await expect(
          row.getByText("Snapshot 0193a000…00b1 · Ready · " + status),
        ).toBeVisible();
        const source = row.getByRole("link", "0193a000…00a1");
        await expect(source).toHaveAttribute(
          "href",
          "/orgs/" + ACME + "/sandboxes/" + SOURCE_SANDBOX,
        );
        await expect(row.getByRole("button")).toHaveCount(ACTION_NAMES.length);

        for (const width of [320, 360, 390]) {
          await browser.setViewport({ width, height: 844 });
          const metrics = await measureSnapshotRow(browser);
          expect(metrics, "Snapshot row at " + width + " px").toEqual({
            documentFits: true,
            detailsAboveActions: true,
            detailsBesideActions: false,
            detailsFit: true,
            shortIdsFit: true,
            actionsFit: true,
            buttonsFit: true,
          });
          await source.focus();
          for (const name of ACTION_NAMES) {
            const action = row.getByRole("button", name);
            await expect(action).toBeVisible();
            await browser.keyboard.press("Tab");
            await expect(action).toBeFocused();
          }
        }
      },
    );
  }

  snapshotPage(
    "keeps snapshot and template details beside their actions on wide screens",
    async ({ browser, screen, openSnapshots }) => {
      await openSnapshots();
      await expect(screen.getByRole("button", ACTION_NAMES[0])).toBeVisible();
      await expect(screen.getByText("node-24-toolchain")).toBeVisible();
      for (const width of [761, 1024]) {
        await browser.setViewport({ width, height: 844 });
        expect((await measureSnapshotRow(browser)).detailsBesideActions).toBe(
          true,
        );
        const templateDetailsBesideAction = await browser.evaluate(() => {
          const row = document.querySelector(
            'ul[aria-label="Templates"] > li',
          )!;
          const details = row.firstElementChild!.getBoundingClientRect();
          const action = row.lastElementChild!.getBoundingClientRect();
          return (
            details.right <= action.left &&
            details.top < action.bottom &&
            action.top < details.bottom
          );
        });
        expect(
          templateDetailsBesideAction,
          "Template row at " + width + " px",
        ).toBe(true);
      }
    },
  );

  snapshotPage(
    "keeps the available actions reachable after export on a narrow screen",
    async ({ browser, screen, openSnapshots }) => {
      await browser.setViewport({ width: 320, height: 844 });
      await openSnapshots();
      const row = screen.getByRole("list", "Snapshots").getByRole("listitem");
      const exportButton = row.getByRole("button", ACTION_NAMES[0]);
      await exportButton.tap();
      await expect(
        row.getByText("Snapshot 0193a000…00b1 · Ready · Exported"),
      ).toBeVisible();
      await expect(exportButton).toHaveCount(0);
      await expect(row.getByRole("button")).toHaveCount(2);
      await row.getByRole("link", "0193a000…00a1").focus();
      for (const name of ACTION_NAMES.slice(1)) {
        const action = row.getByRole("button", name);
        await expect(action).toBeVisible();
        await browser.keyboard.press("Tab");
        await expect(action).toBeFocused();
      }
    },
  );
});
