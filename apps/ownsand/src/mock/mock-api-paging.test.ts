// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { BaseImage, SandboxState } from "@delino/ownsand-protos/v1/common_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { SnapshotService } from "@delino/ownsand-protos/v1/snapshot_pb";
import { uuidV7 } from "../uuid.ts";
import { createMockTransport, MOCK_IDS } from "./mock-api.ts";

const organizationId = MOCK_IDS.acme;

/** The page token that the control plane builds: base64url("v1:" + last ID). */
const tokenOf = (id: string) =>
  btoa(`v1:${id}`).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

async function codeOf(call: Promise<unknown>) {
  const error = await call.then(
    () => undefined,
    (e: unknown) => e,
  );
  expect(error).toBeInstanceOf(ConnectError);
  return (error as ConnectError).code;
}

describe("mock sandbox list paging", () => {
  test("pages 51 matching sandboxes as 50 and then 1 in ascending ID order", async () => {
    const client = createClient(
      SandboxService,
      createMockTransport({ extraSandboxes: 51 }),
    );
    const first = await client.listSandboxes({ organizationId });
    // 3 seeded live sandboxes and 51 extra ones, and the seeded deleted one
    // is not listed.
    expect(first.sandboxes).toHaveLength(50);
    expect(first.nextPageToken).toBe(
      tokenOf(first.sandboxes.at(-1)!.sandboxId),
    );
    const ids = first.sandboxes.map((s) => s.sandboxId);
    expect(ids).toEqual([...ids].sort());
    const second = await client.listSandboxes({
      organizationId,
      pageToken: first.nextPageToken,
    });
    expect(second.sandboxes).toHaveLength(4);
    expect(second.nextPageToken).toBe("");
    const all = [...ids, ...second.sandboxes.map((s) => s.sandboxId)];
    expect(new Set(all).size).toBe(54);
    expect(all).toEqual([...all].sort());
  });

  test("lists sandboxes created in one millisecond in creation order", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const created: string[] = [];
    for (let i = 0; i < 3; i++) {
      const { sandbox } = await client.createSandbox({
        organizationId,
        idempotencyKey: uuidV7(),
        source: {
          source: { case: "baseImage", value: BaseImage.LINUX },
        },
        resources: { vcpuCount: 1, memoryMib: 512n, diskGib: 5n },
        idleTtl: { seconds: 3600n },
        maxTtl: { seconds: 7200n },
      });
      created.push(sandbox!.sandboxId);
    }
    const { sandboxes } = await client.listSandboxes({ organizationId });
    expect(sandboxes.map((s) => s.sandboxId).slice(-3)).toEqual(created);
  });

  test("caps the page size at 200", async () => {
    const client = createClient(
      SandboxService,
      createMockTransport({ extraSandboxes: 201 }),
    );
    const page = await client.listSandboxes({ organizationId, pageSize: 500 });
    expect(page.sandboxes).toHaveLength(200);
    expect(page.nextPageToken).not.toBe("");
  });

  test("walks a filtered list, ends with an empty page after a full one and keeps the filters", async () => {
    const transport = createMockTransport();
    const client = createClient(SandboxService, transport);
    // Seeded: running and paused on build-box, deleted on build-box and
    // unreachable on studio-mac. The filter keeps the two live ones of
    // build-box.
    const filter = { organizationId, deviceId: MOCK_IDS.buildBox };
    const first = await client.listSandboxes({ ...filter, pageSize: 1 });
    expect(first.sandboxes.map((s) => s.sandboxId)).toEqual([
      MOCK_IDS.runningSandbox,
    ]);
    expect(first.nextPageToken).not.toBe("");
    const second = await client.listSandboxes({
      ...filter,
      pageSize: 1,
      pageToken: first.nextPageToken,
    });
    expect(second.sandboxes.map((s) => s.sandboxId)).toEqual([
      MOCK_IDS.pausedSandbox,
    ]);
    // A full page returns a token even though no row follows.
    expect(second.nextPageToken).not.toBe("");
    const third = await client.listSandboxes({
      ...filter,
      pageSize: 1,
      pageToken: second.nextPageToken,
    });
    expect(third.sandboxes).toHaveLength(0);
    expect(third.nextPageToken).toBe("");

    // The state and ended filters apply before paging.
    const ended = await client.listSandboxes({
      ...filter,
      includeEnded: true,
      pageSize: 10,
    });
    expect(ended.sandboxes.map((s) => s.sandboxId)).toEqual([
      MOCK_IDS.runningSandbox,
      MOCK_IDS.pausedSandbox,
      MOCK_IDS.deletedSandbox,
    ]);
    expect(ended.nextPageToken).toBe("");
    const paused = await client.listSandboxes({
      organizationId,
      state: SandboxState.PAUSED,
    });
    expect(paused.sandboxes.map((s) => s.sandboxId)).toEqual([
      MOCK_IDS.pausedSandbox,
    ]);
  });

  test("continues after a deleted last row and after a token that names no row", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const page = await client.listSandboxes({
      organizationId,
      includeEnded: true,
      pageToken: tokenOf(MOCK_IDS.pausedSandbox),
    });
    expect(page.sandboxes.map((s) => s.sandboxId)).toEqual([
      MOCK_IDS.deletedSandbox,
      MOCK_IDS.unreachableSandbox,
    ]);
    const none = await client.listSandboxes({
      organizationId,
      pageToken: tokenOf("0193a000-0000-7000-8000-0000000000ff"),
    });
    expect(none.sandboxes).toHaveLength(0);
    expect(none.nextPageToken).toBe("");
  });

  test("rejects a negative page size and a malformed token", async () => {
    const client = createClient(SandboxService, createMockTransport());
    expect(
      await codeOf(client.listSandboxes({ organizationId, pageSize: -1 })),
    ).toBe(Code.InvalidArgument);
    for (const pageToken of [
      "unknown",
      MOCK_IDS.runningSandbox,
      "!!!",
      tokenOf("not-a-uuid"),
      btoa(`v2:${MOCK_IDS.runningSandbox}`),
    ]) {
      expect(
        await codeOf(client.listSandboxes({ organizationId, pageToken })),
      ).toBe(Code.InvalidArgument);
    }
  });

  test("still checks the membership of the organization", async () => {
    const client = createClient(SandboxService, createMockTransport());
    expect(
      await codeOf(
        client.listSandboxes({
          organizationId: "0193a000-0000-7000-8000-0000000000f0",
        }),
      ),
    ).toBe(Code.NotFound);
  });
});

