// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { createClient, type Transport } from "@connectrpc/connect";
import { ApiKeyService } from "@delino/ownsand-protos/v1/api_key_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { SnapshotService } from "@delino/ownsand-protos/v1/snapshot_pb";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  within,
} from "@testing-library/react";
import { shortIdText } from "./components/ui.tsx";
import { createMockTransport, MOCK_IDS } from "./mock/mock-api.ts";
import { renderConsole, settle } from "./test-support.tsx";

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

const namesOf = (scope: HTMLElement, role = "button") =>
  within(scope)
    .getAllByRole(role)
    .map((b) => b.getAttribute("aria-label") ?? b.textContent);

async function press(element: HTMLElement) {
  await act(async () => {
    fireEvent.click(element);
  });
  await settle(60);
}

describe("snapshot and template rows", () => {
  async function twoSnapshots() {
    const transport: Transport = createMockTransport();
    const snapshots = createClient(SnapshotService, transport);
    const second = (
      await snapshots.createSnapshot({
        organizationId: MOCK_IDS.acme,
        sandboxId: MOCK_IDS.runningSandbox,
      })
    ).snapshot!;
    // Two templates with the same name on two snapshots.
    await snapshots.createTemplate({
      organizationId: MOCK_IDS.acme,
      snapshotId: second.snapshotId,
      name: "node-24-toolchain",
    });
    renderConsole(`/orgs/${MOCK_IDS.acme}/snapshots`, { transport });
    await screen.findByRole("list", { name: "Snapshots" });
    await settle(60);
    return second.snapshotId;
  }

  test("every row button names its row and starts with its visible text", async () => {
    const secondId = await twoSnapshots();
    const firstShort = shortIdText(MOCK_IDS.snapshot);
    const secondShort = shortIdText(secondId);
    const rows = within(screen.getByRole("list", { name: "Snapshots" }))
      .getAllByRole("listitem")
      .map((li) => namesOf(li));
    expect(rows.flat()).toEqual(
      expect.arrayContaining([
        `Export snapshot ${firstShort}`,
        `Make template from snapshot ${firstShort}`,
        `Delete snapshot ${firstShort}`,
        `Export snapshot ${secondShort}`,
        `Make template from snapshot ${secondShort}`,
        `Delete snapshot ${secondShort}`,
      ]),
    );
    const templates = namesOf(screen.getByRole("list", { name: "Templates" }));
    expect(templates).toEqual([
      `Delete template node-24-toolchain from snapshot ${firstShort}`,
      `Delete template node-24-toolchain from snapshot ${secondShort}`,
    ]);
    // The accessible name starts with the visible text, and the visible text
    // is unchanged.
    for (const button of screen.getAllByRole("button")) {
      const label = button.getAttribute("aria-label");
      if (label !== null) {
        expect(label.startsWith(button.textContent!.trim())).toBe(true);
      }
    }
    expect(
      within(screen.getByRole("list", { name: "Snapshots" }))
        .getAllByRole("button")
        .map((b) => b.textContent)
        .filter((t) => t === "Delete"),
    ).toHaveLength(2);
  });

  test("the snapshot delete confirmation names the snapshot, Cancel deletes nothing and confirm deletes only that snapshot", async () => {
    const secondId = await twoSnapshots();
    const secondShort = shortIdText(secondId);
    await press(
      screen.getByRole("button", { name: `Delete snapshot ${secondShort}` }),
    );
    let dialog = await screen.findByRole("alertdialog", {
      name: `Delete the snapshot ${secondShort}?`,
    });
    await press(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(
      screen.getByRole("button", { name: `Delete snapshot ${secondShort}` }),
    ).toBeTruthy();

    // A snapshot with a template cannot be deleted; delete that template
    // first, then the snapshot.
    await press(
      screen.getByRole("button", {
        name: `Delete template node-24-toolchain from snapshot ${secondShort}`,
      }),
    );
    dialog = await screen.findByRole("alertdialog");
    await press(
      within(dialog).getByRole("button", { name: "Delete template" }),
    );
    await press(
      screen.getByRole("button", { name: `Delete snapshot ${secondShort}` }),
    );
    dialog = await screen.findByRole("alertdialog", {
      name: `Delete the snapshot ${secondShort}?`,
    });
    await press(
      within(dialog).getByRole("button", { name: "Delete snapshot" }),
    );
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(
      screen.queryByRole("button", { name: `Delete snapshot ${secondShort}` }),
    ).toBeNull();
    expect(
      screen.getByRole("button", {
        name: `Delete snapshot ${shortIdText(MOCK_IDS.snapshot)}`,
      }),
    ).toBeTruthy();
  });
});

describe("enrollment token, API key and member rows", () => {
  test("enrollment token Revoke names the token prefix", async () => {
    const transport = createMockTransport();
    const devices = createClient(DeviceService, transport);
    const prefixes: string[] = [];
    for (let i = 0; i < 2; i += 1) {
      const res = await devices.createEnrollmentToken({
        organizationId: MOCK_IDS.acme,
        ttl: { seconds: 3600n },
      });
      prefixes.push(res.enrollmentToken.slice(0, 8));
    }
    renderConsole(`/orgs/${MOCK_IDS.acme}/enrollment-tokens`, { transport });
    const list = await screen.findByRole("list", {
      name: "Enrollment tokens",
    });
    await settle(60);
    const names = namesOf(list);
    expect(names).toHaveLength(2);
    expect(names.sort()).toEqual(
      prefixes.map((p) => `Revoke enrollment token ${p}`).sort(),
    );
  });

  test("API key Revoke names the key and its prefix, also for equal names", async () => {
    const transport = createMockTransport();
    const keys = createClient(ApiKeyService, transport);
    for (let i = 0; i < 2; i += 1) {
      await keys.createApiKey({
        organizationId: MOCK_IDS.acme,
        name: "ci-key",
      });
    }
    renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`, { transport });
    const list = await screen.findByRole("list", { name: "API keys" });
    await settle(60);
    const names = namesOf(list);
    const ci = names.filter((n) => n!.startsWith("Revoke API key ci-key ("));
    expect(ci).toHaveLength(2);
    expect(new Set(ci).size).toBe(2);
    expect(
      ci.every((n) => /^Revoke API key ci-key \(osk_[0-9a-f]+\)$/.test(n!)),
    ).toBe(true);
  });

  test("Remove names the member, and Revoke names the invitation", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/members`);
    const members = await screen.findByRole("list", { name: "Members" });
    await settle(60);
    expect(namesOf(members).filter((n) => n!.startsWith("Remove"))).toEqual([
      "Remove Jun Park",
      "Remove user-member-ada",
    ]);
    const invitations = await screen.findByRole("list", {
      name: "Pending invitations",
    });
    expect(namesOf(invitations)).toEqual(["Revoke Member invitation osi_7f3a"]);
    // The visible text of every row button is unchanged.
    expect(within(invitations).getByRole("button").textContent).toBe("Revoke");
  });
});
