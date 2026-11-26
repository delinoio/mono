// SPDX-License-Identifier: Apache-2.0
import {
  Code,
  ConnectError,
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import { Role } from "@delino/ownsand-protos/v1/common_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

export const FIXTURE_ORGANIZATION_ID = "0193a000-0000-7000-8000-0000000000a1";
export const FIXTURE_ORGANIZATION_NAME = "Fixture organization";
export const MIXED_CASE_ORGANIZATION_ID =
  "0193A000-0000-7000-8000-0000000000a1";

/** Model the control plane's UUID handling for organization routes in tests. */
export function organizationUuidTransport(base: Transport): Transport {
  const organizations = [
    {
      organizationId: MOCK_IDS.northwind,
      name: "Northwind Labs",
      callerRole: Role.MEMBER,
    },
    {
      organizationId: FIXTURE_ORGANIZATION_ID,
      name: FIXTURE_ORGANIZATION_NAME,
      callerRole: Role.OWNER,
    },
  ];
  const fixture = createRouterTransport((router) => {
    router.rpc(OrganizationService.method.listOrganizations, () => ({
      organizations,
    }));
    router.rpc(OrganizationService.method.getOrganization, (request) => {
      const organization = organizations.find(
        (org) => org.organizationId === request.organizationId.toLowerCase(),
      );
      if (organization === undefined) {
        throw new ConnectError("organization not found", Code.NotFound);
      }
      return { organization };
    });
  });
  return {
    ...base,
    unary(method, signal, timeout, header, input, contextValues) {
      if (
        method.parent.typeName === OrganizationService.typeName &&
        (method.name === "GetOrganization" ||
          method.name === "ListOrganizations")
      ) {
        return fixture.unary(
          method,
          signal,
          timeout,
          header,
          input,
          contextValues,
        );
      }
      const request = input as typeof input & { organizationId?: string };
      // Use Acme's seeded resources for the fixture organization.
      const resourceInput =
        request.organizationId?.toLowerCase() === FIXTURE_ORGANIZATION_ID
          ? { ...request, organizationId: MOCK_IDS.acme }
          : input;
      return base.unary(
        method,
        signal,
        timeout,
        header,
        resourceInput,
        contextValues,
      );
    },
  };
}
