// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery } from "@connectrpc/connect-query";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { useMemo } from "react";
import { showsFullError } from "./components/ui.tsx";
import {
  boundaryOf,
  FOLLOW_ALL_PAGES_OPTIONS,
  uniqueRows,
  useFollowAllPages,
} from "./pagination.ts";

/** Shares one cached device inventory per organization with the table and pickers. */
export function useDevices(organizationId: string) {
  const query = useInfiniteQuery(
    DeviceService.method.listDevices,
    { organizationId, pageSize: 200, pageToken: "" },
    {
      ...FOLLOW_ALL_PAGES_OPTIONS,
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const pages = useFollowAllPages(query);
  const boundary = boundaryOf(query, pages);
  const data = showsFullError(boundary) ? undefined : query.data;
  const devices = useMemo(
    () =>
      uniqueRows(
        data?.pages.map((page) => page.devices) ?? [],
        (device) => device.deviceId,
      ),
    [data],
  );
  return { devices, boundary, pages };
}
