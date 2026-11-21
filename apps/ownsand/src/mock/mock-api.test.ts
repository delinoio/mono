// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import {
  DeviceState,
  SandboxEndReason,
  SandboxState,
} from "ownsand-sdk/gen/common_pb";
import { DeviceService } from "ownsand-sdk/gen/device_pb";
import { SandboxService } from "ownsand-sdk/gen/sandbox_pb";
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
