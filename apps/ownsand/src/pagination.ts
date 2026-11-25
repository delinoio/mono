// SPDX-License-Identifier: Apache-2.0
import type { QueryClient, QueryKey } from "@tanstack/react-query";
import { useCallback, useEffect } from "react";
import { type BoundaryQuery, isTerminalError } from "./components/ui.tsx";

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
export function lastRequestFailedOnLaterPage(
  query: QueryRequestState,
): boolean {
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

/**
 * Cancel an uncached list fetch before invalidating after a successful write.
 * Invalidation can reuse a pending fetch when it has no cached data.
 */
export function cancelUncachedList(
  queryClient: QueryClient,
  queryKey: QueryKey,
): Promise<void> {
  return queryClient.cancelQueries({
    queryKey,
    exact: true,
    fetchStatus: "fetching",
    predicate: (query) => query.state.data === undefined,
  });
}

/** The part of an infinite query result that `useFollowAllPages` reads. */
export interface PagedQuery {
  data: unknown;
  error: Error | null;
  hasNextPage: boolean;
  isFetching: boolean;
  isFetchingNextPage: boolean;
  isFetchNextPageError: boolean;
  fetchNextPage: (options?: { cancelRefetch?: boolean }) => unknown;
}

/** The completeness of a list that the console reads page by page. */
export interface FollowedPages {
  /**
   * Whether the console can show loaded rows while more pages remain.
   */
  isIncomplete: boolean;
  /** Whether the console is loading a later page now. */
  isLoadingMore: boolean;
  /** The transient error of the last later-page request. */
  loadMoreError: Error | null;
  /** Loads the pages that are missing again after `loadMoreError`. */
  retryLoadMore: () => void;
  /** The terminal error of the last later-page request. */
  terminalLoadMoreError: Error | null;
  /** The full-list error, excluding transient later-page errors. */
  error: Error | null;
}

/**
 * Follows the continuation token of an infinite query until the response has
 * none, so the list holds every row and a row that sorts after the first page
 * (the control plane lists in ascending ID order, so a new row is last) is
 * reachable.
 *
 * A failed later-page request stops the loop. A transient error leaves the
 * list incomplete and lets the user retry from the last loaded page. A
 * terminal error replaces the list. Automatic refetch triggers leave the
 * failed query in place (see `FOLLOW_ALL_PAGES_OPTIONS`).
 */
export function useFollowAllPages(
  query: PagedQuery,
  enabled = true,
): FollowedPages {
  const { data, hasNextPage, isFetching, isFetchNextPageError, fetchNextPage } =
    query;

  // Several surfaces can observe one entry, and all of them run this effect
  // in the same commit. `cancelRefetch: false` joins the request that is in
  // flight instead of cancelling it and sending the same page again.
  //
  // `data` is a dependency because a response can arrive before React renders
  // the fetching state of the request before it. Then `hasNextPage` and
  // `isFetching` have the same values in two commits, and only the new page
  // tells the effect to request the page after it.
  useEffect(() => {
    if (enabled && hasNextPage && !isFetching && !isFetchNextPageError) {
      void fetchNextPage({ cancelRefetch: false });
    }
  }, [
    data,
    enabled,
    hasNextPage,
    isFetching,
    isFetchNextPageError,
    fetchNextPage,
  ]);

  const continuationError = isFetchNextPageError ? query.error : null;
  const terminalLoadMoreError = isTerminalError(continuationError)
    ? continuationError
    : null;
  const loadMoreError =
    terminalLoadMoreError === null ? continuationError : null;

  const retryLoadMore = useCallback(() => {
    if (loadMoreError !== null) void fetchNextPage({ cancelRefetch: false });
  }, [fetchNextPage, loadMoreError]);

  return {
    isIncomplete:
      terminalLoadMoreError === null && data !== undefined && hasNextPage,
    isLoadingMore: query.isFetchingNextPage,
    loadMoreError,
    terminalLoadMoreError,
    retryLoadMore,
    error: loadMoreError === null ? query.error : null,
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

/**
 * Uses the full-list error for `QueryBoundary`. A transient later-page error
 * stays with the loaded rows and its retry in `MorePagesNotice`.
 */
export function boundaryOf<TData>(
  query: {
    data: TData | undefined;
    isPending: boolean;
    isFetching: boolean;
    refetch: () => unknown;
  },
  pages: Pick<FollowedPages, "error">,
): BoundaryQuery<TData> {
  return {
    data: query.data,
    error: pages.error,
    isPending: query.isPending,
    isFetching: query.isFetching,
    refetch: query.refetch,
  };
}
