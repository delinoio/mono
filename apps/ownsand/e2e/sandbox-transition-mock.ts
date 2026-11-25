// SPDX-License-Identifier: Apache-2.0
import {
  Code,
  ConnectError,
  createClient,
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import {
  type GetSandboxRequest,
  SandboxService,
} from "@delino/ownsand-protos/v1/sandbox_pb";

export enum SandboxTransitionOutcome {
  Pending = "pending",
  Missing = "missing",
  Rejected = "rejected",
}

export function mockSandboxTransition(
  base: Transport,
  outcome: SandboxTransitionOutcome,
): Transport {
  const client = createClient(SandboxService, base);
  const resolve = async (
    request: Pick<GetSandboxRequest, "organizationId" | "sandboxId">,
  ) => {
    switch (outcome) {
      case SandboxTransitionOutcome.Pending:
        return { sandbox: (await client.getSandbox(request)).sandbox };
      case SandboxTransitionOutcome.Missing:
        return {};
      case SandboxTransitionOutcome.Rejected:
        throw new ConnectError(
          "Synthetic transition failure",
          Code.Unavailable,
        );
    }
  };
  const { pauseSandbox, resumeSandbox } = SandboxService.method;
  const fixture = createRouterTransport((router) => {
    router.rpc(pauseSandbox, resolve);
    router.rpc(resumeSandbox, resolve);
  });
  return {
    ...base,
    unary(method, ...args) {
      if (
        method.parent.typeName === SandboxService.typeName &&
        (method.name === pauseSandbox.name ||
          method.name === resumeSandbox.name)
      ) {
        return fixture.unary(method, ...args);
      }
      return base.unary(method, ...args);
    },
  };
}

export function browserSandboxTransitionMock(base: Transport): Transport {
  const outcome = Object.values(SandboxTransitionOutcome).find(
    (value) =>
      sessionStorage.getItem("ownsand-e2e-sandbox-transition") === value,
  );
  if (outcome === undefined) return base;
  const mock = mockSandboxTransition(base, outcome);
  if (outcome !== SandboxTransitionOutcome.Pending) return mock;

  let transitions = 0;
  async function hold(phase: "mutation" | "refresh") {
    await new Promise<void>((resolve) => {
      window.addEventListener(
        `ownsand-e2e-release-sandbox-${phase}`,
        () => resolve(),
        {
          once: true,
        },
      );
      sessionStorage.setItem("ownsand-e2e-sandbox-held", phase);
    });
  }

  return {
    ...mock,
    async unary(method, ...args) {
      if (method.name === "PauseSandbox" || method.name === "ResumeSandbox") {
        transitions += 1;
        sessionStorage.setItem(
          "ownsand-e2e-sandbox-transition-count",
          String(transitions),
        );
        await hold("mutation");
      } else if (method.name === "GetSandbox" && transitions > 0) {
        await hold("refresh");
      }
      return mock.unary(method, ...args);
    },
  };
}
