// SPDX-License-Identifier: Apache-2.0
import type { Transport } from "@connectrpc/connect";
import { GitHubInstallationState } from "@delino/sandrun-protos/v1/common_pb";
import type { MockTransportOptions } from "../src/mock/mock-api.ts";
import { createMockTransport as createSettingsMock } from "./settings-mock.ts";
import {
  pagedProfileIntegrations,
  PROFILE_INTEGRATION_FIXTURE,
} from "./profile-integration-fixture.ts";
import {
  linkedSetupTransport,
  SETUP_GUIDANCE_FIXTURE,
  setupWithGitHubInstallations,
} from "./setup-guidance-fixture.ts";

declare global {
  interface Window {
    sandrunProfileIntegrationFixture?: Omit<
      ReturnType<typeof pagedProfileIntegrations>,
      "transport"
    >;
  }
}

export function createMockTransport(options?: MockTransportOptions): Transport {
  const setup = sessionStorage.getItem(SETUP_GUIDANCE_FIXTURE);
  if (setup === "with-work-source") {
    return linkedSetupTransport(createSettingsMock(options), [
      "ListRunnerProfiles",
    ]);
  }
  if (setup === "without-work-sources") {
    return linkedSetupTransport(createSettingsMock(options), [
      "ListAgentIntegrations",
      "ListGitHubInstallations",
      "ListRunnerProfiles",
    ]);
  }

  switch (setup) {
    case "inactive-github":
      return setupWithGitHubInstallations(createSettingsMock(options), {
        continuation: GitHubInstallationState.DELETED,
      });
    case "inactive-github-with-profile":
      return setupWithGitHubInstallations(createSettingsMock(options), {
        firstState: GitHubInstallationState.DELETED,
        existingProfile: true,
      });
    case "active-github-continuation":
      return setupWithGitHubInstallations(createSettingsMock(options), {
        continuation: GitHubInstallationState.ACTIVE,
      });
    case "failed-github-continuation":
    case "failed-github-continuation-with-agent":
      return setupWithGitHubInstallations(createSettingsMock(options), {
        continuation: "fail",
        existingAgentIntegration:
          setup === "failed-github-continuation-with-agent",
      });
  }

  const mode = sessionStorage.getItem(PROFILE_INTEGRATION_FIXTURE);
  if (mode !== "codex" && mode !== "claude" && mode !== "missing") {
    return createSettingsMock(options);
  }
  const { transport, ...fixture } = pagedProfileIntegrations(
    createSettingsMock({
      ...options,
      agentIntegrationPageSize: 1,
      runnerProfileCount: 4,
    }),
    mode,
  );
  window.sandrunProfileIntegrationFixture = fixture;
  return transport;
}
