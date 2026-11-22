// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery } from "@connectrpc/connect-query";
import type { ConnectError } from "@connectrpc/connect";
import type { Organization } from "@delino/ownsand-protos/v1/organization_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { useCallback, useEffect, useMemo } from "react";
import { LIST_ORGANIZATIONS_REQUEST } from "./api.ts";

/** The organizations of the caller, as the organization list and the switcher show them. */
export interface OrganizationsResult {
  /** Every organization that the loaded pages hold, in the server's order. */
  organizations: Organization[];
  /** Whether the first page is still loading. */
  isPending: boolean;
  /** Whether at least the first page is loaded, also after a failed refetch. */
  isLoaded: boolean;
  /** The error of the first page or of a refetch; undefined when a later page failed. */
  error: ConnectError | null;
  /**
   * Whether the control plane still has pages that the console did not
   * load. The list is then incomplete, and a surface must not present it as
   * the whole membership.
   */
  isIncomplete: boolean;
  /** Whether the console is loading a later page now. */
  isLoadingMore: boolean;
  /** The error of the last later-page request, until a retry starts. */
  loadMoreError: ConnectError | null;
  /** Loads the pages that are missing again after `loadMoreError`. */
  retryLoadMore: () => void;
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
 * page. A failed later-page request stops the loop and surfaces as
 * `loadMoreError` with `isIncomplete`; `retryLoadMore` continues from the
 * last loaded page.
 */
export function useOrganizations(): OrganizationsResult {
  const query = useInfiniteQuery(
    OrganizationService.method.listOrganizations,
    LIST_ORGANIZATIONS_REQUEST,
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const { data, hasNextPage, isFetching, isFetchNextPageError, fetchNextPage } =
    query;

  // Two surfaces observe the entry, and both run this effect in the same
  // commit. `cancelRefetch: false` joins the request that is in flight
  // instead of cancelling it and sending the same page again.
  useEffect(() => {
    if (hasNextPage && !isFetching && !isFetchNextPageError) {
      void fetchNextPage({ cancelRefetch: false });
    }
  }, [hasNextPage, isFetching, isFetchNextPageError, fetchNextPage]);

  const organizations = useMemo(() => {
    // A membership change between two page requests can repeat an
    // organization on the boundary; keep one row for each.
    const byId = new Map<string, Organization>();
    for (const page of data?.pages ?? []) {
      for (const org of page.organizations) byId.set(org.organizationId, org);
    }
    return [...byId.values()];
  }, [data]);

  const retryLoadMore = useCallback(() => {
    void fetchNextPage({ cancelRefetch: false });
  }, [fetchNextPage]);

  return {
    organizations,
    isPending: query.isPending,
    isLoaded: data !== undefined,
    error: isFetchNextPageError ? null : query.error,
    isIncomplete: data !== undefined && hasNextPage,
    isLoadingMore: query.isFetchingNextPage,
    loadMoreError: isFetchNextPageError ? query.error : null,
    retryLoadMore,
    refetch: () => query.refetch(),
  };
}