describe("mock snapshot list filters and paging", () => {
  async function withSnapshots() {
    const transport = createMockTransport();
    const client = createClient(SnapshotService, transport);
    // The seeded snapshot and this one are both from the running sandbox on
    // build-box, so the device filter of studio-mac matches neither.
    const created = await client.createSnapshot({
      organizationId,
      sandboxId: MOCK_IDS.runningSandbox,
    });
    return { client, created: created.snapshot! };
  }

  test("filters by sandbox, by device and by both", async () => {
    const { client, created } = await withSnapshots();
    const ids = async (filter: { sandboxId?: string; deviceId?: string }) =>
      (await client.listSnapshots({ organizationId, ...filter })).snapshots.map(
        (s) => s.snapshotId,
      );
    expect(await ids({})).toEqual([MOCK_IDS.snapshot, created.snapshotId]);
    expect(await ids({ sandboxId: MOCK_IDS.runningSandbox })).toEqual([
      MOCK_IDS.snapshot,
      created.snapshotId,
    ]);
    expect(await ids({ sandboxId: MOCK_IDS.pausedSandbox })).toEqual([]);
    expect(await ids({ deviceId: MOCK_IDS.buildBox })).toEqual([
      MOCK_IDS.snapshot,
      created.snapshotId,
    ]);
    expect(await ids({ deviceId: MOCK_IDS.studioMac })).toEqual([]);
    expect(
      await ids({
        sandboxId: MOCK_IDS.runningSandbox,
        deviceId: MOCK_IDS.studioMac,
      }),
    ).toEqual([]);
    expect(
      await ids({
        sandboxId: MOCK_IDS.runningSandbox,
        deviceId: MOCK_IDS.buildBox,
      }),
    ).toEqual([MOCK_IDS.snapshot, created.snapshotId]);
  });

  test("pages in ascending ID order and ends with an empty page after a full one", async () => {
    const { client, created } = await withSnapshots();
    const first = await client.listSnapshots({ organizationId, pageSize: 1 });
    expect(first.snapshots.map((s) => s.snapshotId)).toEqual([
      MOCK_IDS.snapshot,
    ]);
    expect(first.nextPageToken).toBe(tokenOf(MOCK_IDS.snapshot));
    const second = await client.listSnapshots({
      organizationId,
      pageSize: 1,
      pageToken: first.nextPageToken,
    });
    expect(second.snapshots.map((s) => s.snapshotId)).toEqual([
      created.snapshotId,
    ]);
    expect(second.nextPageToken).toBe(tokenOf(created.snapshotId));
    const third = await client.listSnapshots({
      organizationId,
      pageSize: 1,
      pageToken: second.nextPageToken,
    });
    expect(third.snapshots).toHaveLength(0);
    expect(third.nextPageToken).toBe("");
    const short = await client.listSnapshots({ organizationId, pageSize: 5 });
    expect(short.snapshots).toHaveLength(2);
    expect(short.nextPageToken).toBe("");
  });

  test("rejects a negative page size and a malformed token", async () => {
    const client = createClient(SnapshotService, createMockTransport());
    expect(
      await codeOf(client.listSnapshots({ organizationId, pageSize: -1 })),
    ).toBe(Code.InvalidArgument);
    expect(
      await codeOf(
        client.listSnapshots({ organizationId, pageToken: "unknown" }),
      ),
    ).toBe(Code.InvalidArgument);
  });

  test("still checks the membership of the organization", async () => {
    const client = createClient(SnapshotService, createMockTransport());
    expect(
      await codeOf(
        client.listSnapshots({
          organizationId: "0193a000-0000-7000-8000-0000000000f0",
        }),
      ),
    ).toBe(Code.NotFound);
  });
});
