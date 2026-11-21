// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import {
  BaseImage,
  DeviceState,
  SandboxEndReason,
  SandboxState,
} from "ownsand-sdk/gen/common_pb";
import { DeviceService } from "ownsand-sdk/gen/device_pb";
import { SandboxService } from "ownsand-sdk/gen/sandbox_pb";
import { uuidV7 } from "../uuid.ts";
import { createMockTransport, MOCK_IDS } from "./mock-api.ts";

describe("mock device list", () => {
  const organizationId = MOCK_IDS.acme;

  test("lists every seeded device before any removal", async () => {
    const client = createClient(DeviceService, createMockTransport());
    const { devices } = await client.listDevices({ organizationId });
    const ids = devices.map((d) => d.deviceId);
    expect(ids).toContain(MOCK_IDS.buildBox);
    expect(ids).toContain(MOCK_IDS.studioMac);
  });

  test("omits removed devices unless the state filter asks for them", async () => {
    const client = createClient(DeviceService, createMockTransport());
    await client.removeDevice({ organizationId, deviceId: MOCK_IDS.buildBox });

    const all = await client.listDevices({ organizationId });
    expect(all.devices.map((d) => d.deviceId)).not.toContain(MOCK_IDS.buildBox);
    expect(all.devices.every((d) => d.state !== DeviceState.REMOVED)).toBe(
      true,
    );

    const removed = await client.listDevices({
      organizationId,
      state: DeviceState.REMOVED,
    });
    expect(removed.devices.map((d) => d.deviceId)).toEqual([MOCK_IDS.buildBox]);
  });

  test("rejects an explicit unspecified state", async () => {
    const client = createClient(DeviceService, createMockTransport());
    const error = await client
      .listDevices({ organizationId, state: DeviceState.UNSPECIFIED })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ConnectError);
    expect((error as ConnectError).code).toBe(Code.InvalidArgument);
    expect((error as ConnectError).rawMessage).toBe(
      "invalid state: must be a device state",
    );
  });

  test("still returns a removed device and ends its sandboxes", async () => {
    const transport = createMockTransport();
    const devices = createClient(DeviceService, transport);
    const sandboxes = createClient(SandboxService, transport);
    const before = await sandboxes.listSandboxes({ organizationId });
    const onDevice = before.sandboxes.filter(
      (s) =>
        s.deviceId === MOCK_IDS.buildBox && s.state !== SandboxState.DELETED,
    );
    expect(onDevice.length).toBeGreaterThan(0);

    await devices.removeDevice({ organizationId, deviceId: MOCK_IDS.buildBox });

    const { device } = await devices.getDevice({
      organizationId,
      deviceId: MOCK_IDS.buildBox,
    });
    expect(device?.state).toBe(DeviceState.REMOVED);
    for (const s of onDevice) {
      const { sandbox } = await sandboxes.getSandbox({
        organizationId,
        sandboxId: s.sandboxId,
      });
      expect(sandbox?.state).toBe(SandboxState.DELETED);
      expect(sandbox?.endReason).toBe(SandboxEndReason.DEVICE_REMOVED);
    }
  });
});

describe("mock idempotent createSandbox", () => {
  const organizationId = MOCK_IDS.acme;
  const request = (idempotencyKey: string) => ({
    organizationId,
    idempotencyKey,
    source: {
      source: { case: "baseImage" as const, value: BaseImage.LINUX },
    },
    resources: { vcpuCount: 1, memoryMib: 1024n, diskGib: 10n },
    idleTtl: { seconds: 600n },
    maxTtl: { seconds: 3600n },
    labels: { team: "ci" },
  });

  test("a retry returns the same sandbox and the request labels", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const key = uuidV7();
    const first = await client.createSandbox(request(key));
    const retry = await client.createSandbox(request(key));

    expect(retry.sandbox?.sandboxId).toBe(first.sandbox?.sandboxId);
    expect(retry.sandbox?.labels).toEqual({ team: "ci" });
    expect(retry.sandbox?.labels).toEqual(first.sandbox?.labels);
    expect(retry.sandbox?.labels).not.toHaveProperty("mock-idempotency-key");
  });

  test("a new key creates another sandbox and reads hide the key", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const first = await client.createSandbox(request(uuidV7()));
    const second = await client.createSandbox(request(uuidV7()));
    expect(second.sandbox?.sandboxId).not.toBe(first.sandbox?.sandboxId);
    expect(second.sandbox?.labels).toEqual({ team: "ci" });

    const got = await client.getSandbox({
      organizationId,
      sandboxId: first.sandbox!.sandboxId,
    });
    expect(got.sandbox?.labels).toEqual({ team: "ci" });
    const list = await client.listSandboxes({ organizationId });
    for (const s of list.sandboxes) {
      expect(s.labels).not.toHaveProperty("mock-idempotency-key");
    }
  });
});
