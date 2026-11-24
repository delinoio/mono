// SPDX-License-Identifier: Apache-2.0
import {
  callUnaryMethod,
  createConnectQueryKey,
  useTransport,
} from "@connectrpc/connect-query";
import {
  type Organization,
  OrganizationService,
} from "@delino/tide-protos/v1/organization_pb";
import { type QueryClient, useQuery } from "@tanstack/react-query";
import { MAX_PAGE_SIZE } from "./api.ts";

const listOrganizations = OrganizationService.method.listOrganizations;

/** Shares a complete list and includes it in OrganizationService invalidation. */
export function useOrganizations(enabled = true) {
  const transport = useTransport();
  return useQuery({
    queryKey: [
      ...createConnectQueryKey({
        schema: listOrganizations,
        cardinality: "finite",
        transport,
      }),
      "all-pages",
    ],
    enabled,
    queryFn: async ({ signal }) => {
      const organizations: Organization[] = [];
      let pageToken = "";
      do {
        const page = await callUnaryMethod(
          transport,
          listOrganizations,
          { pageSize: MAX_PAGE_SIZE, pageToken },
          { signal },
        );
        organizations.push(...page.organizations);
        pageToken = page.nextPageToken;
      } while (pageToken !== "");
      return organizations;
    },
  });
}

/** Restarts a load that may have begun before the organization was created. */
export async function refreshOrganizations(queryClient: QueryClient) {
  const queryKey = createConnectQueryKey({
    schema: listOrganizations,
    cardinality: undefined,
  });
  await queryClient.cancelQueries({ queryKey });
  await queryClient.invalidateQueries({ queryKey });
}
