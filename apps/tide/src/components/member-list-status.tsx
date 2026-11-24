// SPDX-License-Identifier: Apache-2.0
import type { useMembers } from "../members.ts";
import { ErrorNotice, Loading } from "./ui.tsx";

export function MemberListStatus({
  roster,
}: {
  roster: ReturnType<typeof useMembers>;
}) {
  if (roster.isPending) return <Loading label="Loading members" />;
  if (roster.error) {
    return (
      <>
        {roster.hasNextPage ? (
          <p className="muted">Some members are not shown yet.</p>
        ) : null}
        <ErrorNotice error={roster.error} onRetry={roster.retry} />
      </>
    );
  }
  return roster.hasNextPage ? (
    <p className="muted" role="status">
      Loading more members
    </p>
  ) : null;
}
