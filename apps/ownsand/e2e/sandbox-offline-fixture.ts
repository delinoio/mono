// SPDX-License-Identifier: Apache-2.0
import { clone, equals, isFieldSet } from "@bufbuild/protobuf";
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { ConnectError, createClient } from "@connectrpc/connect";
import { SandboxState } from "@delino/ownsand-protos/v1/common_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import {
  type Sandbox,
  SandboxSchema,
  SandboxService,
} from "@delino/ownsand-protos/v1/sandbox_pb";
import { createMockTransport, MOCK_IDS } from "../src/mock/mock-api.ts";

async function tryOfflineSandboxTransition(
  method: "pauseSandbox" | "resumeSandbox",
) {
  const client = createClient(SandboxService, createMockTransport());
  const request = {
    organizationId: MOCK_IDS.acme,
    sandboxId: MOCK_IDS.unreachableSandbox,
  };
  const { sandbox } = await client.getSandbox(request);
  if (!sandbox) throw new Error("GetSandbox returned no sandbox");
  const before = clone(SandboxSchema, sandbox);

  try {
    await client[method](request);
  } catch (error) {
    if (!(error instanceof ConnectError)) throw error;
    const { sandbox: after } = await client.getSandbox(request);
    return {
      code: error.code,
      message: error.rawMessage,
      state: before.state,
      lastKnownState: before.lastKnownState ?? null,
      recordUnchanged:
        after !== undefined && equals(SandboxSchema, before, after),
    };
  }
  throw new Error(`${method} unexpectedly succeeded`);
}

async function removeOfflineSandboxDevice() {
  const transport = createMockTransport({ userId: "user-admin-jun" });
  const devices = createClient(DeviceService, transport);
  const sandboxes = createClient(SandboxService, transport);
  const organizationId = MOCK_IDS.acme;
  const sandboxId = MOCK_IDS.unreachableSandbox;
  const request = { organizationId, sandboxId };

  const { sandbox: before } = await sandboxes.getSandbox(request);
  if (!before) throw new Error("GetSandbox returned no sandbox before removal");
  const { state, lastKnownState } = before;

  const startedAt = Date.now();
  await devices.removeDevice({ organizationId, deviceId: MOCK_IDS.studioMac });
  const finishedAt = Date.now();

  const { sandbox } = await sandboxes.getSandbox(request);
  const includeEnded = await sandboxes.listSandboxes({
    organizationId,
    includeEnded: true,
  });
  const onlyDeleted = await sandboxes.listSandboxes({
    organizationId,
    state: SandboxState.DELETED,
  });
  const defaults = await sandboxes.listSandboxes({ organizationId });
  const findSeededSandbox = (s: Sandbox) => s.sandboxId === sandboxId;
  const readEnd = (record: Sandbox | undefined) => {
    if (!record) return null;
    return {
      state: record.state,
      endReason: record.endReason ?? null,
      endTimeMs: record.endTime ? timestampMs(record.endTime) : null,
      hasLastKnownState: isFieldSet(record, SandboxSchema.field.lastKnownState),
    };
  };

  return {
    before: { state, lastKnownState: lastKnownState ?? null },
    startedAt,
    finishedAt,
    detail: readEnd(sandbox),
    includeEnded: readEnd(includeEnded.sandboxes.find(findSeededSandbox)),
    onlyDeleted: readEnd(onlyDeleted.sandboxes.find(findSeededSandbox)),
    inDefaultList: defaults.sandboxes.some(findSeededSandbox),
  };
}

declare global {
  interface Window {
    tryOfflineSandboxTransition: typeof tryOfflineSandboxTransition;
    removeOfflineSandboxDevice: typeof removeOfflineSandboxDevice;
  }
}

window.tryOfflineSandboxTransition = tryOfflineSandboxTransition;
window.removeOfflineSandboxDevice = removeOfflineSandboxDevice;
