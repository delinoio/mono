// SPDX-License-Identifier: Apache-2.0
import {
  createClient,
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import { SandboxState } from "@delino/ownsand-protos/v1/common_pb";
import {
  type Sandbox,
  SandboxService,
} from "@delino/ownsand-protos/v1/sandbox_pb";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

export const lastKnownStateCases = [
  {
    name: "unmapped",
    // Protobuf preserves an enum number that this client does not recognize.
    lastKnownState: 99 as SandboxState,
    label: "Unreachable",
  },
  { name: "absent", lastKnownState: undefined, label: "Unreachable" },
  {
    name: "running",
    lastKnownState: SandboxState.RUNNING,
    label: "Unreachable (last running)",
  },
  {
    name: "unspecified",
    lastKnownState: SandboxState.UNSPECIFIED,
    label: "Unreachable (last unknown)",
  },
] as const;

export function withLastKnownState(
  base: Transport,
  lastKnownState: SandboxState | undefined,
): Transport {
  const client = createClient(SandboxService, base);
  const { getSandbox, listSandboxes } = SandboxService.method;
  const update = (sandbox: Sandbox): Sandbox =>
    sandbox.sandboxId === MOCK_IDS.unreachableSandbox
      ? { ...sandbox, lastKnownState }
      : sandbox;
  const fixture = createRouterTransport((router) => {
    router.rpc(getSandbox, async (request) => {
      const { sandbox } = await client.getSandbox(request);
      return { sandbox: sandbox && update(sandbox) };
    });
    router.rpc(listSandboxes, async (request) => {
      const response = await client.listSandboxes(request);
      return { ...response, sandboxes: response.sandboxes.map(update) };
    });
  });
  return {
    ...base,
    unary(method, ...args) {
      if (
        method.parent.typeName === SandboxService.typeName &&
        (method.name === getSandbox.name || method.name === listSandboxes.name)
      ) {
        return fixture.unary(method, ...args);
      }
      return base.unary(method, ...args);
    },
  };
}
