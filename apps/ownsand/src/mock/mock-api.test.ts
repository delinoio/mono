// SPDX-License-Identifier: Apache-2.0
import { type Timestamp, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { afterEach, describe, expect, rs, test } from "@rstest/core";
import {
  BillingAccountRole,
  BillingMeter,
  BillingPhase,
  BillingService,
} from "@delino/ownsand-protos/v1/billing_pb";
import {
  AgentUpdateState,
  ApiKeyState,
  Arch,
  BaseImage,
  BillingReason,
  DeviceState,
  EnrollmentTokenState,
  ErrorDetailSchema,
  GuestOs,
  HostOs,
  Role,
  SandboxEndReason,
  SandboxState,
} from "@delino/ownsand-protos/v1/common_pb";
import { ApiKeyService } from "@delino/ownsand-protos/v1/api_key_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { MemberService } from "@delino/ownsand-protos/v1/member_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import {
  type SandboxSource,
  SandboxService,
} from "@delino/ownsand-protos/v1/sandbox_pb";
import { SnapshotService } from "@delino/ownsand-protos/v1/snapshot_pb";
import { uuidV7 } from "../uuid.ts";
import {
  createMockTransport,
  MOCK_IDS,
  MOCK_USER,
  MockBillingState,
  type MockBillingAccount,
} from "./mock-api.ts";
import { walkFixturePages } from "./walk-fixture-pages.ts";

describe("mock device list", () => {
  const organizationId = MOCK_IDS.acme;
  const removalTime = Date.UTC(2026, 0, 1);

  afterEach(() => {
    rs.useRealTimers();
  });

  test("lists every seeded device before any removal", async () => {
    const client = createClient(DeviceService, createMockTransport());
    const { devices } = await client.listDevices({ organizationId });
    const ids = devices.map((d) => d.deviceId);
    expect(ids).toContain(MOCK_IDS.buildBox);
    expect(ids).toContain(MOCK_IDS.studioMac);
  });

  test("returns the same scrubbed tombstone from remove, get and filtered list", async () => {
    const client = createClient(
      DeviceService,
      createMockTransport({ userId: "user-admin-jun" }),
    );
    const request = { organizationId, deviceId: MOCK_IDS.buildBox };
    const { device: before } = await client.getDevice(request);
    expect(before).toMatchObject({
      name: "build-box-1",
      hostOs: HostOs.LINUX,
      arch: Arch.AMD64,
      agentVersion: "1.4.2",
      updateState: AgentUpdateState.UP_TO_DATE,
      capabilities: { logicalCpuCount: 32 },
      limits: { vcpuCount: 16, vcpuOvercommitPercent: 150 },
      effectiveVcpuLimit: 24,
      effectiveVcpuLimitCount: 24n,
      allocation: { vcpuTotalCount: 10n },
    });
    expect(before?.createTime).toBeDefined();
    expect(before?.lastSeenTime).toBeDefined();
    const studio = await client.getDevice({
      organizationId,
      deviceId: MOCK_IDS.studioMac,
    });
    expect(studio.device).toMatchObject({
      name: "studio-mac",
      capabilities: { logicalCpuCount: 12 },
    });

    const { device: tombstone } = await client.removeDevice(request);
    expect(tombstone).toMatchObject({
      deviceId: before?.deviceId,
      organizationId: before?.organizationId,
      createTime: before?.createTime,
      state: DeviceState.REMOVED,
      name: "",
      hostOs: HostOs.UNSPECIFIED,
      arch: Arch.UNSPECIFIED,
      agentVersion: "",
      updateState: AgentUpdateState.UNSPECIFIED,
      limits: {
        vcpuCount: 0,
        memoryMib: 0n,
        diskGib: 0n,
        vcpuOvercommitPercent: 100,
      },
      effectiveVcpuLimit: 0,
      effectiveVcpuLimitCount: 0n,
      allocation: {
        vcpuCount: 0,
        vcpuTotalCount: 0n,
        memoryMib: 0n,
        diskGib: 0n,
        macosGuestCount: 0,
      },
    });
    expect(tombstone?.capabilities).toBeUndefined();
    expect(tombstone?.lastSeenTime).toBeUndefined();
    expect(tombstone?.pendingIdleSince).toBeUndefined();
    expect((await client.getDevice(request)).device).toEqual(tombstone);
    const removed = await client.listDevices({
      organizationId,
      state: DeviceState.REMOVED,
    });
    expect(removed.devices).toEqual([tombstone]);

    const { devices } = await client.listDevices({ organizationId });
    expect(devices).toEqual([studio.device]);
  });

  test("rejects both limits updates on a removed device", async () => {
    const client = createClient(
      DeviceService,
      createMockTransport({ userId: "user-admin-jun" }),
    );
    const request = { organizationId, deviceId: MOCK_IDS.buildBox };
    const { device } = await client.removeDevice(request);
    const limits = {
      vcpuCount: 4,
      memoryMib: 2048n,
      diskGib: 20n,
      vcpuOvercommitPercent: 125,
    };

    await expect(
      client.updateDeviceLimits({ ...request, limits }),
    ).rejects.toMatchObject({
      code: Code.NotFound,
    });
    await expect(
      client.replaceDeviceLimits({ ...request, limits }),
    ).rejects.toMatchObject({
      code: Code.NotFound,
    });
    expect((await client.getDevice(request)).device).toEqual(device);
  });

  test.each([DeviceState.UNSPECIFIED, 999 as DeviceState, -1 as DeviceState])(
    "rejects invalid device state %s",
    async (state) => {
      const client = createClient(DeviceService, createMockTransport());
      const error = await client
        .listDevices({ organizationId, state })
        .catch((e: unknown) => e);
      expect(error).toBeInstanceOf(ConnectError);
      expect((error as ConnectError).code).toBe(Code.InvalidArgument);
      expect((error as ConnectError).rawMessage).toBe(
        "invalid state: must be a device state",
      );
    },
  );

  test.each([
    { state: DeviceState.ONLINE, ids: [MOCK_IDS.buildBox] },
    { state: DeviceState.OFFLINE, ids: [MOCK_IDS.studioMac] },
    { state: DeviceState.REMOVED, ids: [] },
  ])("lists devices for valid state $state", async ({ state, ids }) => {
    const client = createClient(DeviceService, createMockTransport());
    const { devices, nextPageToken } = await client.listDevices({
      organizationId,
      state,
    });
    expect(devices.map((d) => d.deviceId)).toEqual(ids);
    expect(nextPageToken).toBe("");
  });

  test("still returns a removed device and ends its sandboxes", async () => {
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(removalTime);
    const transport = createMockTransport({ userId: "user-admin-jun" });
    const devices = createClient(DeviceService, transport);
    const sandboxes = createClient(SandboxService, transport);
    const before = await sandboxes.listSandboxes({ organizationId });
    const onDevice = before.sandboxes.filter(
      (s) =>
        s.deviceId === MOCK_IDS.buildBox && s.state !== SandboxState.DELETED,
    );
    expect(onDevice.length).toBeGreaterThan(1);
    for (const sandbox of onDevice) {
      expect(sandbox.endTime).toBeUndefined();
    }

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
      expect(sandbox?.endTime).toEqual(timestampFromMs(removalTime));
    }
  });

  test("preserves an already deleted sandbox and a sandbox on another device", async () => {
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(removalTime - 1000);
    const transport = createMockTransport({ userId: "user-admin-jun" });
    const devices = createClient(DeviceService, transport);
    const sandboxes = createClient(SandboxService, transport);
    const deleted = { organizationId, sandboxId: MOCK_IDS.runningSandbox };
    const other = { organizationId, sandboxId: MOCK_IDS.unreachableSandbox };
    const { sandbox: alreadyDeleted } = await sandboxes.deleteSandbox(deleted);
    expect(alreadyDeleted?.endTime).toEqual(
      timestampFromMs(removalTime - 1000),
    );
    const unaffected = await sandboxes.getSandbox(other);

    rs.setSystemTime(removalTime);
    await devices.removeDevice({ organizationId, deviceId: MOCK_IDS.buildBox });

    const { sandbox } = await sandboxes.getSandbox(deleted);
    expect(sandbox?.state).toBe(SandboxState.DELETED);
    expect(sandbox?.endReason).toBe(SandboxEndReason.DELETED_BY_CALLER);
    expect(sandbox?.endTime).toEqual(timestampFromMs(removalTime - 1000));
    expect(await sandboxes.getSandbox(other)).toEqual(unaffected);
  });
});

