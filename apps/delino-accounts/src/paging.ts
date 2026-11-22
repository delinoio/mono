// SPDX-License-Identifier: Apache-2.0
import type {
  DescMessage,
  DescMethodUnary,
  MessageInitShape,
  MessageShape,
} from "@bufbuild/protobuf";
import type { ConnectError } from "@connectrpc/connect";
import { useInfiniteQuery } from "@connectrpc/connect-query";
import type {
  InfiniteData,
  UseInfiniteQueryResult,
} from "@tanstack/react-query";
import { useEffect } from "react";
import { LIST_PAGE_SIZE } from "./api.ts";

/** The page of a list RPC that this app reads completely. */
export interface ListPage<Item> {
  items: Item[];
  /** The continuation token; an empty string marks the last page. */
  nextPageToken: string;
}

/** The state of a list that is read through all of its pages. */
export interface AllPages<Item> {
  /** Every item that has loaded, in server order, without duplicates. */
  items: Item[];
  /** The first page is still loading. */
  isPending: boolean;
  /**
   * The first request or a refresh failed. Retrying it reads the list again
   * from the first page.
   */
  error: unknown;
  /** The list continues, so a later page is loading or has not loaded. */
  isLoadingMore: boolean;
  /**
   * A request for a later page failed. The items of the earlier pages stay
   * and `retryContinuation` resumes at the failed page.
   */
  continuationError: unknown;
  refetch: () => void;
  retryContinuation: () => void;
}

/**
 * `useInfiniteQuery` with the page parameter fixed to the `pageToken` request
 * field. The generic signature of connect-query constrains the parameter key
 * by the request type, which cannot be expressed for an arbitrary list RPC
 * here, so the call is typed by hand. `pageToken` is checked by the callers'
 * tests and by the `pageToken` field of every list request in the protos.
 */
type PageTokenInfiniteQuery = <I extends DescMessage, O extends DescMessage>(
  schema: DescMethodUnary<I, O>,
  input: object,
  options: {
    pageParamKey: "pageToken";
    getNextPageParam: (last: MessageShape<O>) => string | undefined;
  },
) => UseInfiniteQueryResult<
  InfiniteData<MessageShape<O>, unknown>,
  ConnectError
>;

/**
 * Reads a list RPC that returns `page_token` pages until the server returns an
 * empty `next_page_token`, so that a list with more than one page of items
 * shows every item. Every page request keeps the request scope of the first
 * page and the page size of `LIST_PAGE_SIZE`; only `page_token` changes.
 *
 * `itemKey` identifies an item: the helper drops an item whose key an earlier
 * page already delivered, because a page boundary can move while the list
 * changes.
 */
export function useAllPages<I extends DescMessage, O extends DescMessage, Item>(
  schema: DescMethodUnary<I, O>,
  scope: Omit<MessageInitShape<I>, "pageToken" | "pageSize">,
  read: (response: MessageShape<O>) => ListPage<Item>,
  itemKey: (item: Item) => string,
): AllPages<Item> {
  const query = (useInfiniteQuery as unknown as PageTokenInfiniteQuery)(
    schema,
    { ...scope, pageSize: LIST_PAGE_SIZE, pageToken: "" },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => read(last).nextPageToken || undefined,
    },
  );
  const {
    hasNextPage,
    isFetching,
    isFetchNextPageError,
    fetchNextPage,
    refetch,
  } = query;
  // Read the next page as soon as the previous one has loaded. A failed page
  // stops the loop until the user retries it, so a failing server is not
  // called in a tight loop.
  useEffect(() => {
    if (hasNextPage && !isFetching && !isFetchNextPageError) {
      void fetchNextPage();
    }
  }, [hasNextPage, isFetching, isFetchNextPageError, fetchNextPage]);

  const seen = new Set<string>();
  const items: Item[] = [];
  for (const page of query.data?.pages ?? []) {
    for (const item of read(page).items) {
      const id = itemKey(item);
      if (!seen.has(id)) {
        seen.add(id);
        items.push(item);
      }
    }
  }
  return {
    items,
    isPending: query.isPending,
    error: isFetchNextPageError ? undefined : query.error,
    isLoadingMore: hasNextPage && !isFetchNextPageError,
    continuationError: isFetchNextPageError ? query.error : undefined,
    refetch: () => void refetch(),
    retryContinuation: () => void fetchNextPage(),
  };
}
