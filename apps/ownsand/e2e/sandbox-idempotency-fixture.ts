// SPDX-License-Identifier: Apache-2.0
import { type Code, ConnectError, createClient } from "@connectrpc/connect";
import { BaseImage } from "@delino/ownsand-protos/v1/common_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { createMockTransport, MOCK_IDS } from "../src/mock/mock-api.ts";

const client = createClient(SandboxService, createMockTransport());

async function createSandbox(
  timeMs: number,
  team: string,
  deviceId: string | null,
): Promise<{ id: string; team: string } | { code: Code }> {
  const realNow = Date.now;
  Date.now = () => timeMs;
  try {
    const { sandbox } = await client.createSandbox({
      organizationId: MOCK_IDS.acme,
      idempotencyKey: "0193a000-0000-7000-8000-0000000000f1",
      source: { source: { case: "baseImage", value: BaseImage.LINUX } },
      resources: { vcpuCount: 1, memoryMib: 1024n, diskGib: 10n },
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

declare global {
  interface Window {
    createSandboxForIdempotencyTest: typeof createSandbox;
  }
}

window.createSandboxForIdempotencyTest = createSandbox;