describe("mock idempotent createSandbox", () => {
  const organizationId = MOCK_IDS.acme;
  const createTime = Date.UTC(2026, 0, 1);
  const dayMs = 24 * 60 * 60 * 1000;
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

  afterEach(() => {
    rs.useRealTimers();
  });

  const timestampNs = ({ seconds, nanos }: Timestamp) =>
    seconds * 1_000_000_000n + BigInt(nanos);

  test("keeps the full subsecond deadlines on replay", async () => {
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(1_750_000_000_999);
    const client = createClient(SandboxService, createMockTransport());
    const idleTtl = { seconds: 0n, nanos: 500_000 };
    const maxTtl = { seconds: 0n, nanos: 500_000_000 };
    const params = { ...request(uuidV7()), idleTtl, maxTtl };
    const first = await client.createSandbox(params);
    const sandbox = first.sandbox!;

    expect(sandbox.idleTtl).toMatchObject(idleTtl);
    expect(sandbox.maxTtl).toMatchObject(maxTtl);
    expect(sandbox.idleDeadline).toMatchObject({
      seconds: 1_750_000_000n,
      nanos: 999_500_000,
    });
    expect(sandbox.maxDeadline).toMatchObject({
      seconds: 1_750_000_001n,
      nanos: 499_000_000,
    });
    const createdAt = timestampNs(sandbox.createTime!);
    expect(timestampNs(sandbox.idleDeadline!) - createdAt).toBe(500_000n);
    expect(timestampNs(sandbox.maxDeadline!) - createdAt).toBe(500_000_000n);

    rs.setSystemTime(Date.now() + 1_000);
    expect(await client.createSandbox(params)).toEqual(first);
  });

  test("uses one create time for equal TTLs under an advancing clock", async () => {
    let clock = createTime;
    const dateNow = rs.spyOn(Date, "now").mockImplementation(() => clock++);
    try {
      const client = createClient(SandboxService, createMockTransport());
      const ttl = { seconds: 1n };
      const { sandbox } = await client.createSandbox({
        ...request(uuidV7()),
        idleTtl: ttl,
        maxTtl: ttl,
      });

      expect(sandbox?.idleDeadline).toEqual(sandbox?.maxDeadline);
      expect(
        timestampNs(sandbox!.idleDeadline!) - timestampNs(sandbox!.createTime!),
      ).toBe(1_000_000_000n);
    } finally {
      dateNow.mockRestore();
    }
  });

  test("accepts a maximum TTL of exactly seven days", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const { sandbox } = await client.createSandbox({
      ...request(uuidV7()),
      maxTtl: { seconds: 604_800n },
    });
    const createdAt = timestampNs(sandbox!.createTime!);

    expect(timestampNs(sandbox!.idleDeadline!) - createdAt).toBe(
      600_000_000_000n,
    );
    expect(timestampNs(sandbox!.maxDeadline!) - createdAt).toBe(
      604_800_000_000_000n,
    );
  });

  test.each([
    ["missing idle TTL", { idleTtl: undefined }],
    ["missing maximum TTL", { maxTtl: undefined }],
    ["zero idle TTL", { idleTtl: { seconds: 0n } }],
    ["zero maximum TTL", { maxTtl: { seconds: 0n } }],
    ["negative idle TTL", { idleTtl: { seconds: 0n, nanos: -1 } }],
    ["negative maximum TTL", { maxTtl: { seconds: -1n } }],
    ["mixed-sign duration", { idleTtl: { seconds: 600n, nanos: -1 } }],
    [
      "noncanonical duration",
      { maxTtl: { seconds: 600n, nanos: 1_000_000_000 } },
    ],
    [
      "duration out of protobuf range",
      { maxTtl: { seconds: 315_576_000_001n } },
    ],
    ["idle TTL above maximum", { idleTtl: { seconds: 3600n, nanos: 1 } }],
    [
      "maximum TTL above seven days",
      { maxTtl: { seconds: 604_800n, nanos: 1 } },
    ],
  ] as const)(
    "rejects %s without creating a sandbox or reserving its key",
    async (_, invalid) => {
      const client = createClient(SandboxService, createMockTransport());
      const params = request(uuidV7());
      const list = await client.listSandboxes({ organizationId });

      await expect(
        client.createSandbox({ ...params, ...invalid }),
      ).rejects.toMatchObject({
        code: Code.InvalidArgument,
      });
      expect(await client.listSandboxes({ organizationId })).toEqual(list);
      expect(
        (await client.createSandbox(params)).sandbox?.sandboxId,
      ).toBeDefined();
      await expect(
        client.createSandbox({ ...params, ...invalid }),
      ).rejects.toMatchObject({
        code: Code.InvalidArgument,
      });
    },
  );

  test("a retry returns the same sandbox and the request labels", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const key = uuidV7();
    const first = await client.createSandbox(request(key));
    const retry = await client.createSandbox(request(key));

    expect(retry.sandbox?.sandboxId).toBe(first.sandbox?.sandboxId);
    expect(retry.sandbox?.labels).toEqual({ team: "ci" });
    expect(retry.sandbox?.labels).toEqual(first.sandbox?.labels);
  });

  test("a new key creates another sandbox and reads keep the labels", async () => {
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
    const listed = list.sandboxes.find(
      (s) => s.sandboxId === first.sandbox?.sandboxId,
    );
    expect(listed?.labels).toEqual({ team: "ci" });
  });

  test("a user label named like the old bookkeeping key is preserved", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const labels = { "mock-idempotency-key": "mine" };
    const key = uuidV7();
    const first = await client.createSandbox({ ...request(key), labels });
    expect(first.sandbox?.labels).toEqual(labels);
    const retry = await client.createSandbox({ ...request(key), labels });
    expect(retry.sandbox?.sandboxId).toBe(first.sandbox?.sandboxId);
    expect(retry.sandbox?.labels).toEqual(labels);
    const got = await client.getSandbox({
      organizationId,
      sandboxId: first.sandbox!.sandboxId,
    });
    expect(got.sandbox?.labels).toEqual(labels);
    const list = await client.listSandboxes({ organizationId });
    expect(
      list.sandboxes.find((s) => s.sandboxId === first.sandbox?.sandboxId)
        ?.labels,
    ).toEqual(labels);
  });

  test("a user label equal to another request's key is not a replay", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const key = uuidV7();
    const first = await client.createSandbox({
      ...request(uuidV7()),
      labels: { "mock-idempotency-key": key },
    });
    const second = await client.createSandbox(request(key));
    expect(second.sandbox?.sandboxId).not.toBe(first.sandbox?.sandboxId);
  });

  test("a known key with other parameters fails with AlreadyExists", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const key = uuidV7();
    await client.createSandbox(request(key));
    await expect(
      client.createSandbox({ ...request(key), labels: { team: "other" } }),
    ).rejects.toMatchObject({ code: Code.AlreadyExists });
  });

  test("replays until the 24-hour boundary without extending the reservation", async () => {
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(createTime);
    const client = createClient(SandboxService, createMockTransport());
    const params = request(uuidV7());
    const first = await client.createSandbox(params);

    rs.setSystemTime(createTime + dayMs - 1);
    expect(await client.createSandbox(params)).toEqual(first);
    await expect(
      client.createSandbox({ ...params, labels: { team: "other" } }),
    ).rejects.toMatchObject({ code: Code.AlreadyExists });

    rs.setSystemTime(createTime + dayMs);
    const second = await client.createSandbox(params);
    expect(second.sandbox?.sandboxId).not.toBe(first.sandbox?.sandboxId);
    expect(
      await client.getSandbox({
        organizationId,
        sandboxId: first.sandbox!.sandboxId,
      }),
    ).toMatchObject({ sandbox: first.sandbox });
  });

  test("reserves a reused key for 24 hours with the new parameters", async () => {
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(createTime);
    const client = createClient(SandboxService, createMockTransport());
    const params = request(uuidV7());
    const first = await client.createSandbox(params);
    const changed = { ...params, labels: { team: "other" } };
    const reuseTime = createTime + dayMs + 1;

    rs.setSystemTime(reuseTime);
    const second = await client.createSandbox(changed);
    expect(second.sandbox?.sandboxId).not.toBe(first.sandbox?.sandboxId);
    expect(second.sandbox?.labels).toEqual(changed.labels);
    expect(await client.createSandbox(changed)).toEqual(second);

    rs.setSystemTime(reuseTime + dayMs - 1);
    expect(await client.createSandbox(changed)).toEqual(second);
    rs.setSystemTime(reuseTime + dayMs);
    const third = await client.createSandbox(changed);
    expect(third.sandbox?.sandboxId).not.toBe(second.sandbox?.sandboxId);
  });

  test("leaves an expired key available after failed placement", async () => {
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(createTime);
    const client = createClient(SandboxService, createMockTransport());
    const params = request(uuidV7());
    const first = await client.createSandbox(params);

    rs.setSystemTime(createTime + dayMs);
    await expect(
      client.createSandbox({
        ...params,
        placement: { deviceId: uuidV7() },
      }),
    ).rejects.toMatchObject({ code: Code.NotFound });

    const second = await client.createSandbox(params);
    expect(second.sandbox?.sandboxId).not.toBe(first.sandbox?.sandboxId);
    expect(await client.createSandbox(params)).toEqual(second);
  });

  test("label order does not change the idempotency result", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const key = uuidV7();
    const first = await client.createSandbox({
      ...request(key),
      labels: { team: "ci", env: "dev" },
    });
    const retry = await client.createSandbox({
      ...request(key),
      labels: { env: "dev", team: "ci" },
    });
    expect(retry.sandbox?.sandboxId).toBe(first.sandbox?.sandboxId);
  });
});

