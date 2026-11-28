// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { AgentIntegrationService } from "@delino/sandrun-protos/v1/agent_integration_pb";
import { ApiKeyService } from "@delino/sandrun-protos/v1/api_key_pb";
import { AuditService } from "@delino/sandrun-protos/v1/audit_pb";
import { DevSessionService } from "@delino/sandrun-protos/v1/dev_session_pb";
import { GitHubService } from "@delino/sandrun-protos/v1/github_pb";
import { MemberService } from "@delino/sandrun-protos/v1/member_pb";
import {
  type ListOrganizationsRequest,
  OrganizationService,
} from "@delino/sandrun-protos/v1/organization_pb";
import { OwnSandLinkService } from "@delino/sandrun-protos/v1/ownsand_link_pb";
import { RunnerProfileService } from "@delino/sandrun-protos/v1/runner_profile_pb";
import {
  type ListRequestsRequest,
  WorkService,
} from "@delino/sandrun-protos/v1/work_pb";
import { SandRunClient } from "./client.ts";
import { SandRunError } from "./errors.ts";
import { collect } from "./pagination.ts";
import { FAKE_ORIGIN, fakeServer } from "./testing/fake-server.ts";

const ORG = "0193a000-0000-7000-8000-000000000001";
const RPC_PREFIX = "/delino.sandrun.v1.";

const iteratorCases = {
  organizations: {
    path: `${RPC_PREFIX}OrganizationService/ListOrganizations`,
    item: { organizationId: ORG },
  },
  invitations: {
    path: `${RPC_PREFIX}MemberService/ListInvitations`,
    item: { invitationId: "invitation" },
  },
  members: {
    path: `${RPC_PREFIX}MemberService/ListMembers`,
    item: { userId: "member" },
  },
  gitHubInstallations: {
    path: `${RPC_PREFIX}GitHubService/ListGitHubInstallations`,
    item: { installationId: "installation" },
  },
  agentIntegrations: {
    path: `${RPC_PREFIX}AgentIntegrationService/ListAgentIntegrations`,
    item: { agentIntegrationId: "integration" },
  },
  runnerProfiles: {
    path: `${RPC_PREFIX}RunnerProfileService/ListRunnerProfiles`,
    item: { runnerProfileId: "runner-profile" },
  },
  requests: {
    path: `${RPC_PREFIX}WorkService/ListRequests`,
    item: { requestId: "request" },
  },
  runners: {
    path: `${RPC_PREFIX}WorkService/ListRunners`,
    item: { runnerId: "runner" },
  },
  apiKeys: {
    path: `${RPC_PREFIX}ApiKeyService/ListApiKeys`,
    item: { apiKeyId: "api-key" },
  },
  devSessionProfiles: {
    path: `${RPC_PREFIX}DevSessionService/ListDevSessionProfiles`,
    item: { runnerProfileId: "dev-session-profile" },
  },
  devSessions: {
    path: `${RPC_PREFIX}DevSessionService/ListDevSessions`,
    item: { devSessionId: "dev-session" },
  },
  auditEvents: {
    path: `${RPC_PREFIX}AuditService/ListAuditEvents`,
    item: { auditEventId: "audit-event" },
  },
};
const iteratorNames = Object.keys(
  iteratorCases,
) as (keyof typeof iteratorCases)[];

