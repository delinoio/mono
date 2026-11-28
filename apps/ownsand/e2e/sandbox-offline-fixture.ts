// SPDX-License-Identifier: Apache-2.0
import { clone, equals } from "@bufbuild/protobuf";
import { ConnectError, createClient } from "@connectrpc/connect";
import {
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

declare global {
  interface Window {
    tryOfflineSandboxTransition: typeof tryOfflineSandboxTransition;
  }
}

window.tryOfflineSandboxTransition = tryOfflineSandboxTransition;