describe("mock createSandbox placement", () => {
  const organizationId = MOCK_IDS.acme;
  const request = (deviceId?: string) => ({
    organizationId,
    idempotencyKey: uuidV7(),
    source: {
      source: { case: "baseImage" as const, value: BaseImage.LINUX },
    },
    resources: { vcpuCount: 1, memoryMib: 1024n, diskGib: 10n },
    idleTtl: { seconds: 600n },
    maxTtl: { seconds: 3600n },
    placement: deviceId === undefined ? undefined : { deviceId },
  });

  test("an explicit online device runs the sandbox", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const { sandbox } = await client.createSandbox(request(MOCK_IDS.buildBox));
    expect(sandbox?.deviceId).toBe(MOCK_IDS.buildBox);
  });

  test("automatic placement picks the first online device", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const { sandbox } = await client.createSandbox(request());
    expect(sandbox?.deviceId).toBe(MOCK_IDS.buildBox);
  });

  test("an explicit offline device fails with Unavailable and creates nothing", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const before = await client.listSandboxes({ organizationId });
    const error = await client
      .createSandbox(request(MOCK_IDS.studioMac))
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ConnectError);
    expect((error as ConnectError).code).toBe(Code.Unavailable);
    const after = await client.listSandboxes({ organizationId });
    expect(after.sandboxes).toHaveLength(before.sandboxes.length);
  });

  test("an unknown device fails with NotFound and creates nothing", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const before = await client.listSandboxes({ organizationId });
    const error = await client
      .createSandbox(request(uuidV7()))
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ConnectError);
    expect((error as ConnectError).code).toBe(Code.NotFound);
    const after = await client.listSandboxes({ organizationId });
    expect(after.sandboxes).toHaveLength(before.sandboxes.length);
  });

  test("a removed device fails with NotFound and creates nothing", async () => {
    const transport = createMockTransport();
    const devices = createClient(DeviceService, transport);
    const client = createClient(SandboxService, transport);
    await devices.removeDevice({ organizationId, deviceId: MOCK_IDS.buildBox });
    const before = await client.listSandboxes({ organizationId });
    await expect(
      client.createSandbox(request(MOCK_IDS.buildBox)),
    ).rejects.toMatchObject({ code: Code.NotFound });
    const after = await client.listSandboxes({ organizationId });
    expect(after.sandboxes).toHaveLength(before.sandboxes.length);
  });

  test("a failed explicit placement does not store the idempotency key", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const key = uuidV7();
    await expect(
      client.createSandbox({
        ...request(MOCK_IDS.studioMac),
        idempotencyKey: key,
      }),
    ).rejects.toMatchObject({ code: Code.Unavailable });
    // The same key with another placement is a new request, not a replay.
    const { sandbox } = await client.createSandbox({
      ...request(MOCK_IDS.buildBox),
      idempotencyKey: key,
    });
    expect(sandbox?.deviceId).toBe(MOCK_IDS.buildBox);
  });
});

describe("mock createSandbox base image placement", () => {
  const organizationId = MOCK_IDS.acme;
  const request = (image: BaseImage, deviceId?: string) => ({
    organizationId,
    idempotencyKey: uuidV7(),
    source: { source: { case: "baseImage" as const, value: image } },
    resources: { vcpuCount: 1, memoryMib: 1024n, diskGib: 10n },
    idleTtl: { seconds: 600n },
    maxTtl: { seconds: 3600n },
    placement: deviceId === undefined ? undefined : { deviceId },
  });
  // Another build box that runs every guest, next to the Linux-only seeded one.
  const allGuests = {
    [MOCK_IDS.studioMac]: {
      guestOses: [GuestOs.LINUX, GuestOs.MACOS, GuestOs.WINDOWS],
      builtBaseImages: [BaseImage.LINUX, BaseImage.MACOS, BaseImage.WINDOWS],
    },
  };

  test.each([BaseImage.MACOS, BaseImage.WINDOWS])(
    "the default fixture has no online device for base image %s",
    async (image) => {
      const client = createClient(SandboxService, createMockTransport());
      const before = await client.listSandboxes({ organizationId });
      await expect(client.createSandbox(request(image))).rejects.toMatchObject({
        code: Code.FailedPrecondition,
      });
      await expect(
        client.createSandbox(request(image, MOCK_IDS.buildBox)),
      ).rejects.toMatchObject({ code: Code.FailedPrecondition });
      const after = await client.listSandboxes({ organizationId });
      expect(after.sandboxes).toHaveLength(before.sandboxes.length);
    },
  );

  test("a rejected create stores no idempotency key", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const key = uuidV7();
    await expect(
      client.createSandbox({
        ...request(BaseImage.WINDOWS),
        idempotencyKey: key,
      }),
    ).rejects.toMatchObject({ code: Code.FailedPrecondition });
    // The same key with another request is a new request, not a conflict.
    const { sandbox } = await client.createSandbox({
      ...request(BaseImage.LINUX),
      idempotencyKey: key,
    });
    expect(sandbox?.guestOs).toBe(GuestOs.LINUX);
  });

  test("a Linux create is unchanged", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const { sandbox } = await client.createSandbox(request(BaseImage.LINUX));
    expect(sandbox?.deviceId).toBe(MOCK_IDS.buildBox);
    expect(sandbox?.guestOs).toBe(GuestOs.LINUX);
  });

  test("a device needs both the guest and the built image", async () => {
    // The online build box advertises the Windows guest but has not built the
    // image, then the other way round; neither can run a Windows sandbox.
    for (const caps of [
      { guestOses: [GuestOs.LINUX, GuestOs.WINDOWS] },
      { builtBaseImages: [BaseImage.LINUX, BaseImage.WINDOWS] },
    ]) {
      const client = createClient(
        SandboxService,
        createMockTransport({
          deviceCapabilities: { [MOCK_IDS.buildBox]: caps },
        }),
      );
      await expect(
        client.createSandbox(request(BaseImage.WINDOWS)),
      ).rejects.toMatchObject({ code: Code.FailedPrecondition });
      await expect(
        client.createSandbox(request(BaseImage.WINDOWS, MOCK_IDS.buildBox)),
      ).rejects.toMatchObject({ code: Code.FailedPrecondition });
    }
    const client = createClient(
      SandboxService,
      createMockTransport({
        deviceCapabilities: {
          [MOCK_IDS.buildBox]: {
            guestOses: [GuestOs.LINUX, GuestOs.WINDOWS],
            builtBaseImages: [BaseImage.LINUX, BaseImage.WINDOWS],
          },
        },
      }),
    );
    const { sandbox } = await client.createSandbox(request(BaseImage.WINDOWS));
    expect(sandbox?.deviceId).toBe(MOCK_IDS.buildBox);
  });

  test("automatic placement ignores an offline device that can run the image", async () => {
    // Only the offline studio-mac advertises Windows; it must not host it.
    const client = createClient(
      SandboxService,
      createMockTransport({ deviceCapabilities: allGuests }),
    );
    await expect(
      client.createSandbox(request(BaseImage.WINDOWS)),
    ).rejects.toMatchObject({ code: Code.FailedPrecondition });
  });

  test("an explicit incompatible device fails even when another device could run it", async () => {
    // build-box-1 is online and runs Windows; the explicit Linux-only device
    // must not hand the request over to it.
    const client = createClient(
      SandboxService,
      createMockTransport({
        deviceCapabilities: {
          [MOCK_IDS.buildBox]: {
            guestOses: [GuestOs.LINUX, GuestOs.WINDOWS],
            builtBaseImages: [BaseImage.LINUX, BaseImage.WINDOWS],
          },
          [MOCK_IDS.studioMac]: {
            guestOses: [GuestOs.LINUX],
            builtBaseImages: [BaseImage.LINUX],
          },
        },
      }),
    );
    await expect(
      client.createSandbox(request(BaseImage.WINDOWS, MOCK_IDS.studioMac)),
    ).rejects.toMatchObject({ code: Code.FailedPrecondition });
    const { sandbox } = await client.createSandbox(request(BaseImage.WINDOWS));
    expect(sandbox?.deviceId).toBe(MOCK_IDS.buildBox);
  });

  test("an explicit offline device that can run the image is Unavailable", async () => {
    const client = createClient(
      SandboxService,
      createMockTransport({ deviceCapabilities: allGuests }),
    );
    await expect(
      client.createSandbox(request(BaseImage.MACOS, MOCK_IDS.studioMac)),
    ).rejects.toMatchObject({ code: Code.Unavailable });
  });
});

