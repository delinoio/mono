// SPDX-License-Identifier: Apache-2.0
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import {
  createClient,
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import { Role } from "@delino/tide-protos/v1/common_pb";
import {
  GitHubService,
  type LinkGitHubInstallationRequest,
} from "@delino/tide-protos/v1/github_pb";
import {
  type ListOrganizationsRequest,
  OrganizationSchema,
  OrganizationService,
} from "@delino/tide-protos/v1/organization_pb";
import { createMockTransport } from "./mock-api.ts";

/** Gives the console a full first page and a configurable terminal page. */
export async function organizationPages({
  firstRole = Role.MEMBER,
  lastRole,
  beforeLastPage,
}: {
  firstRole?: Role;
  lastRole?: Role;
  beforeLastPage?: () => Promise<void>;
}) {
  const mock = createMockTransport();
  const client = createClient(OrganizationService, mock);
  const [acme, northwind, globex] = (await client.listOrganizations({}))
    .organizations;
  const firstPage = [
    { ...acme, callerRole: firstRole },
    northwind,
    ...Array.from({ length: 198 }, (_, index) =>
      create(OrganizationSchema, {
        organizationId: `fixture-${index + 3}`,
        name: `Fixture organization ${index + 3}`,
        callerRole: Role.MEMBER,
      }),
    ),
  ];
  const listRequests: ListOrganizationsRequest[] = [];
  const linkRequests: LinkGitHubInstallationRequest[] = [];
  const listMethod = OrganizationService.method.listOrganizations;
  const linkMethod = GitHubService.method.linkGitHubInstallation;
  const pages = createRouterTransport((router) => {
    router.rpc(listMethod, async (req) => {
      listRequests.push(req);
      if (req.pageToken === "") {
        return { organizations: firstPage, nextPageToken: "after-200" };
      }
      await beforeLastPage?.();
      return {
        organizations:
          lastRole === undefined ? [] : [{ ...globex, callerRole: lastRole }],
      };
    });
  });
  const transport: Transport = {
    ...mock,
    async unary(method, ...args) {
      if (method.name === listMethod.name) return pages.unary(method, ...args);
      if (method.name === linkMethod.name) {
        linkRequests.push(
          create(
            linkMethod.input,
            args[3] as MessageInitShape<typeof linkMethod.input>,
          ),
        );
      }
      return mock.unary(method, ...args);
    },
  };
  return { transport, listRequests, linkRequests };
}
