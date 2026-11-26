// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

const SANDBOXES = "/orgs/0193a000-0000-7000-8000-000000000001/sandboxes";

describe("sandbox restores", () => {
  for (const [kind, source] of [
    ["snapshot", /^Snapshot 0193a000…00b1 · /],
    ["template", "Template: node-24-toolchain"],
  ] as const) {
    test(`restores the seeded ${kind} with enough disk on the holder`, async ({
      app,
      browser,
      screen,
    }) => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await expect(
        screen.getByRole("link", "Acme Robotics Owner"),
      ).toBeVisible();
      await browser.evaluate(() => {
        sessionStorage.setItem("ownsand-e2e-restore-devices", "true");
        return true;
      });
      await app.open(SANDBOXES);
      await screen.getByRole("button", "Create sandbox").tap();

      const dialog = screen.getByRole("dialog", "Create a sandbox");
      await dialog.getByRole("combobox", "Source").tap();
      await screen.getByRole("option", source).tap();

      const disk = dialog.getByLabel("Disk (GiB)");
      const create = dialog.getByRole("button", "Create");
      await disk.fill("19");
      await create.tap();
      await expect(dialog.getByRole("alert")).toContainText(
        "invalid resources.disk_gib: must be at least the snapshot's disk size",
      );
      await app.screenshot(`${kind}-restore-small-disk`);

      const device = dialog.getByRole("combobox", "Device");
      await disk.fill("20");
      await device.tap();
      await screen.getByRole("option", "Device 003 (Online)").tap();
      await create.tap();
      await expect(dialog.getByRole("alert")).toContainText(
        "the snapshot is not on this device and is not exported",
      );
      await app.screenshot(`${kind}-restore-non-holder`);

      await device.tap();
      await screen.getByRole("option", "build-box-1 (Online)").tap();
      await create.tap();
      await expect(browser).toHaveURL(
        new RegExp(`${SANDBOXES}/[0-9a-f-]{36}$`),
      );
      await expect(dialog).toBeHidden();
      await expect(screen.getByRole("link", "build-box-1")).toBeVisible();
      await expect(screen.getByText(/20 GiB disk$/)).toBeVisible();
      await app.screenshot(`${kind}-restore-holder`);
    });
  }
});