describe("mock sandbox guest OS", () => {
  const organizationId = MOCK_IDS.acme;
  const request = (source: Parameters<typeof sourceOf>[0], diskGib = 10n) => ({
    organizationId,
    idempotencyKey: uuidV7(),
    source: sourceOf(source),
    resources: { vcpuCount: 1, memoryMib: 1024n, diskGib },
    idleTtl: { seconds: 600n },
    maxTtl: { seconds: 3600n },
  });
  function sourceOf(
    s:
      | { baseImage: BaseImage }
      | { snapshotId: string }
      | { templateId: string }
      | undefined,
  ) {
    if (s === undefined) return undefined;
    if ("baseImage" in s)
      return { source: { case: "baseImage" as const, value: s.baseImage } };
    if ("snapshotId" in s)
      return { source: { case: "snapshotId" as const, value: s.snapshotId } };
    return { source: { case: "templateId" as const, value: s.templateId } };
  }
  const capable = {
    [MOCK_IDS.buildBox]: {
      guestOses: [GuestOs.LINUX, GuestOs.MACOS, GuestOs.WINDOWS],
      builtBaseImages: [BaseImage.LINUX, BaseImage.MACOS, BaseImage.WINDOWS],
    },
  };

  test.each([
    [BaseImage.LINUX, GuestOs.LINUX],
    [BaseImage.MACOS, GuestOs.MACOS],
    [BaseImage.WINDOWS, GuestOs.WINDOWS],
  ])("a sandbox of base image %s runs guest %s", async (image, guestOs) => {
    const client = createClient(
      SandboxService,
      createMockTransport({ deviceCapabilities: capable }),
    );
    const { sandbox } = await client.createSandbox(
      request({ baseImage: image }),
    );
    expect(sandbox?.guestOs).toBe(guestOs);
  });

  test.each([
    [BaseImage.MACOS, GuestOs.MACOS],
    [BaseImage.WINDOWS, GuestOs.WINDOWS],
  ])(
    "a snapshot, a template and a sandbox from them keep guest %s",
    async (image, guestOs) => {
      const transport = createMockTransport({ deviceCapabilities: capable });
      const sandboxes = createClient(SandboxService, transport);
      const snapshots = createClient(SnapshotService, transport);
      const { sandbox } = await sandboxes.createSandbox(
        request({ baseImage: image }),
      );
      const { snapshot } = await snapshots.createSnapshot({
        organizationId,
        sandboxId: sandbox!.sandboxId,
      });
      expect(snapshot?.guestOs).toBe(guestOs);

      const fromSnapshot = await sandboxes.createSandbox(
        request({ snapshotId: snapshot!.snapshotId }),
      );
      expect(fromSnapshot.sandbox?.guestOs).toBe(guestOs);

      const { template } = await snapshots.createTemplate({
        organizationId,
        snapshotId: snapshot!.snapshotId,
        name: "guest-template",
      });
      const fromTemplate = await sandboxes.createSandbox(
        request({ templateId: template!.templateId }),
      );
      expect(fromTemplate.sandbox?.guestOs).toBe(guestOs);
    },
  );

  test("the seeded snapshot and template keep the Linux guest", async () => {
    const client = createClient(SandboxService, createMockTransport());
    const a = await client.createSandbox(
      request({ snapshotId: MOCK_IDS.snapshot }, 20n),
    );
    const b = await client.createSandbox(
      request({ templateId: MOCK_IDS.template }, 20n),
    );
    expect(a.sandbox?.guestOs).toBe(GuestOs.LINUX);
    expect(b.sandbox?.guestOs).toBe(GuestOs.LINUX);
  });

  test("an unknown snapshot or template, or no source, is rejected", async () => {
    const client = createClient(SandboxService, createMockTransport());
    await expect(
      client.createSandbox(request({ snapshotId: uuidV7() })),
    ).rejects.toMatchObject({ code: Code.NotFound });
    await expect(
      client.createSandbox(request({ templateId: uuidV7() })),
    ).rejects.toMatchObject({ code: Code.NotFound });
    await expect(
      client.createSandbox(request(undefined)),
    ).rejects.toMatchObject({ code: Code.InvalidArgument });
    await expect(
      client.createSandbox(request({ baseImage: BaseImage.UNSPECIFIED })),
    ).rejects.toMatchObject({ code: Code.InvalidArgument });
  });
});

describe("mock linkable billing account pagination", () => {
  const organizationId = MOCK_IDS.acme;

  function account(index: number, role = BillingAccountRole.OWNER) {
    return {
      billingAccountId: `0193a000-0000-7000-8000-${index.toString(16).padStart(12, "0")}`,
      name: `Billing fixture ${index}`,
      role,
      balanceUsdMicros: 1_000_000n,
      subscriptionActive: true,
    };
  }

  function client(linkableBillingAccounts: MockBillingAccount[]) {
    return createClient(
      BillingService,
      createMockTransport({ linkableBillingAccounts }),
    );
  }

  test("orders eligible accounts by ID and resumes after the last account", async () => {
    const owner = account(1);
    const admin = account(3, BillingAccountRole.ADMIN);
    const billing = client([
      admin,
      account(2, BillingAccountRole.VIEWER),
      owner,
    ]);
    const request = { organizationId, pageSize: 1 };

    const first = await billing.listLinkableBillingAccounts(request);
    expect(first.billingAccounts).toMatchObject([
      { billingAccountId: owner.billingAccountId, role: owner.role },
    ]);
    expect(first.nextPageToken).not.toBe("");

    const last = await billing.listLinkableBillingAccounts({
      ...request,
      pageToken: first.nextPageToken,
    });
    expect(last.billingAccounts).toMatchObject([
      { billingAccountId: admin.billingAccountId, role: admin.role },
    ]);
    expect(last.nextPageToken).toBe("");
  });

  test("defaults to 50 and caps the page at 200", async () => {
    const accounts = Array.from({ length: 201 }, (_, i) => account(i + 1));
    const billing = client(accounts);
    const ids = accounts.map((a) => a.billingAccountId);

    for (const pageSize of [undefined, 0]) {
      const page = await billing.listLinkableBillingAccounts({
        organizationId,
        pageSize,
      });
      expect(page.billingAccounts.map((a) => a.billingAccountId)).toEqual(
        ids.slice(0, 50),
      );
      expect(page.nextPageToken).not.toBe("");
    }

    const capped = await billing.listLinkableBillingAccounts({
      organizationId,
      pageSize: 500,
    });
    expect(capped.billingAccounts.map((a) => a.billingAccountId)).toEqual(
      ids.slice(0, 200),
    );
    expect(capped.nextPageToken).not.toBe("");

    const last = await billing.listLinkableBillingAccounts({
      organizationId,
      pageSize: 200,
      pageToken: capped.nextPageToken,
    });
    expect(last.billingAccounts.map((a) => a.billingAccountId)).toEqual(
      ids.slice(200),
    );
    expect(last.nextPageToken).toBe("");
  });

  test("omits the cursor when a full page has no eligible account after it", async () => {
    const accounts = Array.from({ length: 200 }, (_, i) => account(i + 1));
    const page = await client([
      ...accounts,
      account(201, BillingAccountRole.VIEWER),
    ]).listLinkableBillingAccounts({ organizationId, pageSize: 200 });
    expect(page.billingAccounts).toHaveLength(200);
    expect(page.nextPageToken).toBe("");
  });

  test("rejects negative sizes and invalid tokens for an authorized caller", async () => {
    const first = account(1);
    const billing = client([first, account(2)]);
    const organizations = createClient(
      OrganizationService,
      createMockTransport(),
    );
    const otherList = await organizations.listOrganizations({ pageSize: 1 });
    expect(otherList.nextPageToken).not.toBe("");

    const otherOwner = createClient(
      BillingService,
      createMockTransport({
        userId: "user-owner-lee",
        linkableBillingAccounts: [first, account(2)],
      }),
    );
    const otherCaller = await otherOwner.listLinkableBillingAccounts({
      organizationId: MOCK_IDS.northwind,
      pageSize: 1,
    });
    expect(otherCaller.nextPageToken).not.toBe("");

    await expect(
      billing.listLinkableBillingAccounts({ organizationId, pageSize: -1 }),
    ).rejects.toMatchObject({ code: Code.InvalidArgument });
    for (const pageToken of [
      "not-a-token",
      first.billingAccountId,
      otherList.nextPageToken,
      otherCaller.nextPageToken,
    ]) {
      await expect(
        billing.listLinkableBillingAccounts({ organizationId, pageToken }),
      ).rejects.toMatchObject({ code: Code.InvalidArgument });
    }
  });
});

