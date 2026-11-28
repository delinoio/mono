// SPDX-License-Identifier: Apache-2.0
import {
  Code,
  ConnectError,
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import {
  GitHubInstallationState,
  RequestState,
} from "@delino/sandrun-protos/v1/common_pb";
import { WorkService } from "@delino/sandrun-protos/v1/work_pb";
import { PAGE_SIZE } from "../src/api.ts";
import type { MockTransportOptions } from "../src/mock/mock-api.ts";
import {
  DEV_SESSION_DELETION_FIXTURE,
  devSessionDeletionFixtures,
} from "./dev-session-deletion-fixture.ts";
import {
  DEV_SESSION_REFRESH_FIXTURE,
  devSessionRefreshFixture,
} from "./dev-session-refresh-fixture.ts";
import {
  INTEGRATION_PERMISSION_FIXTURE,
  integrationPermissionFixture,
} from "./integration-permission-fixture.ts";
import {
  organizationCreationFixture,
  ORGANIZATION_CREATION_FIXTURE,
} from "./organization-creation-fixture.ts";
import {
  REQUEST_CANCELLATION_FIXTURE,
  requestCancellationFixture,
} from "./request-cancellation-fixture.ts";
import { observeDisplayNameWrites } from "./display-name-fixture.ts";
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
    sandrunDevSessionReads?: boolean[];
    sandrunRequestCancellationFixture?: Omit<
      ReturnType<typeof requestCancellationFixture>,
      "transport"
    >;
    sandrunOrganizationCreationFixture?: Omit<
      ReturnType<typeof organizationCreationFixture>,
      "transport"
    >;
    sandrunIntegrationPermissionFixture?: Omit<
      ReturnType<typeof integrationPermissionFixture>,
      "transport"
    >;
    sandrunProfileIntegrationFixture?: Omit<
      ReturnType<typeof pagedProfileIntegrations>,
      "transport"
    >;
  }
}

export function createMockTransport(options?: MockTransportOptions): Transport {
  const deletionMode = sessionStorage.getItem(DEV_SESSION_DELETION_FIXTURE);
  if (deletionMode === "starting" || deletionMode === "running") {
    return createSettingsMock({
      ...options,
      workFixtures: devSessionDeletionFixtures(deletionMode),
    });
  }

  const refreshMode = sessionStorage.getItem(DEV_SESSION_REFRESH_FIXTURE);
  if (
    refreshMode === "success" ||
    refreshMode === "fail-cancel" ||
    refreshMode === "fail-delete"
  ) {
    const { transport, reads } = devSessionRefreshFixture(
      createSettingsMock,
      refreshMode,
      options,
    );
    window.sandrunDevSessionReads = reads;
    return transport;
  }

  if (sessionStorage.getItem(REQUEST_CANCELLATION_FIXTURE) === "enabled") {
    const { transport, ...fixture } = requestCancellationFixture(
      createSettingsMock(options),
    );
    window.sandrunRequestCancellationFixture = fixture;
    return transport;
  }

  if (sessionStorage.getItem("sandrun.request-pagination") === "enabled") {
    const base = createSettingsMock(options);
    const continuation = "opaque-queue-boundary";
    const pages = createRouterTransport((router) => {
      router.service(WorkService, {
        listRequests({ pageSize, pageToken }) {
          if (
            pageSize !== PAGE_SIZE ||
            (pageToken !== "" && pageToken !== continuation)
          ) {
            throw new ConnectError(
              "Unexpected request page",
              Code.InvalidArgument,
            );
          }
          const start = pageToken === "" ? 1 : PAGE_SIZE + 1;
          return {
            requests: Array.from(
              { length: pageToken === "" ? PAGE_SIZE : 2 },
              (_, i) => ({
                requestId: `fixture-${start + i}`,
                sourceKey: `request-${start + i}`,
                state: RequestState.QUEUED,
              }),
            ),
            nextPageToken: pageToken === "" ? continuation : "",
          };
        },
      });
    });
    return {
      ...base,
      unary: (method, ...args) =>
        (method.name === WorkService.method.listRequests.name
          ? pages
          : base
        ).unary(method, ...args),
    };
  }

  const createMode = sessionStorage.getItem(ORGANIZATION_CREATION_FIXTURE);
  if (
    createMode === "hold-list" ||
    createMode === "hold-create" ||
    createMode === "fail-create"
  ) {
    const { transport, ...fixture } = organizationCreationFixture(
      createSettingsMock(options),
      createMode,
    );
    window.sandrunOrganizationCreationFixture = fixture;
    return transport;
  }

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

  const permission = sessionStorage.getItem(INTEGRATION_PERMISSION_FIXTURE);
  if (permission === "linked" || permission === "unlinked") {
    const { transport, ...fixture } = integrationPermissionFixture(
      createSettingsMock(options),
      permission === "unlinked",
    );
    window.sandrunIntegrationPermissionFixture = fixture;
    return transport;
  }

  const mode = sessionStorage.getItem(PROFILE_INTEGRATION_FIXTURE);
  if (mode !== "codex" && mode !== "claude" && mode !== "missing") {
    return observeDisplayNameWrites(createSettingsMock(options));
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
