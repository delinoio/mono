// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery } from "@connectrpc/connect-query";
import { type Member, MemberService } from "@delino/tide-protos/v1/member_pb";
import { useEffect, useMemo } from "react";
import { MAX_PAGE_SIZE } from "./api.ts";

function refetchUnlessLaterPageFailed(query: {
  state: {
    status: string;
    fetchMeta: { fetchMore?: { direction: string } } | null;
  };
}): boolean {
  // Keep a failed continuation available for manual retry when the user
  // switches views, focuses the window or reconnects.
  return !(
    query.state.status === "error" &&
    query.state.fetchMeta?.fetchMore?.direction === "forward"
  );
}

/** The shared roster for one organization. */
export function useMembers(organizationId: string) {
  const query = useInfiniteQuery(
    MemberService.method.listMembers,
    { organizationId, pageSize: MAX_PAGE_SIZE, pageToken: "" },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
      refetchOnMount: refetchUnlessLaterPageFailed,
      refetchOnWindowFocus: refetchUnlessLaterPageFailed,
      refetchOnReconnect: refetchUnlessLaterPageFailed,
    },
  );
  const { data, fetchNextPage, hasNextPage, isError, isFetching } = query;

  // Include data because a fast page can arrive before React renders the
  // fetching state.
  useEffect(() => {
    if (hasNextPage && !isFetching && !isError) {
      // Observers of the same roster can join the request already in flight.
      void fetchNextPage({ cancelRefetch: false });
    }
  }, [data, fetchNextPage, hasNextPage, isError, isFetching]);

  const members = useMemo(() => {
    const byUserId = new Map<string, Member>();
    for (const page of data?.pages ?? []) {
      for (const member of page.members) byUserId.set(member.userId, member);
    }
    return [...byUserId.values()];
  }, [data]);

  return {
    members,
    hasData: data !== undefined,
    query,
  };
}

export function displayName(member: Member): string {
  return member.displayName || member.userId;
}
