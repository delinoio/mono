// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { afterEach, describe, expect, rs, test } from "@rstest/core";
import {
  BillingMeter,
  BillingPhase,
  BillingService,
} from "@delino/ownsand-protos/v1/billing_pb";
import {
  BaseImage,
  BillingReason,
  DeviceState,
  EnrollmentTokenState,
  ErrorDetailSchema,
  GuestOs,
  Role,
  SandboxEndReason,
  SandboxState,
} from "@delino/ownsand-protos/v1/common_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { MemberService } from "@delino/ownsand-protos/v1/member_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { SnapshotService } from "@delino/ownsand-protos/v1/snapshot_pb";
import { uuidV7 } from "../uuid.ts";
import {
  createMockTransport,
  MOCK_IDS,
  MOCK_USER,
  MockBillingState,
} from "./mock-api.ts";

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
  const request = (source: Parameters<typeof sourceOf>[0]) => ({
    organizationId,
    idempotencyKey: uuidV7(),
    source: sourceOf(source),
    resources: { vcpuCount: 1, memoryMib: 1024n, diskGib: 10n },
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
      request({ snapshotId: MOCK_IDS.snapshot }),
    );
    const b = await client.createSandbox(
      request({ templateId: MOCK_IDS.template }),
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
    const ids: string[] = [];
    let pageToken = "";
    do {
      const page = await client.listOrganizations({ pageSize: 2, pageToken });
      ids.push(...page.organizations.map((o) => o.organizationId));
      pageToken = page.nextPageToken;
    } while (pageToken !== "");
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
    expect(before.slice(0, 2)).toEqual([second, first]);

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