describe("mock createSandbox restore restrictions", () => {
  const organizationId = MOCK_IDS.acme;
  const baseImage = { case: "baseImage", value: BaseImage.LINUX } as const;
  const request = (source: SandboxSource["source"], deviceId?: string) => ({
    organizationId,
    source: { source },
    idempotencyKey: uuidV7(),
    resources: { vcpuCount: 2, memoryMib: 4096n, diskGib: 20n },
    idleTtl: { seconds: 600n },
    maxTtl: { seconds: 3600n },
    placement: deviceId === undefined ? undefined : { deviceId },
  });

  async function twoLinuxDevices() {
    const transport = createMockTransport({ extraDevices: 1 });
    const devices = createClient(DeviceService, transport);
    const list = await devices.listDevices({
      organizationId,
      state: DeviceState.ONLINE,
    });
    const other = list.devices.find(
      (device) =>
        device.deviceId !== MOCK_IDS.buildBox &&
        device.hostOs === HostOs.LINUX &&
        device.arch === Arch.AMD64,
    );
    return {
      sandboxes: createClient(SandboxService, transport),
      snapshots: createClient(SnapshotService, transport),
      otherDeviceId: other!.deviceId,
    };
  }

  test.each([
    { case: "snapshotId", value: MOCK_IDS.snapshot },
    { case: "templateId", value: MOCK_IDS.template },
  ] as const)(
    "enforces the disk minimum for $case before placement",
    async (source) => {
      const sandboxes = createClient(SandboxService, createMockTransport());
      const restore = request(source, MOCK_IDS.buildBox);
      const before = await sandboxes.listSandboxes({ organizationId });

      await expect(
        sandboxes.createSandbox({
          ...restore,
          resources: { ...restore.resources, diskGib: 19n },
        }),
      ).rejects.toMatchObject({
        code: Code.InvalidArgument,
        rawMessage:
          "invalid resources.disk_gib: must be at least the snapshot's disk size",
      });
      const after = await sandboxes.listSandboxes({ organizationId });
      expect(after.sandboxes).toHaveLength(before.sandboxes.length);

      const equal = await sandboxes.createSandbox(restore);
      expect(equal.sandbox).toMatchObject({
        deviceId: MOCK_IDS.buildBox,
        state: SandboxState.RUNNING,
        resources: { diskGib: 20n },
      });
      const replay = await sandboxes.createSandbox(restore);
      expect(replay.sandbox?.sandboxId).toBe(equal.sandbox?.sandboxId);

      const larger = await sandboxes.createSandbox({
        ...request(source, MOCK_IDS.buildBox),
        resources: { ...restore.resources, diskGib: 21n },
      });
      expect(larger.sandbox?.resources?.diskGib).toBe(21n);
    },
  );

  test("a base-image sandbox can use a smaller disk", async () => {
    const sandboxes = createClient(SandboxService, createMockTransport());
    const base = request(baseImage, MOCK_IDS.buildBox);
    const { sandbox } = await sandboxes.createSandbox({
      ...base,
      resources: { ...base.resources, diskGib: 19n },
    });
    expect(sandbox).toMatchObject({
      deviceId: MOCK_IDS.buildBox,
      state: SandboxState.RUNNING,
      resources: { diskGib: 19n },
    });
  });

  test.each([
    { case: "snapshotId", value: MOCK_IDS.snapshot },
    { case: "templateId", value: MOCK_IDS.template },
  ] as const)(
    "rejects explicit non-holder placement for $case",
    async (source) => {
      const { sandboxes, otherDeviceId } = await twoLinuxDevices();
      const restore = request(source, otherDeviceId);
      const before = await sandboxes.listSandboxes({ organizationId });

      await expect(sandboxes.createSandbox(restore)).rejects.toMatchObject({
        code: Code.FailedPrecondition,
        rawMessage: "the snapshot is not on this device and is not exported",
      });
      const after = await sandboxes.listSandboxes({ organizationId });
      expect(after.sandboxes).toHaveLength(before.sandboxes.length);

      const corrected = {
        ...restore,
        placement: { deviceId: MOCK_IDS.buildBox },
      };
      const { sandbox } = await sandboxes.createSandbox(corrected);
      expect(sandbox).toMatchObject({
        deviceId: MOCK_IDS.buildBox,
        state: SandboxState.RUNNING,
      });
      const replay = await sandboxes.createSandbox(corrected);
      expect(replay.sandbox?.sandboxId).toBe(sandbox?.sandboxId);
    },
  );

  test("automatic placement skips non-holders until the snapshot is exported", async () => {
    const { sandboxes, snapshots, otherDeviceId } = await twoLinuxDevices();
    const { sandbox } = await sandboxes.createSandbox(
      request(baseImage, otherDeviceId),
    );
    const { snapshot } = await snapshots.createSnapshot({
      organizationId,
      sandboxId: sandbox!.sandboxId,
    });
    const snapshotId = snapshot!.snapshotId;
    const { template } = await snapshots.createTemplate({
      organizationId,
      snapshotId,
      name: "second-device-template",
    });
    const sources = [
      { case: "snapshotId", value: snapshotId },
      { case: "templateId", value: template!.templateId },
    ] as const;
    for (const source of sources) {
      const restore = await sandboxes.createSandbox(request(source));
      expect(restore.sandbox?.deviceId).toBe(otherDeviceId);
    }

    await snapshots.exportSnapshot({ organizationId, snapshotId });
    for (const source of sources) {
      const restore = await sandboxes.createSandbox(
        request(source, MOCK_IDS.buildBox),
      );
      expect(restore.sandbox?.deviceId).toBe(MOCK_IDS.buildBox);
    }
  });
});

describe("mock billing", () => {
  const organizationId = MOCK_IDS.acme;
  const ADMIN = "user-admin-jun";
  const MEMBER = "user-member-ada";
  const billing = (state: MockBillingState, userId?: string) =>
    createClient(
      BillingService,
      createMockTransport({ userId, billing: { [organizationId]: state } }),
    );
  const codeOf = async (call: Promise<unknown>) =>
    ((await call.catch((e: unknown) => e)) as ConnectError).code;

  test("seeds each billing state", async () => {
    const cases = [
      [
        MockBillingState.AllowedLinked,
        BillingPhase.ALLOWED,
        BillingReason.UNSPECIFIED,
        true,
      ],
      [
        MockBillingState.BlockedUnlinked,
        BillingPhase.BLOCKED,
        BillingReason.ACCOUNT_REQUIRED,
        false,
      ],
      [
        MockBillingState.BlockedLinked,
        BillingPhase.BLOCKED,
        BillingReason.BLOCKED,
        true,
      ],
      [
        MockBillingState.Suspended,
        BillingPhase.SUSPENDED,
        BillingReason.SUSPENDED,
        true,
      ],
    ] as const;
    for (const [state, phase, reason, linked] of cases) {
      const res = await billing(state, MEMBER).getBillingStatus({
        organizationId,
      });
      expect(res.billing?.phase).toBe(phase);
      expect(res.billing?.reason).toBe(reason);
      expect(res.billing?.linked).toBe(linked);
      expect(res.billing?.suspendTime === undefined).toBe(
        phase === BillingPhase.ALLOWED,
      );
    }
  });

  test("returns this month's usage for every meter in order, with debt", async () => {
    const res = await billing(
      MockBillingState.BlockedLinked,
      ADMIN,
    ).getBillingUsage({ organizationId });
    expect(res.balanceUsdMicros).toBeLessThan(0n);
    expect(res.meterUsages.map((u) => u.meter)).toEqual([
      BillingMeter.RELAY_EGRESS,
      BillingMeter.R2_STORAGE,
      BillingMeter.CLASS_A_RPC,
      BillingMeter.CLASS_B_RPC,
    ]);
    expect(
      res.meterUsages.some(
        (u) => u.amountUsdMicros > 0n && u.amountUsdMicros < 10_000n,
      ),
    ).toBe(true);
  });

  test("applies the role matrix", async () => {
    const state = MockBillingState.AllowedLinked;
    expect(
      await codeOf(billing(state, MEMBER).getBillingUsage({ organizationId })),
    ).toBe(Code.PermissionDenied);
    expect(
      await codeOf(
        billing(state, ADMIN).listLinkableBillingAccounts({ organizationId }),
      ),
    ).toBe(Code.PermissionDenied);
    expect(
      await codeOf(
        billing(state, ADMIN).unlinkBillingAccount({ organizationId }),
      ),
    ).toBe(Code.PermissionDenied);
    expect(
      await codeOf(
        billing(state, "user-stranger").getBillingStatus({ organizationId }),
      ),
    ).toBe(Code.NotFound);
  });

  test("fails usage without a link", async () => {
    expect(
      await codeOf(
        billing(MockBillingState.BlockedUnlinked).getBillingUsage({
          organizationId,
        }),
      ),
    ).toBe(Code.FailedPrecondition);
  });

  test("links, rejects another link and unlinks into a block at once", async () => {
    const client = billing(MockBillingState.BlockedUnlinked);
    const before = await client.getBillingStatus({ organizationId });
    const { billingAccounts } = await client.listLinkableBillingAccounts({
      organizationId,
    });
    expect(billingAccounts.length).toBeGreaterThan(0);
    const linked = await client.linkBillingAccount({
      organizationId,
      billingAccountId: MOCK_IDS.acmeBillingAccount,
    });
    expect(linked.billing?.phase).toBe(BillingPhase.ALLOWED);
    expect(linked.billingAccountName).toBe("Acme Robotics billing");
    expect(
      await codeOf(
        client.linkBillingAccount({
          organizationId,
          billingAccountId: MOCK_IDS.labBillingAccount,
        }),
      ),
    ).toBe(Code.AlreadyExists);
    const unlinked = await client.unlinkBillingAccount({ organizationId });
    expect(unlinked.billing?.phase).toBe(BillingPhase.BLOCKED);
    expect(unlinked.billing?.reason).toBe(BillingReason.ACCOUNT_REQUIRED);
    // The new block starts at the unlink, after the earlier one ended.
    expect(unlinked.billing!.blockedSince!.seconds).toBeGreaterThan(
      before.billing!.blockedSince!.seconds,
    );
  });

  test("keeps an earlier block start when a blocked organization unlinks", async () => {
    const client = billing(MockBillingState.BlockedLinked);
    const before = await client.getBillingStatus({ organizationId });
    const after = await client.unlinkBillingAccount({ organizationId });
    expect(after.billing?.reason).toBe(BillingReason.ACCOUNT_REQUIRED);
    expect(after.billing?.blockedSince).toEqual(before.billing?.blockedSince);
  });

  test("an ownership transfer removes the link", async () => {
    const transport = createMockTransport();
    await createClient(OrganizationService, transport).transferOwnership({
      organizationId,
      userId: ADMIN,
    });
    const res = await createClient(BillingService, transport).getBillingStatus({
      organizationId,
    });
    expect(res.billing?.linked).toBe(false);
    expect(res.billing?.reason).toBe(BillingReason.ACCOUNT_REQUIRED);
  });

  test("rejects resource creation of a blocked organization with a billing reason", async () => {
    const transport = createMockTransport({
      billing: { [organizationId]: MockBillingState.BlockedLinked },
    });
    const error = (await createClient(SandboxService, transport)
      .resumeSandbox({ organizationId, sandboxId: MOCK_IDS.pausedSandbox })
      .catch((e: unknown) => e)) as ConnectError;
    expect(error.code).toBe(Code.ResourceExhausted);
    expect(error.rawMessage).toContain("https://accounts.delino.io");
    expect(error.findDetails(ErrorDetailSchema)[0]?.billingReason).toBe(
      BillingReason.BLOCKED,
    );
  });
});

