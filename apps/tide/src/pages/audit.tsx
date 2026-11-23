// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery, useQuery } from "@connectrpc/connect-query";
import { Button, ButtonVariant } from "delino-ui/button";
import { AuditService } from "@delino/tide-protos/v1/audit_pb";
import { MemberService } from "@delino/tide-protos/v1/member_pb";
import { useMemo } from "react";
import { MAX_PAGE_SIZE, PAGE_SIZE } from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  Empty,
  ErrorNotice,
  Loading,
  PageHeader,
  ShortId,
} from "../components/ui.tsx";
import { formatTime, labels, memberName } from "../format.ts";

/** Route `audit`: audit events of the organization, newest first. */
export function AuditPage() {
  const { organizationId } = useOrg();
  const members = useQuery(MemberService.method.listMembers, {
    organizationId,
    pageSize: MAX_PAGE_SIZE,
  });
  const names = useMemo(
    () =>
      new Map(
        (members.data?.members ?? []).map((m) => [m.userId, m.displayName]),
      ),
    [members.data],
  );
  const list = useInfiniteQuery(
    AuditService.method.listAuditEvents,
    { organizationId, pageSize: PAGE_SIZE, pageToken: "" },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const events = list.data?.pages.flatMap((p) => p.events) ?? [];

  return (
    <>
      <PageHeader
        title="Audit events"
        description="Changes in this organization, newest first. Audit events never contain credentials, commands, output or file contents."
      />
      {list.isPending ? (
        <Loading label="Loading audit events" />
      ) : list.error ? (
        <ErrorNotice error={list.error} onRetry={() => void list.refetch()} />
      ) : events.length === 0 ? (
        <Empty title="No audit events" />
      ) : (
        <div className="table-wrap">
          <table className="table" aria-label="Audit events">
            <thead>
              <tr>
                <th scope="col">Time</th>
                <th scope="col">Action</th>
                <th scope="col">Actor</th>
                <th scope="col">Target</th>
                <th scope="col">Subject</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e) => (
                <tr key={e.auditEventId}>
                  <td>{formatTime(e.createTime)}</td>
                  <td>{labels.auditAction[e.action]}</td>
                  <td>
                    {memberName(names, e.actorUserId)}
                    {e.actorApiKeyId !== "" ? (
                      <span className="muted">
                        {" "}
                        with API key <ShortId id={e.actorApiKeyId} />
                      </span>
                    ) : null}
                  </td>
                  <td>
                    {labels.auditTargetKind[e.targetKind]}{" "}
                    {e.targetId !== "" ? <ShortId id={e.targetId} /> : null}
                  </td>
                  <td>
                    {e.subjectUserId === ""
                      ? "—"
                      : memberName(names, e.subjectUserId)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {list.hasNextPage ? (
        <Button
          variant={ButtonVariant.Secondary}
          onClick={() => void list.fetchNextPage()}
        >
          Show more
        </Button>
      ) : null}
    </>
  );
}
