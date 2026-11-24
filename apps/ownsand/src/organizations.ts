// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery } from "@connectrpc/connect-query";
import type { Organization } from "@delino/ownsand-protos/v1/organization_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { useMemo } from "react";
import { LIST_ORGANIZATIONS_REQUEST } from "./api.ts";
import {
  type FollowedPages,
  FOLLOW_ALL_PAGES_OPTIONS,
  uniqueRows,
  useFollowAllPages,
} from "./pagination.ts";

/** The organizations of the caller, as the organization list and the switcher show them. */
export interface OrganizationsResult extends FollowedPages {
  /** Every organization that the loaded pages hold, in the server's order. */
  organizations: Organization[];
  /** The loaded organizations, or undefined until the first page loads. */
  data: Organization[] | undefined;
  /** Whether the first page is still loading. */
  isPending: boolean;
  /** Whether at least the first page is loaded, also after a failed refetch. */
  isLoaded: boolean;
  /** Whether a request for this list is in progress. */
  isFetching: boolean;
  /** Reads every page again from the first one. */
  refetch: () => Promise<unknown>;
}

/**
 * Reads every page of `ListOrganizations` into one cached list.
 *
 * The control plane caps a page at 200 organizations and returns a
 * continuation token while a page is full, so the hook follows the token
 * until the response has none. The organization list and the switcher both
 * call it, so they share one React Query entry and one set of requests; the
 * entry is an infinite query of the `OrganizationService`, so
 * `invalidateServices(OrganizationService)` refetches it from the first
 * page. A failed later-page request stops the loop. See `useFollowAllPages`
 * for how transient and terminal errors affect the list.
 */
export function useOrganizations(): OrganizationsResult {
  const query = useInfiniteQuery(
    OrganizationService.method.listOrganizations,
    LIST_ORGANIZATIONS_REQUEST,
    {
      ...FOLLOW_ALL_PAGES_OPTIONS,
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const pages = useFollowAllPages(query);
  const { data } = query;

  // A membership change between two page requests can repeat an
  // organization on the boundary; keep one row for each.
  const organizations = useMemo(
    () =>
      uniqueRows(
        data?.pages.map((page) => page.organizations) ?? [],
        (org) => org.organizationId,
      ),
    [data],
  );

  return {
    organizations,
    data: data === undefined ? undefined : organizations,
    isPending: query.isPending,
    isFetching: query.isFetching,
    isLoaded: data !== undefined,
    ...pages,
    refetch: query.refetch,
  };
}
