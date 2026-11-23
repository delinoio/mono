// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery } from "@connectrpc/connect-query";
import type { ConnectError } from "@connectrpc/connect";
import type { Organization } from "@delino/ownsand-protos/v1/organization_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { useMemo } from "react";
import { LIST_ORGANIZATIONS_REQUEST } from "./api.ts";
import {
  FOLLOW_ALL_PAGES_OPTIONS,
  uniqueRows,
  useFollowAllPages,
} from "./pagination.ts";

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
 * last loaded page. While that failure stands, a window focus, a reconnect
 * or a new observer does not refetch the entry, so only `retryLoadMore` or
 * an invalidation requests a page again.
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
    isPending: query.isPending,
    isLoaded: data !== undefined,
    ...pages,
    refetch: () => query.refetch(),
  };
}
