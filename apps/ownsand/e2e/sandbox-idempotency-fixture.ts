// SPDX-License-Identifier: Apache-2.0
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { type Code, ConnectError, createClient } from "@connectrpc/connect";
import { BaseImage } from "@delino/ownsand-protos/v1/common_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { createMockTransport, MOCK_IDS } from "../src/mock/mock-api.ts";

const client = createClient(SandboxService, createMockTransport());
const organizationId = MOCK_IDS.acme;
const createRequest = {
  organizationId,
  source: { source: { case: "baseImage" as const, value: BaseImage.LINUX } },
  resources: { vcpuCount: 1, memoryMib: 1024n, diskGib: 10n },
};

async function createSandbox(
  timeMs: number,
  team: string,
  deviceId: string | null,
): Promise<{ id: string; team: string } | { code: Code }> {
  const realNow = Date.now;
  Date.now = () => timeMs;
  try {
    const { sandbox } = await client.createSandbox({
      ...createRequest,
      idempotencyKey: "0193a000-0000-7000-8000-0000000000f1",
      idleTtl: { seconds: 600n },
      maxTtl: { seconds: 3600n },
      labels: { team },
      placement: deviceId === null ? undefined : { deviceId },
    });
    if (!sandbox) throw new Error("CreateSandbox returned no sandbox");
    return { id: sandbox.sandboxId, team: sandbox.labels.team ?? "" };
  } catch (error) {
    if (error instanceof ConnectError) return { code: error.code };
    throw error;
  } finally {
    Date.now = realNow;
  }
}

const timestampNs = ({ seconds, nanos }: Timestamp) =>
  seconds * 1_000_000_000n + BigInt(nanos);

async function createSandboxWithTtls(
  clockMs: number,
  advanceMs: number,
  idleNanos: number,
  maxNanos: number,
): Promise<
  | { id: string; created: string; idleOffset: number; maxOffset: number }
  | { code: Code }
> {
  const realNow = Date.now;
  Date.now = () => {
    const current = clockMs;
    clockMs += advanceMs;
    return current;
  };
  try {
    const { sandbox } = await client.createSandbox({
      ...createRequest,
      idempotencyKey: "0193a000-0000-7000-8000-0000000000f3",
      idleTtl: {
        seconds: BigInt(Math.floor(idleNanos / 1e9)),
        nanos: idleNanos % 1e9,
      },
      maxTtl: {
        seconds: BigInt(Math.floor(maxNanos / 1e9)),
        nanos: maxNanos % 1e9,
      },
    });
    if (!sandbox?.createTime || !sandbox.idleDeadline || !sandbox.maxDeadline) {
      throw new Error("CreateSandbox returned no timestamps");
    }
    const created = timestampNs(sandbox.createTime);
    return {
      id: sandbox.sandboxId,
      created: created.toString(),
      idleOffset: Number(timestampNs(sandbox.idleDeadline) - created),
      maxOffset: Number(timestampNs(sandbox.maxDeadline) - created),
    };
  } catch (error) {
    if (error instanceof ConnectError) return { code: error.code };
    throw error;
  } finally {
    Date.now = realNow;
  }
}

async function listSandboxIds() {
  const { sandboxes } = await client.listSandboxes({ organizationId });
  return sandboxes.map((sandbox) => sandbox.sandboxId);
}

declare global {
  interface Window {
    createSandboxForIdempotencyTest: typeof createSandbox;
    createSandboxWithTtls: typeof createSandboxWithTtls;
    listSandboxIds: typeof listSandboxIds;
  }
}

window.createSandboxForIdempotencyTest = createSandbox;
window.createSandboxWithTtls = createSandboxWithTtls;
window.listSandboxIds = listSandboxIds;
