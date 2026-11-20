// SPDX-License-Identifier: Apache-2.0

/** Page size of every list request. */
export const PAGE_SIZE = 50;

/** Pagination options shared by every list query. */
export const pageOptions = {
  pageParamKey: "pageToken",
  getNextPageParam: (last: { nextPageToken: string }) =>
    last.nextPageToken === "" ? undefined : last.nextPageToken,
} as const;
