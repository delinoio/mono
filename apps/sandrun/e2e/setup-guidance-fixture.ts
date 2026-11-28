// SPDX-License-Identifier: Apache-2.0
import {
  Code,
  ConnectError,
  createRouterTransport,
  type ServiceImpl,
  type Transport,
} from "@connectrpc/connect";
import { AgentIntegrationService } from "@delino/sandrun-protos/v1/agent_integration_pb";
import {
  GitHubInstallationState,
  OwnSandLinkState,
} from "@delino/sandrun-protos/v1/common_pb";
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
  listGitHubInstallations?: ServiceImpl<
    typeof GitHubService
  >["listGitHubInstallations"],
): Transport {
  const fixture = createRouterTransport((router) => {
    router.service(OwnSandLinkService, {
      getOwnSandLink: ({ organizationId }) => ({
        link: { organizationId, state: linkState },
      }),
    });
    router.service(GitHubService, {
      listGitHubInstallations: listGitHubInstallations ?? (() => ({})),
    });
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
  if (listGitHubInstallations) fixtureMethods.add("ListGitHubInstallations");
  return {
    ...mock,
    unary(method, ...args) {
      const transport = fixtureMethods.has(method.name) ? fixture : mock;
      return transport.unary(method, ...args);
    },
  };
}

export const SETUP_GUIDANCE_FIXTURE = "sandrun-e2e-setup-guidance";

export function setupWithGitHubInstallations(
  mock: Transport,
  {
    firstState = GitHubInstallationState.SUSPENDED,
    continuation,
    existingProfile,
    existingAgentIntegration,
  }: {
    firstState?: GitHubInstallationState;
    continuation?: GitHubInstallationState | "fail";
    existingProfile?: boolean;
    existingAgentIntegration?: boolean;
  } = {},
): Transport {
  const empty: Parameters<typeof linkedSetupTransport>[1] = [];
  if (!existingProfile) empty.push("ListRunnerProfiles");
  if (!existingAgentIntegration) empty.push("ListAgentIntegrations");
  return linkedSetupTransport(
    mock,
    empty,
    OwnSandLinkState.ACTIVE,
    ({ organizationId, pageToken }) => {
      const firstPage = pageToken === "";
      const state = firstPage ? firstState : continuation;
      if (state === "fail") {
        throw new ConnectError("GitHub continuation failed", Code.Internal);
      }
      if (state === undefined) return {};
      const pageNumber = firstPage ? "first" : "second";
      return {
        installations: [
          {
            organizationId,
            installationId: `${pageNumber}-installation`,
            githubAccountLogin: `github-${pageNumber}-page`,
            state,
          },
        ],
        nextPageToken: firstPage && continuation !== undefined ? "next" : "",
      };
    },
  );
}