describe("mock organization list paging", () => {
  async function withExtras(count: number) {
    const client = createClient(OrganizationService, createMockTransport());
    for (let i = 0; i < count; i++) {
      await client.createOrganization({ name: `Extra ${i}` });
    }
    return client;
  }

  test("returns the first 50 rows by default and a token after a full page", async () => {
    const client = await withExtras(60);
    const first = await client.listOrganizations({});
    expect(first.organizations).toHaveLength(50);
    expect(first.nextPageToken).not.toBe("");
    const second = await client.listOrganizations({
      pageToken: first.nextPageToken,
    });
    // 2 seeded and 60 created organizations.
    expect(second.organizations).toHaveLength(12);
    expect(second.nextPageToken).toBe("");
  });

  test("walks every page once, in order, with the caller's role", async () => {
    const client = await withExtras(4);
    const ids = await walkFixturePages(6, 2, async (request) => {
      const page = await client.listOrganizations(request);
      return {
        rows: page.organizations.map((o) => o.organizationId),
        nextPageToken: page.nextPageToken,
      };
    });
    const all = await client.listOrganizations({ pageSize: 200 });
    expect(ids).toEqual(all.organizations.map((o) => o.organizationId));
    expect(new Set(ids).size).toBe(6);
    expect(
      all.organizations.find((o) => o.organizationId === MOCK_IDS.northwind)!
        .callerRole,
    ).toBe(Role.MEMBER);
  });

  test("returns a token and then an empty last page when the rows fill the page exactly", async () => {
    const client = createClient(OrganizationService, createMockTransport());
    const first = await client.listOrganizations({ pageSize: 2 });
    expect(first.organizations).toHaveLength(2);
    expect(first.nextPageToken).not.toBe("");
    const last = await client.listOrganizations({
      pageSize: 2,
      pageToken: first.nextPageToken,
    });
    expect(last.organizations).toHaveLength(0);
    expect(last.nextPageToken).toBe("");
  });

  test("rejects a token that names no row", async () => {
    const client = createClient(OrganizationService, createMockTransport());
    const error = await client
      .listOrganizations({ pageToken: "unknown" })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ConnectError);
    expect((error as ConnectError).code).toBe(Code.InvalidArgument);
  });
});

describe("mock updateMemberRole", () => {
  const organizationId = MOCK_IDS.acme;
  const ada = "user-member-ada";

  const rolesOf = async (client: ReturnType<typeof memberClient>) => {
    const { members } = await client.listMembers({ organizationId });
    return members.map((m) => [m.userId, m.role]);
  };
  const memberClient = (transport = createMockTransport()) =>
    createClient(MemberService, transport);

  test("rejects an omitted role before the member lookup", async () => {
    const client = memberClient();
    const before = await rolesOf(client);
    const error = await client
      .updateMemberRole({ organizationId, userId: ada })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ConnectError);
    expect((error as ConnectError).code).toBe(Code.InvalidArgument);
    expect(await rolesOf(client)).toEqual(before);
  });

  for (const [name, role] of [
    ["an unspecified role", Role.UNSPECIFIED],
    ["an unknown numeric role", 99 as Role],
    ["the owner role", Role.OWNER],
  ] as const) {
    test(`rejects ${name} and changes nothing`, async () => {
      const client = memberClient();
      const before = await rolesOf(client);
      const error = await client
        .updateMemberRole({ organizationId, userId: ada, role })
        .catch((e: unknown) => e);
      expect(error).toBeInstanceOf(ConnectError);
      expect((error as ConnectError).code).toBe(Code.InvalidArgument);
      expect(await rolesOf(client)).toEqual(before);
    });
  }

  test("validates the role before it looks the member up", async () => {
    const error = await memberClient()
      .updateMemberRole({
        organizationId,
        userId: "user-unknown",
        role: Role.OWNER,
      })
      .catch((e: unknown) => e);
    expect((error as ConnectError).code).toBe(Code.InvalidArgument);
  });

  test("still changes a member between admin and member", async () => {
    const client = memberClient();
    const promoted = await client.updateMemberRole({
      organizationId,
      userId: ada,
      role: Role.ADMIN,
    });
    expect(promoted.member?.role).toBe(Role.ADMIN);
    const demoted = await client.updateMemberRole({
      organizationId,
      userId: ada,
      role: Role.MEMBER,
    });
    expect(demoted.member?.role).toBe(Role.MEMBER);
    const { members } = await client.listMembers({ organizationId });
    expect(members.find((m) => m.userId === ada)?.role).toBe(Role.MEMBER);
  });

  test("a valid role for the current owner is PermissionDenied", async () => {
    const client = memberClient();
    const before = await rolesOf(client);
    const error = await client
      .updateMemberRole({
        organizationId,
        userId: MOCK_USER,
        role: Role.MEMBER,
      })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ConnectError);
    expect((error as ConnectError).code).toBe(Code.PermissionDenied);
    expect(await rolesOf(client)).toEqual(before);
  });
});

describe("mock removeMember", () => {
  const organizationId = MOCK_IDS.acme;
  const admin = "user-admin-jun";
  const member = "user-member-ada";
  const unknown = "user-unknown";

  const memberClient = (userId: string) =>
    createClient(MemberService, createMockTransport({ userId }));

  for (const [role, caller] of [
    ["owner", MOCK_USER],
    ["admin", admin],
  ] as const) {
    test(`rejects an unknown target for an ${role} without changing members`, async () => {
      const client = memberClient(caller);
      const before = await client.listMembers({ organizationId });

      await expect(
        client.removeMember({ organizationId, userId: unknown }),
      ).rejects.toMatchObject({
        code: Code.NotFound,
        rawMessage: "member not found",
      });
      expect(await client.listMembers({ organizationId })).toEqual(before);
    });

    test(`rejects an empty target for an ${role} without changing members`, async () => {
      const client = memberClient(caller);
      const before = await client.listMembers({ organizationId });

      await expect(
        client.removeMember({ organizationId, userId: "" }),
      ).rejects.toMatchObject({
        code: Code.InvalidArgument,
        rawMessage: "invalid user_id: is required",
      });
      expect(await client.listMembers({ organizationId })).toEqual(before);
    });
  }

  test("checks member permissions before validating or looking up the target", async () => {
    const client = memberClient(member);
    for (const userId of [unknown, ""]) {
      await expect(
        client.removeMember({ organizationId, userId }),
      ).rejects.toMatchObject({ code: Code.PermissionDenied });
    }
  });

  test("removes an existing member", async () => {
    const client = memberClient(MOCK_USER);
    const { members } = await client.listMembers({ organizationId });
    expect(members.map((entry) => entry.userId)).toContain(member);

    await client.removeMember({ organizationId, userId: member });

    expect((await client.listMembers({ organizationId })).members).toEqual(
      members.filter((entry) => entry.userId !== member),
    );
  });

  test("allows a member to leave the organization", async () => {
    const client = memberClient(member);

    await client.removeMember({ organizationId, userId: member });

    await expect(client.listMembers({ organizationId })).rejects.toMatchObject({
      code: Code.NotFound,
    });
  });

  test("blocks the owner from leaving", async () => {
    await expect(
      memberClient(MOCK_USER).removeMember({
        organizationId,
        userId: MOCK_USER,
      }),
    ).rejects.toMatchObject({ code: Code.FailedPrecondition });
  });

  test("blocks an admin from removing the owner", async () => {
    await expect(
      memberClient(admin).removeMember({ organizationId, userId: MOCK_USER }),
    ).rejects.toMatchObject({
      code: Code.PermissionDenied,
      rawMessage: "the owner cannot be removed",
    });
  });
});

