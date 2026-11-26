// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery } from "@connectrpc/connect-query";
import { AuditService } from "@delino/sandrun-protos/v1/audit_pb";
import { useMemo } from "react";
import { PAGE_SIZE } from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  Empty,
  ListPagination,
  PageHeader,
  QueryBoundary,
  ShortId,
} from "../components/ui.tsx";
import { formatTime, labels, memberName } from "../format.ts";
import { useMembers } from "../members.ts";

/** Route `audit`: audit events of the organization, newest first. */
export function AuditPage() {
  const { organizationId } = useOrg();
  const { members } = useMembers(organizationId);
  const names = useMemo(
    () => new Map(members.map((member) => [member.userId, member.displayName])),
    [members],
  );
  const list = useInfiniteQuery(
    AuditService.method.listAuditEvents,
    { organizationId, pageSize: PAGE_SIZE, pageToken: "" },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );

  return (
    <>
      <PageHeader
        title="Audit events"
        description="Changes in this organization, newest first. Audit events never contain credentials, commands, output or file contents."
      />
      <QueryBoundary query={list} loading="Loading audit events">
        {(data) => {
          const events = data.pages.flatMap((p) => p.events);
          return events.length === 0 ? (
            <Empty title="No audit events" />
          ) : (
            <div
              className="table-wrap"
              role="region"
              aria-label="Audit events table"
              tabIndex={0}
            >
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
          );
        }}
      </QueryBoundary>
      <ListPagination query={list} />
    </>
  );
}
