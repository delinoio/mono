// SPDX-License-Identifier: Apache-2.0
import { Code, createClient } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { BaseImage } from "@delino/ownsand-protos/v1/common_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { createMockTransport, MOCK_IDS } from "./mock-api.ts";

// Fixed keys with hex letters, so the upper-case spelling always differs.
const KEY_A = "0193a000-0000-7000-8000-00000000fa01";
const KEY_B = "0193a000-0000-7000-8000-00000000fa02";
const KEY_C = "0193a000-0000-7000-8000-00000000fa03";
const KEY_D = "0193a000-0000-7000-8000-00000000fa04";
const KEY_E = "0193a000-0000-7000-8000-00000000fa05";

describe("mock createSandbox UUID case variants", () => {
  const organizationId = MOCK_IDS.acme;
  const request = (idempotencyKey: string, deviceId?: string) => ({
    organizationId,
    idempotencyKey,
    source: {
      source: { case: "baseImage" as const, value: BaseImage.LINUX },
    },
    resources: { vcpuCount: 1, memoryMib: 1024n, diskGib: 10n },
    idleTtl: { seconds: 600n },
    maxTtl: { seconds: 3600n },
    labels: { team: "ci" },
    placement: deviceId === undefined ? undefined : { deviceId },
  });

  test("a key in another hex case replays one sandbox", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const key = KEY_A;
    const before = await client.listSandboxes({ organizationId });
    const first = await client.createSandbox(request(key.toLowerCase()));
    const replay = await client.createSandbox(request(key.toUpperCase()));
    expect(replay.sandbox?.sandboxId).toBe(first.sandbox?.sandboxId);
    const after = await client.listSandboxes({ organizationId });
    expect(after.sandboxes.length).toBe(before.sandboxes.length + 1);
  });

  test("an organization and device in uppercase name the same records", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const key = KEY_B;
    const upper = MOCK_IDS.buildBox.toUpperCase();
    expect(upper).not.toBe(MOCK_IDS.buildBox);
    const first = await client.createSandbox({
      ...request(key, upper),
      organizationId: organizationId.toUpperCase(),
    });
    const replay = await client.createSandbox(request(key, MOCK_IDS.buildBox));
    expect(first.sandbox?.organizationId).toBe(organizationId);
    expect(first.sandbox?.deviceId).toBe(MOCK_IDS.buildBox);
    expect(replay.sandbox?.sandboxId).toBe(first.sandbox?.sandboxId);
    expect(replay.sandbox?.deviceId).toBe(MOCK_IDS.buildBox);
  });

  test("a changed request under a case variant of the key fails", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const key = KEY_C;
    const first = await client.createSandbox(request(key.toLowerCase()));
    await expect(
      client.createSandbox({
        ...request(key.toUpperCase()),
        labels: { team: "qa" },
      }),
    ).rejects.toMatchObject({ code: Code.AlreadyExists });
    const got = await client.getSandbox({
      organizationId,
      sandboxId: first.sandbox!.sandboxId,
    });
    expect(got.sandbox?.labels).toEqual({ team: "ci" });
  });

  test("a different key still creates another sandbox", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const first = await client.createSandbox(request(KEY_D));
    const second = await client.createSandbox(request(KEY_E.toUpperCase()));
    expect(second.sandbox?.sandboxId).not.toBe(first.sandbox?.sandboxId);
    expect(second.sandbox?.labels).toEqual({ team: "ci" });
  });

  test("a snapshot ID in uppercase names the same snapshot", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const upper = MOCK_IDS.snapshot.toUpperCase();
    expect(upper).not.toBe(MOCK_IDS.snapshot);
    const { sandbox } = await client.createSandbox({
      ...request(KEY_A),
      resources: { vcpuCount: 1, memoryMib: 1024n, diskGib: 20n },
      source: { source: { case: "snapshotId" as const, value: upper } },
    });
    expect(sandbox?.source?.source).toEqual({
      case: "snapshotId",
      value: MOCK_IDS.snapshot,
    });
  });
});
