// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { SnapshotService } from "@delino/ownsand-protos/v1/snapshot_pb";
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
