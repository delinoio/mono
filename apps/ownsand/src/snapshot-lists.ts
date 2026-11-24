// SPDX-License-Identifier: Apache-2.0
import type { DescMessage } from "@bufbuild/protobuf";
import {
  type ConnectQueryKey,
  createInfiniteQueryOptions,
  useTransport,
} from "@connectrpc/connect-query";
import { SnapshotService } from "@delino/ownsand-protos/v1/snapshot_pb";
import { useInfiniteQuery } from "@tanstack/react-query";
import { useMemo } from "react";
import { POLL_INTERVAL_MS, snapshotSettling } from "./api.ts";
import { isTerminalError, showsFullError } from "./components/ui.tsx";
import {
  FOLLOW_ALL_PAGES_OPTIONS,
  boundaryOf,
  lastRequestFailedOnLaterPage,
  uniqueRows,
  useFollowAllPages,
} from "./pagination.ts";

type ListSurface = "management" | "picker";

// Keep the management page position independent of the picker's automatic
// continuation. Both keys still match SnapshotService invalidation.
function listKey<T extends DescMessage>(
  key: ConnectQueryKey<T>,
  surface: ListSurface,
): ConnectQueryKey<T> {
  const scope = { ...key[1], surface };
  return [key[0], scope] as ConnectQueryKey<T>;
}

export function useSnapshots(
  organizationId: string,
  surface: ListSurface,
  enabled = true,
) {
  const transport = useTransport();
  const options = createInfiniteQueryOptions(
    SnapshotService.method.listSnapshots,
    { organizationId, pageSize: 200, pageToken: "" },
    {
      transport,
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const query = useInfiniteQuery({
    ...options,
    ...FOLLOW_ALL_PAGES_OPTIONS,
    queryKey: listKey(options.queryKey, surface),
    enabled,
    refetchInterval: (q) => {
      if (
        surface !== "management" ||
        isTerminalError(q.state.error) ||
        lastRequestFailedOnLaterPage(q)
      ) {
        return false;
      }
      const rows = uniqueRows(
        q.state.data?.pages.map((page) => page.snapshots) ?? [],
        (snapshot) => snapshot.snapshotId,
      );
      return rows.some(snapshotSettling) ? POLL_INTERVAL_MS : false;
    },
  });
  const pages = useFollowAllPages(
    query,
    enabled && surface === "picker" && !isTerminalError(query.error),
  );
  const boundary = boundaryOf(query, pages);
  const data = showsFullError(boundary) ? undefined : query.data;
  const rows = useMemo(
    () =>
      uniqueRows(
        data?.pages.map((page) => page.snapshots) ?? [],
        (snapshot) => snapshot.snapshotId,
      ),
    [data],
  );
  return { rows, boundary, pages, fetchNextPage: query.fetchNextPage };
}

export function useTemplates(
  organizationId: string,
  surface: ListSurface,
  enabled = true,
) {
  const transport = useTransport();
  const options = createInfiniteQueryOptions(
    SnapshotService.method.listTemplates,
    { organizationId, pageSize: 200, pageToken: "" },
    {
      transport,
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const query = useInfiniteQuery({
    ...options,
    ...FOLLOW_ALL_PAGES_OPTIONS,
    queryKey: listKey(options.queryKey, surface),
    enabled,
  });
  const pages = useFollowAllPages(
    query,
    enabled && surface === "picker" && !isTerminalError(query.error),
  );
  const boundary = boundaryOf(query, pages);
  const data = showsFullError(boundary) ? undefined : query.data;
  const rows = useMemo(
    () =>
      uniqueRows(
        data?.pages.map((page) => page.templates) ?? [],
        (template) => template.templateId,
      ),
    [data],
  );
  return { rows, boundary, pages, fetchNextPage: query.fetchNextPage };
}
