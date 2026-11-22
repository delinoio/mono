// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import {
  BillingMeter,
  BillingPhase,
  BillingService,
} from "@delino/ownsand-protos/v1/billing_pb";
import {
  BaseImage,
  BillingReason,
  DeviceState,
  ErrorDetailSchema,
  SandboxEndReason,
  SandboxState,
} from "@delino/ownsand-protos/v1/common_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { uuidV7 } from "../uuid.ts";
import { createMockTransport, MOCK_IDS, MockBillingState } from "./mock-api.ts";

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
