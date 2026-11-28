// SPDX-License-Identifier: Apache-2.0
import type { Transport } from "@connectrpc/connect";
import type { MockTransportOptions } from "../src/mock/mock-api.ts";
import { createMockTransport as createSettingsMock } from "./settings-mock.ts";
import {
  pagedProfileIntegrations,
  PROFILE_INTEGRATION_FIXTURE,
} from "./profile-integration-fixture.ts";
import { linkedSetupTransport } from "./setup-guidance-fixture.ts";

declare global {
  interface Window {
    sandrunProfileIntegrationFixture?: Omit<
      ReturnType<typeof pagedProfileIntegrations>,
      "transport"
    >;
  }
}

export function createMockTransport(options?: MockTransportOptions): Transport {
  const setup = sessionStorage.getItem("sandrun-e2e-setup-guidance");
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