describe("mock enrollment token expiry", () => {
  const organizationId = MOCK_IDS.acme;
  const T0 = Date.UTC(2026, 0, 1, 12, 0, 0);

  afterEach(() => {
    rs.useRealTimers();
  });

  const createShortToken = async (
    client: ReturnType<typeof createClient<typeof DeviceService>>,
    seconds: number,
  ) => {
    const { token } = await client.createEnrollmentToken({
      organizationId,
      ttl: { seconds: BigInt(seconds) },
    });
    return token!.enrollmentTokenId;
  };
  const stateOf = async (
    client: ReturnType<typeof createClient<typeof DeviceService>>,
    id: string,
  ) => {
    const { tokens } = await client.listEnrollmentTokens({ organizationId });
    return tokens.find((t) => t.enrollmentTokenId === id)?.state;
  };

  test("lists a one-nanosecond token as pending until the clock advances", async () => {
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(T0 + 750);
    const client = createClient(DeviceService, createMockTransport());

    const { token } = await client.createEnrollmentToken({
      organizationId,
      ttl: { nanos: 1 },
    });

    expect(token?.expireTime).toMatchObject({
      seconds: BigInt(T0 / 1000),
      nanos: 750_000_001,
    });
    expect(await stateOf(client, token!.enrollmentTokenId)).toBe(
      EnrollmentTokenState.PENDING,
    );
    rs.setSystemTime(T0 + 751);
    expect(await stateOf(client, token!.enrollmentTokenId)).toBe(
      EnrollmentTokenState.EXPIRED,
    );
  });

  test("uses one creation time when the clock advances during a request", async () => {
    let clock = T0;
    const dateNow = rs.spyOn(Date, "now").mockImplementation(() => clock++);
    try {
      const client = createClient(DeviceService, createMockTransport());

      const { token } = await client.createEnrollmentToken({
        organizationId,
        ttl: { seconds: 1n, nanos: 500_000_000 },
      });

      const { createTime, expireTime } = token!;
      const elapsedNanos =
        (expireTime!.seconds - createTime!.seconds) * 1_000_000_000n +
        BigInt(expireTime!.nanos - createTime!.nanos);
      expect(elapsedNanos).toBe(1_500_000_000n);
    } finally {
      dateNow.mockRestore();
    }
  });

  test("checks the caller's role and billing before validating the TTL", async () => {
    const billing = { [organizationId]: MockBillingState.BlockedUnlinked };
    const member = createClient(
      DeviceService,
      createMockTransport({ userId: "user-member-ada", billing }),
    );
    const owner = createClient(DeviceService, createMockTransport({ billing }));
    const request = { organizationId, ttl: { seconds: 0n } };
    const before = await owner.listEnrollmentTokens({ organizationId });

    await expect(member.createEnrollmentToken(request)).rejects.toMatchObject({
      code: Code.PermissionDenied,
    });
    await expect(owner.createEnrollmentToken(request)).rejects.toMatchObject({
      code: Code.ResourceExhausted,
    });
    expect(await owner.listEnrollmentTokens({ organizationId })).toEqual(
      before,
    );
  });

  test("an unused token is PENDING before expiry and EXPIRED at and after it", async () => {
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(T0);
    const client = createClient(DeviceService, createMockTransport());
    const id = await createShortToken(client, 1);

    expect(await stateOf(client, id)).toBe(EnrollmentTokenState.PENDING);
    rs.setSystemTime(T0 + 999);
    expect(await stateOf(client, id)).toBe(EnrollmentTokenState.PENDING);
    rs.setSystemTime(T0 + 1000);
    expect(await stateOf(client, id)).toBe(EnrollmentTokenState.EXPIRED);
    rs.setSystemTime(T0 + 2000);
    expect(await stateOf(client, id)).toBe(EnrollmentTokenState.EXPIRED);
  });

  test("the list returns a copy and keeps the stored record and the order", async () => {
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(T0);
    const client = createClient(DeviceService, createMockTransport());
    const first = await createShortToken(client, 1);
    const second = await createShortToken(client, 3600);
    const idsOf = async () =>
      (await client.listEnrollmentTokens({ organizationId })).tokens.map(
        (t) => t.enrollmentTokenId,
      );
    const before = await idsOf();
    // The control plane lists in ascending ID order, so new tokens come last.
    expect(before.slice(-2)).toEqual([first, second]);

    rs.setSystemTime(T0 + 2000);
    expect(await idsOf()).toEqual(before);
    expect(await stateOf(client, first)).toBe(EnrollmentTokenState.EXPIRED);
    expect(await stateOf(client, second)).toBe(EnrollmentTokenState.PENDING);

    // The stored record is still pending, so the clock moving back shows it.
    rs.setSystemTime(T0);
    expect(await stateOf(client, first)).toBe(EnrollmentTokenState.PENDING);
  });

  test("a used and a revoked token keep their state after expiry", async () => {
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(T0);
    const client = createClient(DeviceService, createMockTransport());
    const revoked = await createShortToken(client, 1);
    await client.revokeEnrollmentToken({
      organizationId,
      enrollmentTokenId: revoked,
    });
    const used = (
      await client.listEnrollmentTokens({ organizationId })
    ).tokens.find((t) => t.state === EnrollmentTokenState.USED);
    expect(used).toBeDefined();

    rs.setSystemTime(T0 + 3 * 24 * 3600 * 1000);
    expect(await stateOf(client, revoked)).toBe(EnrollmentTokenState.REVOKED);
    expect(await stateOf(client, used!.enrollmentTokenId)).toBe(
      EnrollmentTokenState.USED,
    );
  });
});

describe("mock revokeEnrollmentToken", () => {
  const organizationId = MOCK_IDS.acme;

  test("revokes a pending token and a repeated revoke returns it again", async () => {
    const client = createClient(DeviceService, createMockTransport());
    const { token } = await client.createEnrollmentToken({
      organizationId,
      ttl: { seconds: 3600n },
    });
    const enrollmentTokenId = token!.enrollmentTokenId;
    expect(token?.state).toBe(EnrollmentTokenState.PENDING);

    const first = await client.revokeEnrollmentToken({
      organizationId,
      enrollmentTokenId,
    });
    expect(first.token?.state).toBe(EnrollmentTokenState.REVOKED);
    expect(first.token?.enrollmentTokenId).toBe(enrollmentTokenId);

    const second = await client.revokeEnrollmentToken({
      organizationId,
      enrollmentTokenId,
    });
    expect(second.token?.state).toBe(EnrollmentTokenState.REVOKED);
    expect(second.token?.enrollmentTokenId).toBe(enrollmentTokenId);

    const { tokens } = await client.listEnrollmentTokens({ organizationId });
    expect(
      tokens.filter((t) => t.enrollmentTokenId === enrollmentTokenId),
    ).toHaveLength(1);
    expect(
      tokens.find((t) => t.enrollmentTokenId === enrollmentTokenId)?.state,
    ).toBe(EnrollmentTokenState.REVOKED);
  });

  test("a used token still fails with FailedPrecondition and stays used", async () => {
    const client = createClient(DeviceService, createMockTransport());
    const { tokens } = await client.listEnrollmentTokens({ organizationId });
    const used = tokens.find((t) => t.state === EnrollmentTokenState.USED);
    expect(used).toBeDefined();

    const error = await client
      .revokeEnrollmentToken({
        organizationId,
        enrollmentTokenId: used!.enrollmentTokenId,
      })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ConnectError);
    expect((error as ConnectError).code).toBe(Code.FailedPrecondition);

    const after = await client.listEnrollmentTokens({ organizationId });
    expect(
      after.tokens.find((t) => t.enrollmentTokenId === used!.enrollmentTokenId)
        ?.state,
    ).toBe(EnrollmentTokenState.USED);
  });

  test("an unknown token is NotFound", async () => {
    const client = createClient(DeviceService, createMockTransport());
    const error = await client
      .revokeEnrollmentToken({
        organizationId,
        enrollmentTokenId: uuidV7(),
      })
      .catch((e: unknown) => e);
    expect((error as ConnectError).code).toBe(Code.NotFound);
  });
});

