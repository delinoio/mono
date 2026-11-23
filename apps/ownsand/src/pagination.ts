// SPDX-License-Identifier: Apache-2.0
import type { ConnectError } from "@connectrpc/connect";
import { useCallback, useEffect } from "react";

/** The part of a cached query that tells which request failed. */
interface QueryRequestState {
  state: {
    status: string;
    fetchMeta: { fetchMore?: { direction: string } } | null;
  };
}

/**
 * Whether the last request of the query failed on a later page.
 *
 * This is the state that `isFetchNextPageError` of the observer reports, but
 * read from the cached query, so an automatic refetch can ask it before it
 * starts.
 */
function lastRequestFailedOnLaterPage(query: QueryRequestState): boolean {
  return (
    query.state.status === "error" &&
    query.state.fetchMeta?.fetchMore?.direction === "forward"
  );
}

/**
 * Lets React Query refetch the entry on its own trigger (window focus,
 * reconnect, mount of an observer) unless a later page failed.
 *
 * A refetch that succeeds reads the first page again and clears the error
 * flag, so the continuation effect of `useFollowAllPages` would then request
 * the failed page without the user's retry. Returning false keeps the flag
 * and the loaded pages until `retryLoadMore` or an invalidation runs.
 */
function refetchUnlessLaterPageFailed(query: QueryRequestState): boolean {
  return !lastRequestFailedOnLaterPage(query);
}

/**
 * Options of an infinite query that reads every page of a `page_token` list.
 * Spread them into the options of `useInfiniteQuery` and pass the query to
 * `useFollowAllPages`.
 */
export const FOLLOW_ALL_PAGES_OPTIONS = {
  pageParamKey: "pageToken",
  refetchOnWindowFocus: refetchUnlessLaterPageFailed,
  refetchOnReconnect: refetchUnlessLaterPageFailed,
  refetchOnMount: refetchUnlessLaterPageFailed,
} as const;

/** The part of an infinite query result that `useFollowAllPages` reads. */
export interface PagedQuery {
  data: unknown;
  error: ConnectError | null;
  hasNextPage: boolean;
  isFetching: boolean;
  isFetchingNextPage: boolean;
  isFetchNextPageError: boolean;
  fetchNextPage: (options?: { cancelRefetch?: boolean }) => unknown;
}

/** The completeness of a list that the console reads page by page. */
export interface FollowedPages {
  /**
   * Whether the control plane still has pages that the console did not
   * load. The list is then incomplete, and a surface must not present it as
   * the whole list.
   */
  isIncomplete: boolean;
  /** Whether the console is loading a later page now. */
  isLoadingMore: boolean;
  /** The error of the last later-page request, until a retry starts. */
  loadMoreError: ConnectError | null;
  /** Loads the pages that are missing again after `loadMoreError`. */
  retryLoadMore: () => void;
  /** The error of the first page or of a refetch; null when a later page failed. */
  error: ConnectError | null;
}

/**
 * Follows the continuation token of an infinite query until the response has
 * none, so the list holds every row and a row that sorts after the first page
 * (the control plane lists in ascending ID order, so a new row is last) is
 * reachable.
 *
 * A failed later-page request stops the loop and surfaces as `loadMoreError`
 * with `isIncomplete`; `retryLoadMore` continues from the last loaded page.
 * While that failure stands, a window focus, a reconnect or a new observer
 * does not refetch the entry (see `FOLLOW_ALL_PAGES_OPTIONS`), so only
 * `retryLoadMore` or an invalidation requests a page again.
 */
export function useFollowAllPages(query: PagedQuery): FollowedPages {
  const { data, hasNextPage, isFetching, isFetchNextPageError, fetchNextPage } =
    query;

  // Several surfaces can observe one entry, and all of them run this effect
  // in the same commit. `cancelRefetch: false` joins the request that is in
  // flight instead of cancelling it and sending the same page again.
  useEffect(() => {
    if (hasNextPage && !isFetching && !isFetchNextPageError) {
      void fetchNextPage({ cancelRefetch: false });
    }
  }, [hasNextPage, isFetching, isFetchNextPageError, fetchNextPage]);

  const retryLoadMore = useCallback(() => {
    void fetchNextPage({ cancelRefetch: false });
  }, [fetchNextPage]);

  return {
    isIncomplete: data !== undefined && hasNextPage,
    isLoadingMore: query.isFetchingNextPage,
    loadMoreError: isFetchNextPageError ? query.error : null,
    retryLoadMore,
    error: isFetchNextPageError ? null : query.error,
  };
}

/**
 * Flattens the rows of every page and keeps one row for each ID. A change
 * between two page requests can repeat a row on the boundary of two pages.
 */
export function uniqueRows<T>(
  pages: ReadonlyArray<readonly T[]>,
  idOf: (row: T) => string,
): T[] {
  const byId = new Map<string, T>();
  for (const rows of pages) {
    for (const row of rows) byId.set(idOf(row), row);
  }
  return [...byId.values()];
}
