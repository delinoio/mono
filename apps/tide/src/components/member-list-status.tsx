// SPDX-License-Identifier: Apache-2.0
import type { ReactNode } from "react";
import type { useMembers } from "../members.ts";
import { ErrorNotice, QueryBoundary } from "./ui.tsx";

export function MemberListStatus({
  roster: { query },
  children,
}: {
  roster: ReturnType<typeof useMembers>;
  children?: ReactNode;
}) {
  return (
    <QueryBoundary query={query} loading="Loading members">
      {() => (
        <>
          {query.hasNextPage ? (
            query.error && !query.isFetching ? (
              <p className="muted">Some members are not shown yet.</p>
            ) : (
              <p className="muted" role="status">
                Loading more members
              </p>
            )
          ) : null}
          {query.isFetchNextPageError && !query.isFetching ? (
            <ErrorNotice
              error={query.error}
              onRetry={() => void query.fetchNextPage({ cancelRefetch: false })}
            />
          ) : null}
          {children}
        </>
      )}
    </QueryBoundary>
  );
}
