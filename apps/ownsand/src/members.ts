// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery } from "@connectrpc/connect-query";
import type { ConnectError } from "@connectrpc/connect";
import type { Member } from "@delino/ownsand-protos/v1/member_pb";
import { MemberService } from "@delino/ownsand-protos/v1/member_pb";
import { useMemo } from "react";
import type { BoundaryQuery } from "./components/ui.tsx";
import {
  FOLLOW_ALL_PAGES_OPTIONS,
  uniqueRows,
  useFollowAllPages,
} from "./pagination.ts";

/** The members of one organization, as the members page and the settings show them. */
export interface MembersResult {
  /** Every member that the loaded pages hold, in the server's order. */
  members: Member[];
  /** Whether the first page is still loading. */
  isPending: boolean;
  /** Whether at least the first page is loaded, also after a failed refetch. */
  isLoaded: boolean;
  /** Whether a request of the entry is running now. */
  isFetching: boolean;
  /** The error of the first page or of a refetch; null when a later page failed. */
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
 * Reads every page of `ListMembers` of an organization into one cached list.
 *
 * The control plane caps a page at 200 members and returns a continuation
 * token while a page is full, so the hook follows the token until the
 * response has none. The members page and the ownership transfer of the
 * settings page both call it, so they share one React Query entry and one
 * set of requests; the entry is an infinite query of the `MemberService`, so
 * `invalidateServices(MemberService)` refetches it from the first page. A
 * failed later-page request stops the loop and surfaces as `loadMoreError`
 * with `isIncomplete`; `retryLoadMore` continues from the last loaded page.
 */
export function useMembers(organizationId: string): MembersResult {
  const query = useInfiniteQuery(
    MemberService.method.listMembers,
    { organizationId, pageSize: 200, pageToken: "" },
    {
      ...FOLLOW_ALL_PAGES_OPTIONS,
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const pages = useFollowAllPages(query);
  const { data } = query;

  // A membership change between two page requests can repeat a member on the
  // boundary; keep one row for each.
  const members = useMemo(
    () =>
      uniqueRows(
        data?.pages.map((page) => page.members) ?? [],
        (m) => m.userId,
      ),
    [data],
  );

  return {
    members,
    isPending: query.isPending,
    isLoaded: data !== undefined,
    isFetching: query.isFetching,
    ...pages,
    refetch: () => query.refetch(),
  };
}

/**
 * The query that `QueryBoundary` reads for the members: the error of a later
 * page is not the boundary's error, because the page shows the loaded members
 * and `MorePagesNotice` offers the retry of that page.
 */
export function membersBoundary(
  result: MembersResult,
): BoundaryQuery<Member[]> {
  return {
    data: result.isLoaded ? result.members : undefined,
    error: result.error,
    isPending: result.isPending,
    isFetching: result.isFetching,
    refetch: result.refetch,
  };
}