describe("mock enrollment token list paging", () => {
  const organizationId = MOCK_IDS.acme;

  /** The client of a mock whose organization has `count` more tokens than the seeded one. */
  async function withTokens(count: number) {
    const client = createClient(DeviceService, createMockTransport());
    const ids: string[] = [];
    for (let i = 0; i < count; i++) {
      const { token } = await client.createEnrollmentToken({
        organizationId,
        ttl: { seconds: 3600n },
      });
      ids.push(token!.enrollmentTokenId);
    }
    return { client, ids };
  }

  test("returns the first 50 rows by default and a token after a full page", async () => {
    const { client } = await withTokens(60);
    const first = await client.listEnrollmentTokens({ organizationId });
    expect(first.tokens).toHaveLength(50);
    expect(first.nextPageToken).not.toBe("");
    const second = await client.listEnrollmentTokens({
      organizationId,
      pageToken: first.nextPageToken,
    });
    // One seeded and 60 created tokens.
    expect(second.tokens).toHaveLength(11);
    expect(second.nextPageToken).toBe("");
  });

  test("walks every page once, in creation order, and lists a new token last", async () => {
    const { client, ids } = await withTokens(5);
    const walked = await walkFixturePages(6, 2, async (request) => {
      const page = await client.listEnrollmentTokens({
        organizationId,
        ...request,
      });
      return {
        rows: page.tokens.map((t) => t.enrollmentTokenId),
        nextPageToken: page.nextPageToken,
      };
    });
    expect(walked).toHaveLength(6);
    expect(new Set(walked).size).toBe(6);
    expect(walked.slice(1)).toEqual(ids);
  });

  test("caps a page at 200 rows", async () => {
    const { client } = await withTokens(205);
    const page = await client.listEnrollmentTokens({
      organizationId,
      pageSize: 1000,
    });
    expect(page.tokens).toHaveLength(200);
    expect(page.nextPageToken).not.toBe("");
  });

  test("returns an empty last page after a page that fills exactly", async () => {
    const { client } = await withTokens(1);
    const first = await client.listEnrollmentTokens({
      organizationId,
      pageSize: 2,
    });
    expect(first.tokens).toHaveLength(2);
    expect(first.nextPageToken).not.toBe("");
    const last = await client.listEnrollmentTokens({
      organizationId,
      pageSize: 2,
      pageToken: first.nextPageToken,
    });
    expect(last.tokens).toHaveLength(0);
    expect(last.nextPageToken).toBe("");
  });

  test("rejects a token that names no row", async () => {
    const client = createClient(DeviceService, createMockTransport());
    const error = await client
      .listEnrollmentTokens({ organizationId, pageToken: "unknown" })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ConnectError);
    expect((error as ConnectError).code).toBe(Code.InvalidArgument);
  });
});

describe("mock API key revocation", () => {
  const organizationId = MOCK_IDS.acme;

  afterEach(() => {
    rs.useRealTimers();
  });

  test("keeps the first revocation time in a retry and a later list", async () => {
    const client = createClient(ApiKeyService, createMockTransport());
    const { apiKey } = await client.createApiKey({
      organizationId,
      name: "revocation test",
    });
    expect(apiKey?.state).toBe(ApiKeyState.ACTIVE);
    const apiKeyId = apiKey!.apiKeyId;
    const firstTime = Date.UTC(2026, 0, 1);
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(firstTime);

    const first = await client.revokeApiKey({ organizationId, apiKeyId });
    expect(first.apiKey).toMatchObject({
      state: ApiKeyState.REVOKED,
      revokeTime: timestampFromMs(firstTime),
    });

    rs.setSystemTime(firstTime + 1_000);
    const retry = await client.revokeApiKey({ organizationId, apiKeyId });
    expect(retry.apiKey).toMatchObject({
      state: ApiKeyState.REVOKED,
      revokeTime: timestampFromMs(firstTime),
    });
    const { apiKeys } = await client.listApiKeys({ organizationId });
    expect(
      apiKeys.find((key) => key.apiKeyId === apiKeyId)?.revokeTime,
    ).toEqual(timestampFromMs(firstTime));
  });

  test("returns NotFound when an owner revokes an unknown key", async () => {
    const owner = createClient(ApiKeyService, createMockTransport());
    await expect(
      owner.revokeApiKey({ organizationId, apiKeyId: uuidV7() }),
    ).rejects.toMatchObject({ code: Code.NotFound });
  });

  test("rejects a member revoking a seeded key", async () => {
    const owner = createClient(ApiKeyService, createMockTransport());
    const member = createClient(
      ApiKeyService,
      createMockTransport({ userId: "user-member-ada" }),
    );
    const { apiKeys } = await owner.listApiKeys({ organizationId });
    await expect(
      member.revokeApiKey({ organizationId, apiKeyId: apiKeys[0]!.apiKeyId }),
    ).rejects.toMatchObject({ code: Code.PermissionDenied });
  });
});

describe("mock API key list paging", () => {
  const organizationId = MOCK_IDS.acme;

  async function withKeys(count: number) {
    const client = createClient(ApiKeyService, createMockTransport());
    const ids: string[] = [];
    for (let i = 0; i < count; i++) {
      const { apiKey } = await client.createApiKey({
        organizationId,
        name: `key-${i}`,
      });
      ids.push(apiKey!.apiKeyId);
    }
    return { client, ids };
  }

  test("returns the first 50 rows by default and a token after a full page", async () => {
    const { client } = await withKeys(60);
    const first = await client.listApiKeys({ organizationId });
    expect(first.apiKeys).toHaveLength(50);
    expect(first.nextPageToken).not.toBe("");
    const second = await client.listApiKeys({
      organizationId,
      pageToken: first.nextPageToken,
    });
    // One seeded and 60 created keys.
    expect(second.apiKeys).toHaveLength(11);
    expect(second.nextPageToken).toBe("");
  });

  test("walks every page once, in creation order, and lists a new key last", async () => {
    const { client, ids } = await withKeys(5);
    const walked = await walkFixturePages(6, 2, async (request) => {
      const page = await client.listApiKeys({
        organizationId,
        ...request,
      });
      return {
        rows: page.apiKeys.map((k) => k.apiKeyId),
        nextPageToken: page.nextPageToken,
      };
    });
    expect(walked).toHaveLength(6);
    expect(new Set(walked).size).toBe(6);
    expect(walked.slice(1)).toEqual(ids);
  });

  test("caps a page at 200 rows", async () => {
    const { client } = await withKeys(205);
    const page = await client.listApiKeys({ organizationId, pageSize: 1000 });
    expect(page.apiKeys).toHaveLength(200);
    expect(page.nextPageToken).not.toBe("");
  });

  test("rejects a token that names no row", async () => {
    const client = createClient(ApiKeyService, createMockTransport());
    const error = await client
      .listApiKeys({ organizationId, pageToken: "unknown" })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ConnectError);
    expect((error as ConnectError).code).toBe(Code.InvalidArgument);
  });
});

describe("mock seeded extra list rows", () => {
  const organizationId = MOCK_IDS.acme;

  test("adds unused enrollment tokens after the seeded one and pages them", async () => {
    const client = createClient(
      DeviceService,
      createMockTransport({ extraEnrollmentTokens: 201 }),
    );
    const first = await client.listEnrollmentTokens({
      organizationId,
      pageSize: 200,
    });
    expect(first.tokens).toHaveLength(200);
    expect(first.tokens[0]!.state).toBe(EnrollmentTokenState.USED);
    expect(first.nextPageToken).not.toBe("");
    const second = await client.listEnrollmentTokens({
      organizationId,
      pageSize: 200,
      pageToken: first.nextPageToken,
    });
    // One seeded and 201 added tokens.
    expect(second.tokens).toHaveLength(2);
    expect(second.nextPageToken).toBe("");
    expect(
      new Set([...first.tokens, ...second.tokens].map((t) => t.tokenPrefix))
        .size,
    ).toBe(202);
    expect(
      second.tokens.every((t) => t.state === EnrollmentTokenState.PENDING),
    ).toBe(true);
    // A token that a request creates is listed after the added ones.
    const { token } = await client.createEnrollmentToken({
      organizationId,
      ttl: { seconds: 3600n },
    });
    const last = await client.listEnrollmentTokens({
      organizationId,
      pageSize: 200,
      pageToken: first.nextPageToken,
    });
    expect(last.tokens.at(-1)!.enrollmentTokenId).toBe(
      token!.enrollmentTokenId,
    );
  });

  test("adds active API keys after the seeded one and pages them", async () => {
    const client = createClient(
      ApiKeyService,
      createMockTransport({ extraApiKeys: 201 }),
    );
    const first = await client.listApiKeys({ organizationId, pageSize: 200 });
    expect(first.apiKeys).toHaveLength(200);
    expect(first.apiKeys[0]!.name).toBe("ci-runner");
    expect(first.apiKeys[1]!.name).toBe("key-0");
    const second = await client.listApiKeys({
      organizationId,
      pageSize: 200,
      pageToken: first.nextPageToken,
    });
    // One seeded and 201 added keys.
    expect(second.apiKeys.map((k) => k.name)).toEqual(["key-199", "key-200"]);
    expect(second.nextPageToken).toBe("");
  });

  test("seeds no extra rows by default", async () => {
    const mock = createMockTransport();
    const tokens = await createClient(DeviceService, mock).listEnrollmentTokens(
      { organizationId },
    );
    const keys = await createClient(ApiKeyService, mock).listApiKeys({
      organizationId,
    });
    expect(tokens.tokens).toHaveLength(1);
    expect(keys.apiKeys).toHaveLength(1);
  });
});