describe("pagination", () => {
  test("follows next_page_token until it is empty and keeps the filters", async () => {
    const seen: ListRequestsRequest[] = [];
    const pages: Record<string, { ids: string[]; next: string }> = {
      "": { ids: ["r1", "r2"], next: "p2" },
      p2: { ids: ["r3", "r4"], next: "p3" },
      p3: { ids: ["r5"], next: "" },
    };
    const server = fakeServer((router) => {
      router.service(WorkService, {
        listRequests: (req) => {
          seen.push(req);
          const page = pages[req.pageToken]!;
          return {
            requests: page.ids.map((requestId) => ({ requestId })),
            nextPageToken: page.next,
          };
        },
      });
    });
    const client = new SandRunClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "test-token",
    });
    const items = await collect(
      client.iterate.requests({
        organizationId: ORG,
        pageSize: 2,
        includeEnded: true,
      }),
    );
    expect(items.map((r) => r.requestId)).toEqual([
      "r1",
      "r2",
      "r3",
      "r4",
      "r5",
    ]);
    expect(seen.map((r) => r.pageToken)).toEqual(["", "p2", "p3"]);
    expect(seen.every((r) => r.pageSize === 2 && r.includeEnded)).toBe(true);
    expect(seen.every((r) => r.organizationId === ORG)).toBe(true);
  });

  test("stops early when the caller stops iterating", async () => {
    const seen: ListOrganizationsRequest[] = [];
    const server = fakeServer((router) => {
      router.service(OrganizationService, {
        listOrganizations: (req) => {
          seen.push(req);
          return {
            organizations: [{ organizationId: `o-${req.pageToken}` }],
            nextPageToken: `${req.pageToken}x`,
          };
        },
      });
    });
    const client = new SandRunClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "test-token",
    });
    const received: string[] = [];
    for await (const org of client.iterate.organizations()) {
      received.push(org.organizationId);
      break;
    }
    expect(received).toEqual(["o-"]);
    expect(seen.map((request) => request.pageToken)).toEqual([""]);
  });

  test("ends with an error when the server repeats a page token", async () => {
    let requestNumber = 0;
    const server = fakeServer((router) => {
      router.service(AuditService, {
        listAuditEvents: (req) => {
          requestNumber += 1;
          if (requestNumber > 2) {
            throw new ConnectError(
              `Repeated-token fixture received unexpected ListAuditEvents request ${requestNumber} with page token ${JSON.stringify(req.pageToken)}`,
              Code.FailedPrecondition,
            );
          }
          return {
            events: [{ auditEventId: "a" }],
            nextPageToken: "same",
          };
        },
      });
    });
    const client = new SandRunClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "test-token",
    });
    await expect(
      collect(client.iterate.auditEvents({ organizationId: ORG })),
    ).rejects.toThrow(SandRunError);
    expect(server.callsOf("ListAuditEvents").length).toBe(2);
  });

  test("a failed page ends the iteration with the Connect code", async () => {
    const server = fakeServer((router) => {
      router.service(MemberService, {
        listMembers: (req) => {
          if (req.pageToken === "p2") {
            throw new ConnectError("gone", Code.NotFound);
          }
          return { members: [{ userId: "u1" }], nextPageToken: "p2" };
        },
      });
    });
    const client = new SandRunClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "test-token",
    });
    const received: string[] = [];
    const error = await (async () => {
      for await (const member of client.iterate.members({
        organizationId: ORG,
      })) {
        received.push(member.userId);
      }
    })().catch((e: unknown) => e);
    expect(received).toEqual(["u1"]);
    expect((error as ConnectError).code).toBe(Code.NotFound);
  });

  test("maps the public iterators to all paginated list RPCs one to one", () => {
    const paginatedPaths = [
      OrganizationService,
      MemberService,
      OwnSandLinkService,
      GitHubService,
      AgentIntegrationService,
      RunnerProfileService,
      WorkService,
      ApiKeyService,
      DevSessionService,
      AuditService,
    ].flatMap((service) =>
      service.methods
        .filter((method) =>
          method.input.fields.some((field) => field.name === "page_token"),
        )
        .map((method) => `/${service.typeName}/${method.name}`),
    );
    const expectedPaths = Object.values(iteratorCases).map(({ path }) => path);
    const client = new SandRunClient({
      origin: FAKE_ORIGIN,
      token: "test-token",
    });
    expect(Object.keys(client.iterate).sort()).toEqual(
      [...iteratorNames].sort(),
    );
    expect(new Set(expectedPaths).size).toBe(expectedPaths.length);
    expect(paginatedPaths.sort()).toEqual(expectedPaths.sort());
  });

  test.each(iteratorNames)(
    "%s calls its RPC and yields its item",
    async (name) => {
      const lastPage = { nextPageToken: "" };
      const server = fakeServer((router) => {
        router.service(OrganizationService, {
          listOrganizations: () => ({
            ...lastPage,
            organizations: [iteratorCases.organizations.item],
          }),
        });
        router.service(MemberService, {
          listInvitations: () => ({
            ...lastPage,
            invitations: [iteratorCases.invitations.item],
          }),
          listMembers: () => ({
            ...lastPage,
            members: [iteratorCases.members.item],
          }),
        });
        router.service(GitHubService, {
          listGitHubInstallations: () => ({
            ...lastPage,
            installations: [iteratorCases.gitHubInstallations.item],
          }),
        });
        router.service(AgentIntegrationService, {
          listAgentIntegrations: () => ({
            ...lastPage,
            integrations: [iteratorCases.agentIntegrations.item],
          }),
        });
        router.service(RunnerProfileService, {
          listRunnerProfiles: () => ({
            ...lastPage,
            profiles: [iteratorCases.runnerProfiles.item],
          }),
        });
        router.service(WorkService, {
          listRequests: () => ({
            ...lastPage,
            requests: [iteratorCases.requests.item],
          }),
          listRunners: () => ({
            ...lastPage,
            runners: [iteratorCases.runners.item],
          }),
        });
        router.service(ApiKeyService, {
          listApiKeys: () => ({
            ...lastPage,
            apiKeys: [iteratorCases.apiKeys.item],
          }),
        });
        router.service(DevSessionService, {
          listDevSessionProfiles: () => ({
            ...lastPage,
            profiles: [iteratorCases.devSessionProfiles.item],
          }),
          listDevSessions: () => ({
            ...lastPage,
            devSessions: [iteratorCases.devSessions.item],
          }),
        });
        router.service(AuditService, {
          listAuditEvents: () => ({
            ...lastPage,
            events: [iteratorCases.auditEvents.item],
          }),
        });
      });
      const client = new SandRunClient({
        origin: FAKE_ORIGIN,
        fetch: server.fetch,
        token: "test-token",
      });
      const items = await collect<unknown>(
        name === "organizations"
          ? client.iterate.organizations()
          : client.iterate[name]({ organizationId: ORG }),
      );

      expect(server.calls.map(({ path }) => path)).toEqual([
        iteratorCases[name].path,
      ]);
      expect(items).toHaveLength(1);
      expect(items[0]).toMatchObject(iteratorCases[name].item);
    },
  );
});
