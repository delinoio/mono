// SPDX-License-Identifier: Apache-2.0
import { createRouterTransport, type Transport } from "@connectrpc/connect";
import { AgentIntegrationService } from "@delino/sandrun-protos/v1/agent_integration_pb";
import { OwnSandLinkState } from "@delino/sandrun-protos/v1/common_pb";
import { GitHubService } from "@delino/sandrun-protos/v1/github_pb";
import { OwnSandLinkService } from "@delino/sandrun-protos/v1/ownsand_link_pb";
import { RunnerProfileService } from "@delino/sandrun-protos/v1/runner_profile_pb";
import { WorkService } from "@delino/sandrun-protos/v1/work_pb";

export function linkedSetupTransport(
  mock: Transport,
  empty: (
    "ListGitHubInstallations" | "ListAgentIntegrations" | "ListRunnerProfiles"
  )[],
  linkState = OwnSandLinkState.ACTIVE,
): Transport {
  const fixture = createRouterTransport((router) => {
    router.service(OwnSandLinkService, {
      getOwnSandLink: ({ organizationId }) => ({
        link: { organizationId, state: linkState },
      }),
    });
    router.service(GitHubService, { listGitHubInstallations: () => ({}) });
    router.service(AgentIntegrationService, {
      listAgentIntegrations: () => ({}),
    });
    router.service(RunnerProfileService, { listRunnerProfiles: () => ({}) });
    router.service(WorkService, { listRunners: () => ({}) });
  });
  const fixtureMethods = new Set<string>([
    ...empty,
    "GetOwnSandLink",
    "ListRunners",
  ]);
  return {
    ...mock,
    unary(method, ...args) {
      const transport = fixtureMethods.has(method.name) ? fixture : mock;
      return transport.unary(method, ...args);
    },
  };
}
