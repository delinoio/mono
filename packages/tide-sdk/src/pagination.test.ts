// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { AgentIntegrationService } from "@delino/tide-protos/v1/agent_integration_pb";
import { ApiKeyService } from "@delino/tide-protos/v1/api_key_pb";
import { AuditService } from "@delino/tide-protos/v1/audit_pb";
import { DevSessionService } from "@delino/tide-protos/v1/dev_session_pb";
import { GitHubService } from "@delino/tide-protos/v1/github_pb";
import { MemberService } from "@delino/tide-protos/v1/member_pb";
import {
  type ListOrganizationsRequest,
  OrganizationService,
} from "@delino/tide-protos/v1/organization_pb";
import { OwnSandLinkService } from "@delino/tide-protos/v1/ownsand_link_pb";
import { RunnerProfileService } from "@delino/tide-protos/v1/runner_profile_pb";
import {
  type ListRequestsRequest,
  WorkService,
} from "@delino/tide-protos/v1/work_pb";
import { createTideIterators, TideClient } from "./client.ts";
import { TideError } from "./errors.ts";
import { collect } from "./pagination.ts";
import { FAKE_ORIGIN, fakeServer } from "./testing/fake-server.ts";

const ORG = "0193a000-0000-7000-8000-000000000001";

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
    const client = new TideClient({
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
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "test-token",
    });
    for await (const org of client.iterate.organizations()) {
      expect(org.organizationId).toBe("o-");
      break;
    }
    expect(seen.length).toBe(1);
  });

  test("ends with an error when the server repeats a page token", async () => {
    const server = fakeServer((router) => {
      router.service(AuditService, {
        listAuditEvents: () => ({
          events: [{ auditEventId: "a" }],
          nextPageToken: "same",
        }),
      });
    });
    const client = new TideClient({
      origin: FAKE_ORIGIN,
      fetch: server.fetch,
      token: "test-token",
    });
    await expect(
      collect(client.iterate.auditEvents({ organizationId: ORG })),
    ).rejects.toThrow(TideError);
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
    const client = new TideClient({
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

  test("has an iterator for every paginated list RPC", () => {
    const paginated = [
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
      service.methods.filter((method) =>
        method.input.fields.some((field) => field.name === "page_token"),
      ),
    );
    const iterators = createTideIterators(
      new TideClient({ origin: FAKE_ORIGIN, token: "test-token" }),
    );
    expect(Object.keys(iterators).length).toBe(paginated.length);
    expect(paginated.length).toBe(12);
  });
});
